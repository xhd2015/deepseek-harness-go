package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xhd2015/deepseek-harness-go/llm"
)

const (
	toolOutputLimit = 256 * 1024
	globResultLimit = 100
	grepMatchLimit  = 250
	readLineLimit   = 2000
)

// ToolExecutor serves the read-only tools this host exposes. A zero root
// disables every tool.
type ToolExecutor struct {
	root     string
	enabled  bool
	initErr  error
	patterns map[string]*regexp.Regexp
}

// NewToolExecutor canonicalizes root and reports whether tools are available.
func NewToolExecutor(root string) *ToolExecutor {
	executor := &ToolExecutor{patterns: map[string]*regexp.Regexp{}}
	if strings.TrimSpace(root) == "" {
		return executor
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		executor.initErr = err
		return executor
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		executor.initErr = err
		return executor
	}
	info, err := os.Stat(resolved)
	if err != nil {
		executor.initErr = err
		return executor
	}
	if !info.IsDir() {
		executor.initErr = fmt.Errorf("%s is not a directory", resolved)
		return executor
	}
	executor.root = resolved
	executor.enabled = true
	return executor
}

// Enabled reports whether a usable root was configured.
func (e *ToolExecutor) Enabled() bool { return e != nil && e.enabled }

// InitError reports why a configured root was rejected.
func (e *ToolExecutor) InitError() error {
	if e == nil {
		return nil
	}
	return e.initErr
}

// Definitions returns the model-facing tool definitions.
func (e *ToolExecutor) Definitions() []llm.ToolDef {
	if !e.Enabled() {
		return nil
	}
	return []llm.ToolDef{
		{
			Name:        "read",
			Description: "Read a UTF-8 text file and return line-numbered content.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string", "description": "Path to read, resolved against the tool root."},
					"offset":    map[string]any{"type": "number", "description": "1-based first line to return. Defaults to 1."},
					"limit":     map[string]any{"type": "number", "description": "Maximum number of lines to return. Defaults to 2000."},
				},
				"required": []string{"file_path"},
			},
		},
		{
			Name:        "glob",
			Description: "Find files, not directories, whose paths match a glob pattern. Returns up to 100 paths in modification-time order.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Glob pattern such as **/*.go or *.md. A pattern without / matches the basename at any depth."},
					"path":    map[string]any{"type": "string", "description": "Directory to search in. Defaults to the tool root."},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "grep",
			Description: "Search file contents with a regular expression. Returns matching lines with line numbers, grouped by file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Regular expression to search for."},
					"path":    map[string]any{"type": "string", "description": "File or directory to search. Defaults to the tool root."},
					"include": map[string]any{"type": "string", "description": "One glob filter for which files to search, such as *.go."},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// Execute runs one tool call and returns model-facing text plus whether the
// invocation failed.
func (e *ToolExecutor) Execute(ctx context.Context, name, arguments string) (string, bool) {
	if !e.Enabled() {
		return "tools are not enabled", true
	}
	var args map[string]any
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "invalid tool arguments: " + err.Error(), true
		}
	}
	switch name {
	case "read":
		return e.read(ctx, args)
	case "glob":
		return e.glob(ctx, args)
	case "grep":
		return e.grep(ctx, args)
	default:
		return "unknown tool: " + name, true
	}
}

func (e *ToolExecutor) read(ctx context.Context, args map[string]any) (string, bool) {
	filePath, ok := stringArg(args, "file_path")
	if !ok || strings.TrimSpace(filePath) == "" {
		return "file_path must be a non-empty string", true
	}
	offset := intArg(args, "offset", 1)
	limit := intArg(args, "limit", readLineLimit)
	if offset < 1 {
		offset = 1
	}
	if limit < 1 {
		limit = readLineLimit
	}
	if limit > readLineLimit {
		return fmt.Sprintf("limit must be less than or equal to %d", readLineLimit), true
	}
	resolved, err := e.resolve(filePath, false)
	if err != nil {
		return err.Error(), true
	}
	file, err := os.Open(resolved)
	if err != nil {
		return err.Error(), true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err.Error(), true
	}
	if info.IsDir() {
		return "path is a directory: " + resolved, true
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	total := 0
	var body strings.Builder
	last := offset - 1
	for scanner.Scan() {
		line++
		total = line
		if line < offset {
			continue
		}
		if line >= offset+limit {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err.Error(), true
		}
		text := scanner.Text()
		fmt.Fprintf(&body, "%d: %s\n", line, text)
		last = line
		if body.Len() > toolOutputLimit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err.Error(), true
	}
	display := resolved
	if rel, err := filepath.Rel(e.root, resolved); err == nil && !strings.HasPrefix(rel, "..") {
		display = filepath.ToSlash(rel)
	}
	footer := fmt.Sprintf("(End of file - total %d lines)", total)
	if last < total {
		footer = fmt.Sprintf("(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, last, total, last+1)
	}
	content := strings.TrimSuffix(body.String(), "\n")
	if content == "" {
		content = footer
	} else {
		content += "\n\n" + footer
	}
	return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s\n</content>", display, content), false
}

func (e *ToolExecutor) glob(ctx context.Context, args map[string]any) (string, bool) {
	pattern, ok := stringArg(args, "pattern")
	if !ok || strings.TrimSpace(pattern) == "" {
		return "pattern must be a non-empty string", true
	}
	pattern = filepath.ToSlash(pattern)
	if strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
		return "pattern must stay inside the tool root", true
	}
	base := e.root
	if dir, ok := stringArg(args, "path"); ok && strings.TrimSpace(dir) != "" {
		resolved, err := e.resolve(dir, true)
		if err != nil {
			return err.Error(), true
		}
		base = resolved
	}
	info, err := os.Stat(base)
	if err != nil {
		return err.Error(), true
	}
	if !info.IsDir() {
		return "path is not a directory: " + base, true
	}
	basenameOnly := !strings.Contains(pattern, "/")
	type found struct {
		path    string
		modTime time.Time
	}
	files := make([]found, 0, 64)
	visited := 0
	deadline := time.Now().Add(10 * time.Second)
	errStop := fmt.Errorf("stop")
	err = filepath.WalkDir(base, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errStop
		}
		if entry.IsDir() {
			return nil
		}
		visited++
		if visited > 100000 {
			return errStop
		}
		rel, err := filepath.Rel(e.root, candidate)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		rel = filepath.ToSlash(rel)
		target := rel
		if basenameOnly {
			target = path.Base(rel)
		}
		if !globMatch(pattern, target) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, found{path: rel, modTime: info.ModTime()})
		return nil
	})
	if err != nil && err != errStop && ctx.Err() == nil {
		return err.Error(), true
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.After(files[j].modTime)
	})
	if len(files) == 0 {
		return "No files found", false
	}
	truncated := len(files) > globResultLimit
	if truncated {
		files = files[:globResultLimit]
	}
	lines := make([]string, len(files))
	for i, file := range files {
		lines[i] = file.path
	}
	output := strings.Join(lines, "\n")
	if truncated {
		output = fmt.Sprintf("%s\n\n(Showing first %d files. Narrow the pattern to see more.)", output, globResultLimit)
	}
	return trimOutput(output), false
}

func (e *ToolExecutor) grep(ctx context.Context, args map[string]any) (string, bool) {
	pattern, ok := stringArg(args, "pattern")
	if !ok || pattern == "" {
		return "pattern must be a non-empty string", true
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return "invalid regular expression: " + err.Error(), true
	}
	base := e.root
	if target, ok := stringArg(args, "path"); ok && strings.TrimSpace(target) != "" {
		resolved, err := e.resolve(target, true)
		if err != nil {
			return err.Error(), true
		}
		base = resolved
	}
	include, _ := stringArg(args, "include")
	type match struct {
		path string
		line int
		text string
	}
	matches := []match{}
	addFile := func(filePath string) error {
		if len(matches) >= grepMatchLimit {
			return fmt.Errorf("stop")
		}
		info, err := os.Stat(filePath)
		if err != nil || info.IsDir() || info.Size() > 8*1024*1024 {
			return nil
		}
		if include != "" {
			rel, _ := filepath.Rel(e.root, filePath)
			if !globMatch(include, filepath.ToSlash(rel)) && !globMatch(include, filepath.Base(filePath)) {
				return nil
			}
		}
		file, err := os.Open(filePath)
		if err != nil {
			return nil
		}
		defer file.Close()
		head := make([]byte, 8192)
		n, _ := file.Read(head)
		if bytes.IndexByte(head[:n], 0) >= 0 {
			return nil
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		line := 0
		rel, _ := filepath.Rel(e.root, filePath)
		for scanner.Scan() {
			line++
			if compiled.MatchString(scanner.Text()) {
				text := scanner.Text()
				if len(text) > 400 {
					text = text[:400] + "…"
				}
				matches = append(matches, match{path: filepath.ToSlash(rel), line: line, text: text})
				if len(matches) >= grepMatchLimit {
					return fmt.Errorf("stop")
				}
			}
		}
		return nil
	}
	info, err := os.Stat(base)
	if err != nil {
		return err.Error(), true
	}
	if info.IsDir() {
		_ = filepath.WalkDir(base, func(candidate string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			return addFile(candidate)
		})
	} else {
		_ = addFile(base)
	}
	if err := ctx.Err(); err != nil {
		return err.Error(), true
	}
	if len(matches) == 0 {
		return "No matches found", false
	}
	byFile := map[string][]match{}
	order := []string{}
	for _, item := range matches {
		if _, seen := byFile[item.path]; !seen {
			order = append(order, item.path)
		}
		byFile[item.path] = append(byFile[item.path], item)
	}
	var output strings.Builder
	for i, filePath := range order {
		if i > 0 {
			output.WriteString("\n\n")
		}
		output.WriteString(filePath)
		for _, item := range byFile[filePath] {
			fmt.Fprintf(&output, "\nLine %d: %s", item.line, item.text)
		}
	}
	text := trimOutput(output.String())
	if len(matches) >= grepMatchLimit {
		text += fmt.Sprintf("\n\n(Showing first %d matches. Narrow the pattern to see more.)", grepMatchLimit)
	}
	return text, false
}

func (e *ToolExecutor) resolve(raw string, allowDir bool) (string, error) {
	candidate := raw
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(e.root, candidate)
	}
	candidate = filepath.Clean(candidate)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(candidate))
		if parentErr != nil {
			return "", fmt.Errorf("path is outside the tool root")
		}
		if !withinRoot(e.root, parent) {
			return "", fmt.Errorf("path is outside the tool root")
		}
		resolved = filepath.Join(parent, filepath.Base(candidate))
	}
	if !withinRoot(e.root, resolved) {
		return "", fmt.Errorf("path is outside the tool root")
	}
	if !allowDir {
		info, err := os.Stat(resolved)
		if err == nil && info.IsDir() {
			return "", fmt.Errorf("path is a directory: %s", resolved)
		}
	}
	return resolved, nil
}

func withinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func globMatch(pattern, target string) bool {
	pattern = filepath.ToSlash(pattern)
	target = filepath.ToSlash(target)
	return matchSegments(strings.Split(pattern, "/"), strings.Split(target, "/"))
}

func matchSegments(pattern, target []string) bool {
	if len(pattern) == 0 {
		return len(target) == 0
	}
	if pattern[0] == "**" {
		if matchSegments(pattern[1:], target) {
			return true
		}
		return len(target) > 0 && matchSegments(pattern, target[1:])
	}
	if len(target) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], target[0])
	if err != nil || !matched {
		return false
	}
	return matchSegments(pattern[1:], target[1:])
}

func stringArg(args map[string]any, name string) (string, bool) {
	value, ok := args[name]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func intArg(args map[string]any, name string, fallback int) int {
	value, ok := args[name]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func trimOutput(text string) string {
	if len(text) <= toolOutputLimit {
		return text
	}
	cut := text[:toolOutputLimit]
	if index := strings.LastIndex(cut, "\n"); index > 0 {
		cut = cut[:index]
	}
	return cut + "\n\n(Output capped.)"
}
