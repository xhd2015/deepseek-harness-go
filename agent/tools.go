package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xhd2015/deepseek-harness-go/llm"
)

const (
	toolOutputLimit = 256 * 1024

	readLineLimit      = 2000
	readMaxLineLength  = 2000
	readMaxBytes       = 50 * 1024
	globResultLimit    = 100
	grepMatchLimit     = 250
	grepMaxLineBytes   = 2000
	bashMaxOutputBytes = 64 * 1000
	bashDefaultTimeout = 120 * time.Second
	bashMaxTimeout     = 600 * time.Second
)

// ToolInvocation is one tool call in its session context. Cwd is the session
// workspace; SessionDir is where oversized search results are spilled.
type ToolInvocation struct {
	SessionID  string
	SessionDir string
	Cwd        string
	Name       string
	Arguments  string
}

// ToolExecutor hosts the core tools this backend exposes. Every call resolves
// relative paths against its invocation's workspace, so one executor serves
// all sessions.
type ToolExecutor struct {
	root       string
	permission string
	sandbox    string
	dshHome    string
	enabled    bool
	initErr    error
	observed   observationStore
}

type observationStore struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (o *observationStore) mark(sessionID, path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[sessionID+"\x00"+path] = true
}

func (o *observationStore) has(sessionID, path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen[sessionID+"\x00"+path]
}

// NewToolExecutor canonicalizes the configured workspace. An empty root uses
// the process working directory, matching the DSH default workspace.
func NewToolExecutor(root string) *ToolExecutor {
	executor := &ToolExecutor{permission: "danger-full-access", sandbox: "danger-full-access"}
	if strings.TrimSpace(root) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			executor.initErr = err
			return executor
		}
		root = cwd
	}
	resolved, err := canonicalDir(root)
	if err != nil {
		executor.initErr = err
		return executor
	}
	executor.root = resolved
	executor.enabled = true
	return executor
}

// SetDSHHome records the harness home exported to shell commands as
// DSH_HOME. Sessions store this value at startup so every child observes the
// same harness identity.
func (e *ToolExecutor) SetDSHHome(home string) {
	if e == nil {
		return
	}
	e.dshHome = home
}

// SetPolicy records the permission/sandbox modes used for mutation checks.
func (e *ToolExecutor) SetPolicy(permission, sandbox string) {
	if e == nil {
		return
	}
	if permission != "" {
		e.permission = permission
	}
	if sandbox != "" {
		e.sandbox = sandbox
	}
}

// Enabled reports whether the executor has a usable workspace root.
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
			Name: "bash",
			Description: "Execute a bash command (`bash -c`) and return its stdout/stderr. " +
				"Each call runs in a fresh shell; pass `workdir` instead of using `cd`. " +
				"Provide `description` before `command` in the arguments. " +
				"Credential-shaped variables and ambient `DSH_*` variables are scrubbed; managed `DSH_*` variables expose current harness environment facts. " +
				"Before any delete or move, verify that the resolved absolute target path is the intended one; never run it against a computed path you have not checked. " +
				"An unset variable expands to an empty string, so guard variables in such paths with `${VAR:?}`.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"description": map[string]any{
						"type":        "string",
						"description": "Clear, concise description of what this command does in active voice, 5-10 words (shown in the UI). Examples: \"ls\" → \"List files in current directory\"; \"git status\" → \"Show working tree status\"; \"npm install\" → \"Install package dependencies\".",
					},
					"command": map[string]any{
						"type":        "string",
						"description": "The bash command to execute.",
					},
					"timeoutMs": map[string]any{
						"type":        "number",
						"description": "Timeout in milliseconds. The executor applies its configured default and cap, and kills the command on expiry.",
					},
					"workdir": map[string]any{
						"type":        "string",
						"description": "Working directory for this command. Defaults to the session workspace; a relative path is resolved against it.",
					},
				},
				"required": []string{"description", "command"},
			},
		},
		{
			Name:        "read",
			Description: "Read a UTF-8 text file and return line-numbered content.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string", "description": "Path to read, resolved by the filesystem backend."},
					"offset":    map[string]any{"type": "number", "description": "1-based first line to return. Defaults to 1."},
					"limit":     map[string]any{"type": "number", "description": "Maximum number of lines to return. Defaults to 2000."},
				},
				"required": []string{"file_path"},
			},
		},
		{
			Name:        "write",
			Description: "Create or fully replace a UTF-8 text file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string", "description": "Path to write, resolved by the filesystem backend. Provide `file_path` before `content` in the arguments."},
					"content":   map[string]any{"type": "string", "description": "Full UTF-8 text content to write."},
				},
				"required": []string{"file_path", "content"},
			},
		},
		{
			Name:        "edit",
			Description: "Edit an existing UTF-8 text file by replacing literal text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path":   map[string]any{"type": "string", "description": "Path to edit, resolved by the filesystem backend. Provide `file_path` before `old_string` and `new_string` in the arguments."},
					"old_string":  map[string]any{"type": "string", "description": "Literal text to replace."},
					"new_string":  map[string]any{"type": "string", "description": "Literal replacement text. Use an empty string to delete the match."},
					"replace_all": map[string]any{"type": "boolean", "description": "Replace all matches. Defaults to false; when false, old_string must appear exactly once."},
				},
				"required": []string{"file_path", "old_string", "new_string"},
			},
		},
		{
			Name:        "glob",
			Description: "Find files, not directories, whose paths match a glob pattern, including hidden and ignored files. Returns up to 100 paths in modification-time order; a larger result is sampled across top-level entries and reports where the complete list was saved.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Glob pattern to match file paths against (e.g. \"**/*.ts\", \"src/**/*.test.js\"). A pattern with no \"/\" matches the basename at any depth, so \"*\" and \"*.ts\" both search the whole tree; include a separator to anchor the depth."},
					"path":    map[string]any{"type": "string", "description": "Directory to search in. Defaults to the session workspace; a relative path resolves against it."},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "grep",
			Description: "Search file contents with a ripgrep regular expression. Returns matching lines with line numbers, grouped by file. Returns up to 250 matches; a larger result reports where the complete match list was saved.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Regular expression to search for (ripgrep syntax)."},
					"path":    map[string]any{"type": "string", "description": "File or directory to search. Defaults to the session workspace; a relative path resolves against it."},
					"include": map[string]any{"type": "string", "description": "One glob filter for which files to search (e.g. \"*.ts\", \"*.{js,jsx}\"). Not a list; negation is not supported."},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// Execute runs one tool call and returns model-facing text plus whether the
// invocation failed.
func (e *ToolExecutor) Execute(ctx context.Context, inv ToolInvocation) (string, bool) {
	if !e.Enabled() {
		return "tools are not enabled", true
	}
	var args map[string]any
	if strings.TrimSpace(inv.Arguments) != "" {
		if err := json.Unmarshal([]byte(inv.Arguments), &args); err != nil {
			return "invalid tool arguments: " + err.Error(), true
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	switch inv.Name {
	case "bash":
		return e.bash(ctx, inv, args)
	case "read":
		return e.read(ctx, inv, args)
	case "write":
		return e.write(ctx, inv, args)
	case "edit":
		return e.edit(ctx, inv, args)
	case "glob":
		return e.glob(ctx, inv, args)
	case "grep":
		return e.grep(ctx, inv, args)
	default:
		return "unknown tool: " + inv.Name, true
	}
}

// baseDir resolves the workspace all relative paths in this call use.
func (e *ToolExecutor) baseDir(inv ToolInvocation) (string, error) {
	raw := strings.TrimSpace(inv.Cwd)
	if raw == "" {
		raw = e.root
	}
	if raw == "" {
		return "", fmt.Errorf("session workspace is not available")
	}
	resolved, err := canonicalDir(raw)
	if err != nil {
		return "", fmt.Errorf("session workspace is not available: %v", err)
	}
	return resolved, nil
}

func canonicalDir(raw string) (string, error) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

// resolvePath turns a model-supplied path into a canonical absolute path and
// the lexical display path used in model-facing error messages. DSH reports
// the path as the caller spelled it (made absolute), not as the symlink target.
func (e *ToolExecutor) resolvePath(inv ToolInvocation, raw string, allowMissing bool) (string, string, error) {
	base, err := e.baseDir(inv)
	if err != nil {
		return "", filepath.ToSlash(raw), err
	}
	display := lexicalDisplayPath(base, raw)
	candidate := display
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if !allowMissing || !os.IsNotExist(err) {
			return "", display, err
		}
		resolved, err = resolveMissingPath(candidate)
		if err != nil {
			return "", display, err
		}
	}
	return resolved, display, nil
}

// lexicalDisplayPath anchors a caller path without resolving symlinks, matching
// DSH's target.displayPath spelling.
func lexicalDisplayPath(base, raw string) string {
	candidate := raw
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(base, candidate)
	}
	return filepath.ToSlash(filepath.Clean(candidate))
}

// pathErrorText reduces an OS path error to the stable wording DSH exposes.
func pathErrorText(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	default:
		return err.Error()
	}
}

// resolveMissingPath canonicalizes the existing ancestor of a path whose tail
// may not exist yet, so write can create missing parent directories without
// resolving symlinks in reverse.
func resolveMissingPath(candidate string) (string, error) {
	var tail []string
	current := candidate
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve parent of %s", candidate)
		}
		tail = append(tail, filepath.Base(current))
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for index := len(tail) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, tail[index])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		current = parent
	}
}

func displayPath(absolute, base string) string {
	rel, err := filepath.Rel(base, absolute)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(absolute)
}

func withinBase(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// allowMutation applies the configured file policy. Shell commands are not
// confined by this check; only the filesystem tools are.
func (e *ToolExecutor) allowMutation(inv ToolInvocation, target, display string) error {
	if e.permission == "read-only" || e.sandbox == "read-only" {
		return fmt.Errorf("cannot modify %q: tools are read-only", display)
	}
	if e.sandbox == "workspace-write" {
		base, err := e.baseDir(inv)
		if err != nil {
			return err
		}
		if !withinBase(base, target) {
			return fmt.Errorf("cannot modify %q: path is outside the session workspace", display)
		}
	}
	return nil
}

func (e *ToolExecutor) requireObserved(inv ToolInvocation, target, display string) error {
	if e.observed.has(inv.SessionID, target) {
		return nil
	}
	return fmt.Errorf("cannot modify %q: file has not been read — read the file, then retry", display)
}

func (e *ToolExecutor) markObserved(inv ToolInvocation, target string) {
	e.observed.mark(inv.SessionID, target)
}

func (e *ToolExecutor) spill(inv ToolInvocation, name, content string) (string, bool) {
	dir := strings.TrimSpace(inv.SessionDir)
	if dir == "" {
		return "", false
	}
	spillDir := filepath.Join(dir, "spill")
	if err := os.MkdirAll(spillDir, 0o755); err != nil {
		return "", false
	}
	target := filepath.Join(spillDir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "", false
	}
	return target, true
}

func stringArg(args map[string]any, name string) (string, bool) {
	value, ok := args[name]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func boolArg(args map[string]any, name string) bool {
	value, ok := args[name]
	if !ok {
		return false
	}
	flag, _ := value.(bool)
	return flag
}

func numberArg(args map[string]any, name string) (float64, bool) {
	value, ok := args[name]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func positiveIntArg(args map[string]any, name string, fallback int) (int, error) {
	value, ok := numberArg(args, name)
	if !ok {
		if _, present := args[name]; present {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return fallback, nil
	}
	if value < 1 || value != float64(int(value)) {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return int(value), nil
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
