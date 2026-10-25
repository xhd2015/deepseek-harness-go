package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const searchSpillHint = "Use read with offset/limit, or grep this path to search within it."

type globEntry struct {
	absolute string
	display  string
	modified int64
}

func (e *ToolExecutor) glob(ctx context.Context, inv ToolInvocation, args map[string]any) (string, bool) {
	pattern, ok := stringArg(args, "pattern")
	if !ok || strings.TrimSpace(pattern) == "" {
		return "pattern must be a non-empty string", true
	}
	rawPath := ""
	hasPath := false
	if raw, present := args["path"]; present {
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "path must be a non-empty string when given", true
		}
		rawPath = text
		hasPath = true
	}
	base, err := e.baseDir(inv)
	if err != nil {
		return err.Error(), true
	}
	root := base
	displayRoot := ""
	sampleRoot := "."
	if hasPath {
		root, _, err = e.resolvePath(inv, rawPath, false)
		if err != nil {
			return fmt.Sprintf("cannot search %q: %v", rawPath, err), true
		}
		displayRoot = globDisplayRoot(rawPath)
		if filepath.IsAbs(rawPath) {
			// DSH relativizes absolute output for display but still samples
			// against the absolute search root, so an absolute subtree forms
			// one top-level group rather than one group per file.
			sampleRoot = filepath.ToSlash(filepath.Clean(rawPath))
		} else {
			sampleRoot = filepath.ToSlash(rawPath)
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Sprintf("cannot search %q: %v", rawPathOrBase(rawPath, hasPath), err), true
	}

	var entries []globEntry
	add := func(absolute, display, match string, modified int64) {
		if hasVCSComponent(display) || !matchesGlob(pattern, match) {
			return
		}
		entries = append(entries, globEntry{absolute: absolute, display: display, modified: modified})
	}
	if !info.IsDir() {
		// ripgrep accepts an explicit file regardless of --glob filters and VCS
		// excludes; only a directory search applies them.
		display := globPath(displayRoot, "", base)
		entries = append(entries, globEntry{
			absolute: filepath.Clean(root),
			display:  display,
			modified: info.ModTime().UnixNano(),
		})
	} else {
		walkRoot := root
		if linfo, lerr := os.Lstat(root); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(root)
			if rerr != nil {
				return fmt.Sprintf("cannot search %q: %v", rawPath, rerr), true
			}
			walkRoot = resolved
		}
		walkErr := filepath.WalkDir(walkRoot, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				if current != walkRoot && isVCSDir(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			// ripgrep does not follow symlinks found during directory traversal.
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := entry.Info()
			if err != nil || info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(walkRoot, current)
			if err != nil {
				return nil
			}
			match := globRawPath(displayRoot, rel)
			display := match
			if hasPath && filepath.IsAbs(displayRoot) {
				display = displayPath(match, base)
			} else {
				match = strings.TrimPrefix(match, "./")
			}
			add(filepath.Clean(current), display, match, info.ModTime().UnixNano())
			return nil
		})
		if walkErr != nil {
			return walkErr.Error(), true
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modified != entries[j].modified {
			return entries[i].modified < entries[j].modified
		}
		return entries[i].display < entries[j].display
	})
	if len(entries) == 0 {
		return "No files found", false
	}
	all := make([]string, len(entries))
	for index, entry := range entries {
		all[index] = entry.display
	}
	shown := all
	basis := "."
	if len(all) > globResultLimit {
		sample := sampleAcrossTopLevel(all, globResultLimit, sampleRoot)
		shown = sample.items
		if sample.total != len(all) {
			basis = fmt.Sprintf(", sampled across %d of the %d top-level entries this pattern matched instead of taken in modification-time order.", sample.shown, sample.total)
			if sample.shown < sample.total {
				basis += " Narrow path to inspect a specific subtree."
			}
		}
	}
	if len(all) <= globResultLimit {
		return strings.Join(shown, "\n"), false
	}
	locator, saved := e.spill(inv, "glob-results.txt", strings.Join(all, "\n"))
	recovery := "The complete result could not be saved; narrow pattern or path to see more."
	if saved {
		recovery = fmt.Sprintf("Full sorted result stored at: %s. %s", locator, searchSpillHint)
	}
	return fmt.Sprintf("%s\n\n(Showing %d of %d paths%s %s)", strings.Join(shown, "\n"), len(shown), len(all), basis, recovery), false
}

func rawPathOrBase(raw string, present bool) string {
	if present {
		return raw
	}
	return "."
}

// globDisplayRoot renders the explicit search-root argument the way ripgrep
// prefixes it in --files output. An omitted root has no prefix, "." and "./x"
// keep their ./ prefix, and absolute roots stay absolute.
func globDisplayRoot(raw string) string {
	if filepath.IsAbs(raw) {
		return filepath.ToSlash(filepath.Clean(raw))
	}
	cleaned := filepath.ToSlash(filepath.Clean(raw))
	if cleaned == "." {
		return "."
	}
	if raw == "." || strings.HasPrefix(raw, "."+string(filepath.Separator)) || strings.HasPrefix(raw, "./") {
		return "./" + cleaned
	}
	return cleaned
}

// globPath joins a display root and a root-relative file path. Absolute output
// under the session workspace is relativized, matching DSH's display rule.
func globPath(displayRoot, rel, base string) string {
	raw := globRawPath(displayRoot, rel)
	if filepath.IsAbs(displayRoot) {
		return displayPath(raw, base)
	}
	return raw
}

// globRawPath is the path ripgrep itself emits: an absolute target stays
// absolute even when DSH later relativizes it for display.
func globRawPath(displayRoot, rel string) string {
	if filepath.IsAbs(displayRoot) {
		absolute := displayRoot
		if rel != "" && rel != "." {
			absolute = filepath.Join(absolute, filepath.FromSlash(rel))
		}
		return filepath.ToSlash(absolute)
	}
	rel = filepath.ToSlash(rel)
	switch displayRoot {
	case "":
		return rel
	case ".":
		return "./" + rel
	default:
		if rel == "" || rel == "." {
			return displayRoot
		}
		return displayRoot + "/" + rel
	}
}

// sampleAcrossTopLevel chooses an over-cap page by giving every top-level
// entry a slot before any entry gets a second, then returns the page grouped
// by entry in discovery order. It mirrors DSH's sampleAcrossTopLevel.
func sampleAcrossTopLevel(paths []string, maxItems int, root string) globSample {
	type group struct {
		key   string
		items []string
		next  int
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, item := range paths {
		key := topLevelSegment(relativeToSearchRoot(item, root))
		if existing := byKey[key]; existing != nil {
			existing.items = append(existing.items, item)
			continue
		}
		created := &group{key: key, items: []string{item}}
		byKey[key] = created
		groups = append(groups, created)
	}
	active := make([]*group, len(groups))
	copy(active, groups)
	taken := map[string][]string{}
	count := 0
	for len(active) > 0 && count < maxItems {
		var next []*group
		for _, current := range active {
			if count >= maxItems {
				break
			}
			taken[current.key] = append(taken[current.key], current.items[current.next])
			current.next++
			count++
			if current.next < len(current.items) {
				next = append(next, current)
			}
		}
		active = next
	}
	items := make([]string, 0, count)
	for _, current := range groups {
		items = append(items, taken[current.key]...)
	}
	return globSample{items: items, shown: len(taken), total: len(groups)}
}

type globSample struct {
	items []string
	shown int
	total int
}

func relativeToSearchRoot(pathValue, root string) string {
	pathValue = filepath.ToSlash(pathValue)
	root = filepath.ToSlash(root)
	if root == "." {
		return strings.TrimPrefix(pathValue, "./")
	}
	trimmedRoot := strings.TrimRight(root, "/")
	if trimmedRoot == "" {
		return strings.TrimLeft(pathValue, "/")
	}
	if pathValue == trimmedRoot {
		return ""
	}
	if strings.HasPrefix(pathValue, trimmedRoot+"/") {
		return pathValue[len(trimmedRoot)+1:]
	}
	return pathValue
}

func topLevelSegment(pathValue string) string {
	trimmed := strings.TrimLeft(pathValue, "/")
	if cut := strings.Index(trimmed, "/"); cut >= 0 {
		return trimmed[:cut]
	}
	return trimmed
}

func hasVCSComponent(display string) bool {
	for _, part := range strings.Split(filepath.ToSlash(display), "/") {
		if isVCSDir(part) {
			return true
		}
	}
	return false
}

func isVCSDir(name string) bool {
	switch name {
	case ".git", ".svn", ".hg", ".bzr", ".jj", ".sl":
		return true
	default:
		return false
	}
}

func matchesGlob(pattern, rel string) bool {
	pattern = filepath.ToSlash(pattern)
	rel = filepath.ToSlash(rel)
	// ripgrep normalizes leading "/" and "./" pattern prefixes, while output
	// paths keep them (an explicit "." target prints "./x"). Match file paths
	// as ripgrep does: strip the target's "./" prefix but keep absolute paths
	// anchored.
	pattern = strings.TrimPrefix(pattern, "/")
	rel = strings.TrimPrefix(rel, "./")
	if pattern == "" {
		return false
	}
	for _, expanded := range expandBraces(pattern) {
		if strings.Contains(expanded, "/") {
			if matchSegments(strings.Split(expanded, "/"), strings.Split(rel, "/")) {
				return true
			}
			continue
		}
		if matchSegment(expanded, path.Base(rel)) {
			return true
		}
	}
	return false
}

func expandBraces(pattern string) []string {
	open := -1
	for index := 0; index < len(pattern); index++ {
		if pattern[index] == '{' {
			open = index
			break
		}
	}
	if open == -1 {
		return []string{pattern}
	}
	depth := 0
	closeIndex := -1
	var commas []int
	for index := open; index < len(pattern); index++ {
		switch pattern[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIndex = index
				index = len(pattern)
			}
		case ',':
			if depth == 1 {
				commas = append(commas, index)
			}
		}
	}
	if closeIndex == -1 {
		return []string{pattern}
	}
	if len(commas) == 0 {
		return expandBraces(pattern[:open] + pattern[open+1:closeIndex] + pattern[closeIndex+1:])
	}
	var results []string
	start := open + 1
	boundaries := append(append([]int{}, commas...), closeIndex)
	for _, end := range boundaries {
		alternative := pattern[start:end]
		results = append(results, expandBraces(pattern[:open]+alternative+pattern[closeIndex+1:])...)
		start = end + 1
		if len(results) > 256 {
			break
		}
	}
	return results
}

func matchSegments(patterns, parts []string) bool {
	if len(patterns) == 0 {
		return len(parts) == 0
	}
	if patterns[0] == "**" {
		for index := 0; index <= len(parts); index++ {
			if matchSegments(patterns[1:], parts[index:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	if !matchSegment(patterns[0], parts[0]) {
		return false
	}
	return matchSegments(patterns[1:], parts[1:])
}

func matchSegment(pattern, value string) bool {
	matched, err := path.Match(pattern, value)
	return err == nil && matched
}

type grepMatch struct {
	path       string
	lineNumber int
	line       string
}

func (e *ToolExecutor) grep(ctx context.Context, inv ToolInvocation, args map[string]any) (string, bool) {
	pattern, ok := stringArg(args, "pattern")
	if !ok || pattern == "" {
		return "pattern must be a non-empty string", true
	}
	for _, name := range []string{"path", "include"} {
		if raw, present := args[name]; present {
			text, ok := raw.(string)
			if !ok {
				return fmt.Sprintf("%s must be a string", name), true
			}
			if name == "path" && strings.TrimSpace(text) == "" {
				return "path must be a non-empty string when given", true
			}
			if name == "include" {
				if err := validateInclude(text); err != nil {
					return err.Error(), true
				}
			}
		}
	}
	base, err := e.baseDir(inv)
	if err != nil {
		return err.Error(), true
	}
	target := base
	if raw, present := args["path"]; present {
		raw := raw.(string)
		target, _, err = e.resolvePath(inv, raw, false)
		if err != nil {
			return fmt.Sprintf("cannot search %q: %v", raw, err), true
		}
	}
	if _, err := os.Stat(target); err != nil {
		return fmt.Sprintf("cannot search %q: %v", displayPath(target, base), err), true
	}
	include, _ := stringArg(args, "include")

	// ripgrep repeats the caller's path spelling in its output. Keep the raw
	// argument so "." stays "./path" and relative subdirectories are not
	// rewritten.
	targetArg := ""
	if raw, present := args["path"]; present {
		targetArg = raw.(string)
	}

	var matches []grepMatch
	if rg, lookErr := exec.LookPath("rg"); lookErr == nil {
		matches, err = runRipgrepGrep(ctx, rg, base, targetArg, pattern, include)
	} else {
		matches, err = searchFiles(ctx, base, target, pattern, include)
	}
	if err != nil {
		return err.Error(), true
	}
	if len(matches) == 0 {
		return "No matches found", false
	}
	for index := range matches {
		matches[index].line = previewLine(matches[index].line, grepMaxLineBytes)
	}
	total := len(matches)
	kept := matches
	truncated := false
	if total > grepMatchLimit {
		kept = matches[:grepMatchLimit]
		truncated = true
	}
	body := formatGrepMatches(kept)
	if !truncated {
		header := fmt.Sprintf("Found %d match", total)
		if total != 1 {
			header += "es"
		}
		return header + "\n\n" + body, false
	}
	var spilled []grepMatch
	spilled = append(spilled, matches...)
	content := fmt.Sprintf("Found %d matches\n\n%s", total, formatGrepMatches(spilled))
	locator, saved := e.spill(inv, "grep-results.txt", content)
	recovery := "The complete result could not be saved; narrow pattern, path, or include to see more."
	if saved {
		recovery = fmt.Sprintf("Full grep result stored at: %s. %s", locator, searchSpillHint)
	}
	return fmt.Sprintf("Found %d of %d matches\n\n%s\n\n(%s)", grepMatchLimit, total, body, recovery), false
}

func validateInclude(include string) error {
	if strings.TrimSpace(include) == "" {
		return fmt.Errorf("include must be a non-empty glob when given")
	}
	if strings.HasPrefix(include, "!") {
		return fmt.Errorf("include must be a positive glob filter; negated patterns (\"!…\") are not supported")
	}
	depth := 0
	for _, char := range include {
		switch char {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return fmt.Errorf("include must be one glob, not a comma-separated list (use {a,b} alternation instead)")
			}
		}
	}
	return nil
}

func runRipgrepGrep(ctx context.Context, binary, base, targetArg, pattern, include string) ([]grepMatch, error) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"--json", "--regexp=" + pattern}
	if include != "" {
		args = append(args, "--glob="+include)
	}
	if targetArg != "" {
		args = append(args, "--", targetArg)
	}
	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Dir = base
	cmd.Env = os.Environ()
	stdout := &limitedBuffer{limit: 20_000_000}
	stderr := &limitedBuffer{limit: 64 * 1024}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if runCtx.Err() != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("grep timed out after 30000ms")
	}
	if err != nil {
		var exitErr *exec.ExitError
		if isExitError(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("grep failed: %s", detail)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("grep failed: raw output exceeded 20000000 bytes")
	}
	matches, parseErr := parseRipgrepRecords(stdout.Bytes(), base)
	if parseErr != nil {
		return nil, parseErr
	}
	return matches, nil
}

func parseRipgrepRecords(raw []byte, base string) ([]grepMatch, error) {
	var matches []grepMatch
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text  string `json:"text"`
					Bytes string `json:"bytes"`
				} `json:"lines"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("grep failed: malformed ripgrep output: %v", err)
		}
		if record.Type != "match" {
			continue
		}
		pathText := record.Data.Path.Text
		if pathText == "" {
			return nil, fmt.Errorf("grep failed: malformed ripgrep output: match without path")
		}
		display := filepath.ToSlash(pathText)
		if filepath.IsAbs(pathText) {
			display = displayPath(pathText, base)
		}
		text := record.Data.Lines.Text
		if text == "" && record.Data.Lines.Bytes != "" {
			text = "(line is not valid UTF-8)"
		}
		text = strings.TrimSuffix(text, "\n")
		text = strings.TrimSuffix(text, "\r")
		matches = append(matches, grepMatch{path: display, lineNumber: record.Data.LineNumber, line: text})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("grep failed: malformed ripgrep output: %v", err)
	}
	return matches, nil
}

func formatGrepMatches(matches []grepMatch) string {
	var order []string
	grouped := map[string][]grepMatch{}
	for _, match := range matches {
		if _, seen := grouped[match.path]; !seen {
			order = append(order, match.path)
		}
		grouped[match.path] = append(grouped[match.path], match)
	}
	sections := make([]string, 0, len(order))
	for _, file := range order {
		rows := make([]string, 0, len(grouped[file]))
		for _, match := range grouped[file] {
			rows = append(rows, fmt.Sprintf("Line %d: %s", match.lineNumber, match.line))
		}
		sections = append(sections, file+"\n"+strings.Join(rows, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func previewLine(line string, limit int) string {
	if len(line) <= limit {
		return line
	}
	return truncateUTF8(line, limit) + " (line truncated)"
}

func searchFiles(ctx context.Context, base, target, pattern, include string) ([]grepMatch, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("grep failed: invalid pattern: %v", err)
	}
	var matches []grepMatch
	collect := func(current string, info fs.FileInfo) error {
		if info.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		display := displayPath(current, base)
		if include != "" && !matchesGlob(include, display) {
			return nil
		}
		raw, err := os.ReadFile(current)
		if err != nil {
			return nil
		}
		if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
			return nil
		}
		for index, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			if re.MatchString(line) {
				matches = append(matches, grepMatch{path: display, lineNumber: index + 1, line: line})
			}
		}
		return nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("grep failed: %v", err)
	}
	if info.IsDir() {
		err = filepath.Walk(target, func(current string, info fs.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			return collect(current, info)
		})
	} else {
		err = collect(target, info)
	}
	if err != nil {
		return nil, fmt.Errorf("grep failed: %v", err)
	}
	return matches, nil
}

type limitedBuffer struct {
	limit    int
	data     []byte
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.limit {
		remaining := b.limit - len(b.data)
		if remaining > 0 {
			b.data = append(b.data, p[:remaining]...)
		}
		b.overflow = true
		return len(p), nil
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.data }
func (b *limitedBuffer) String() string { return string(b.data) }
