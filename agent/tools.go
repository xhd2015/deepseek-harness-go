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
	"github.com/xhd2015/deepseek-harness-go/session"
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
	CallID     string
	RootCallID string
	// Emit appends a log-only side-effect event to the owning session.
	Emit func(eventType string, data any) error
	// Notify schedules a completion notice for the owning session.
	Notify func(text string)
	// Session is the owning session, when the invocation is model-authored.
	Session *session.Session
}

// ToolOutcome is one model-facing tool result. Content overrides the single
// text block when a tool returns structured blocks such as an image.
type ToolOutcome struct {
	Text    string
	IsError bool
	Content []any
	Meta    any
	Present any
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
	jobs       *jobManager
	// skills is the catalog this executor can load by name. It is resolved once
	// per executor so a load does not walk the directories again.
	skills []SkillDefinition
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
	executor := &ToolExecutor{permission: "danger-full-access", sandbox: "danger-full-access", jobs: newJobManager()}
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

// SetSkills records the catalog the skill tool may load from.
func (e *ToolExecutor) SetSkills(skills []SkillDefinition) {
	if e == nil {
		return
	}
	e.skills = skills
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
						"description": "Timeout in milliseconds. The executor applies its configured default and cap; on expiry the command moves to the background as a job instead of being killed.",
					},
					"workdir": map[string]any{
						"type":        "string",
						"description": "Working directory for this command. Defaults to the session workspace; a relative path is resolved against it.",
					},
					"run_in_background": map[string]any{
						"type":        "boolean",
						"description": "Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.",
					},
					"sandbox_permissions": map[string]any{
						"type":        "string",
						"description": "The narrowest wider sandbox mode for a one-shot retry of the exact command the sandbox just denied; the retry asks the user for approval.",
						"enum":        []string{"workspace-write", "danger-full-access"},
					},
					"justification": map[string]any{
						"type":        "string",
						"description": "Required with sandbox_permissions: one sentence for the user explaining why this exact command needs the wider access. Use the language of the user’s current request.",
					},
				},
				"required": []string{"description", "command"},
			},
		},
		{
			Name:        workingDirectoryToolName,
			Description: workingDirectoryDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cd": map[string]any{
						"type":        "string",
						"description": "Existing directory to enter. Omit to read the current directory.",
					},
				},
			},
		},
		{
			Name:        presentToolName,
			Description: presentDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"files": map[string]any{
						"type":        "array",
						"description": "Usually the 1-2 most important deliverables; at most 4 per call.",
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Path of an existing regular file. Relative paths use the Session working directory.",
								},
								"description": map[string]any{
									"type":        "string",
									"description": "Brief description for the user.",
								},
							},
							"required": []string{"path"},
						},
					},
				},
				"required": []string{"files"},
			},
		},
		{
			Name:        skillToolName,
			Description: skillDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The exact skill name from the available skills list.",
					},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        getGoalToolName,
			Description: getGoalDescription,
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        createGoalToolName,
			Description: createGoalDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"objective": map[string]any{
						"type":        "string",
						"description": "The concrete completion objective inferred from the direct human request.",
					},
					"max_goal_rounds": map[string]any{
						"type":        "number",
						"description": "Optional positive safe-integer limit on automatic continuation rounds.",
					},
				},
				"required": []string{"objective"},
			},
		},
		{
			Name:        updateGoalToolName,
			Description: updateGoalDescription,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal_id": map[string]any{
						"type":        "string",
						"description": "Exact id returned by get_goal.",
					},
					"revision": map[string]any{
						"type":        "number",
						"description": "Exact positive revision returned by get_goal.",
					},
					"action": map[string]any{
						"type":        "string",
						"description": "edit, pause, and resume require a direct top-level human request. complete and blocked are also allowed during an automatic continuation of this goal; blocked is rejected before the configured minimum round count.",
						"enum":        []string{"edit", "pause", "resume", "complete", "blocked"},
					},
					"objective": map[string]any{
						"type":        "string",
						"description": "Replacement objective; valid only with action edit.",
					},
					"max_goal_rounds": map[string]any{
						"type":        "number",
						"description": "Replacement cap; valid only with action edit.",
					},
					"blocked_reason": map[string]any{
						"type":        "string",
						"description": "Required only with action blocked: the concrete condition that persisted across rounds and blocks progress.",
					},
				},
				"required": []string{"goal_id", "revision", "action"},
			},
		},
		{
			Name:        askUserQuestionToolName,
			Description: askUserQuestionDescription,
			Parameters:  askUserQuestionParameters(),
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
					"sandbox_permissions": map[string]any{
						"type":        "string",
						"description": "The narrowest wider sandbox mode for a one-shot retry of the exact operation the sandbox just denied; the retry asks the user for approval.",
						"enum":        []string{"workspace-write", "danger-full-access"},
					},
					"justification": map[string]any{
						"type":        "string",
						"description": "Required with sandbox_permissions: one sentence for the user explaining why this exact file operation needs the wider access. Use the language of the user’s current request.",
					},
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
					"sandbox_permissions": map[string]any{
						"type":        "string",
						"description": "The narrowest wider sandbox mode for a one-shot retry of the exact operation the sandbox just denied; the retry asks the user for approval.",
						"enum":        []string{"workspace-write", "danger-full-access"},
					},
					"justification": map[string]any{
						"type":        "string",
						"description": "Required with sandbox_permissions: one sentence for the user explaining why this exact file operation needs the wider access. Use the language of the user’s current request.",
					},
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
			Name:        "todo_write",
			Description: "Record and update a task list to plan multi-step work and show progress; skip it for trivial single-step tasks. Add one todo per concrete step before you start. While work remains, keep the todos being worked on `in_progress`, several only when work runs in parallel. Mark each todo `completed` as soon as it is done.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type":        "array",
						"description": "The COMPLETE task list, replacing any previous list.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"content": map[string]any{"type": "string", "description": "What the task is — a short imperative line."},
								"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}, "description": "pending (not started) | in_progress (now) | completed (done)."},
							},
							"required":             []string{"content", "status"},
							"additionalProperties": false,
						},
					},
				},
				"required": []string{"todos"},
			},
		},
		{
			Name:        "job_output",
			Description: "Read a background job: output since the previous read for stream jobs, or the result of a finished final-output job.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":     map[string]any{"type": "string", "description": "Job id returned by the tool that started the background work."},
					"wait":       map[string]any{"type": "boolean", "description": "Block until the job finishes or the timeout expires; a timed-out wait leaves the job running. Defaults to false."},
					"timeout_ms": map[string]any{"type": "number", "description": "Max wait in milliseconds with wait: true. Defaults to and is capped by configuration."},
				},
				"required": []string{"job_id"},
			},
		},
		{
			Name:        "job_list",
			Description: "List your background jobs (running and finished) with their ids, kinds, and statuses.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "job_kill",
			Description: "Request cancellation of a running background job.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string", "description": "Job id returned by the tool that started the background work."},
					"reason": map[string]any{"type": "string", "description": "Optional short reason, recorded in the log and forwarded to the job."},
				},
				"required": []string{"job_id"},
			},
		},
		{
			Name:        "read_image",
			Description: "Read a PNG/JPEG/WebP/GIF file and return the image itself. Large images are downscaled automatically; do not install image libraries or create thumbnails to inspect an image.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string", "description": "Path to the image file, resolved by the filesystem backend."},
				},
				"required": []string{"file_path"},
			},
		},
		{
			Name:        "web_search",
			Description: "Search the web for current information. Returns an optional summary answer and a list of source URLs.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"queries": map[string]any{
						"type":        "array",
						"description": "1–10 search queries; their results are merged.",
						"items":       map[string]any{"type": "string"},
					},
				},
				"required": []string{"queries"},
			},
		},
		{
			Name:        "web_fetch",
			Description: "Fetch the content of a specific HTTP(S) URL and return it decoded to text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "The HTTP(S) URL to fetch."},
				},
				"required": []string{"url"},
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
	outcome := e.ExecuteOutcome(ctx, inv)
	return outcome.Text, outcome.IsError
}

// ExecuteOutcome runs one tool call and returns its structured result.
func (e *ToolExecutor) ExecuteOutcome(ctx context.Context, inv ToolInvocation) ToolOutcome {
	if !e.Enabled() {
		return ToolOutcome{Text: "tools are not enabled", IsError: true}
	}
	var args map[string]any
	if strings.TrimSpace(inv.Arguments) != "" {
		if err := json.Unmarshal([]byte(inv.Arguments), &args); err != nil {
			return ToolOutcome{Text: "invalid tool arguments: " + err.Error(), IsError: true}
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	switch inv.Name {
	case "bash":
		text, failed := e.bash(ctx, inv, args)
		return ToolOutcome{Text: text, IsError: failed}
	case "read":
		text, failed := e.read(ctx, inv, args)
		return ToolOutcome{Text: text, IsError: failed}
	case "write":
		text, failed := e.write(ctx, inv, args)
		return ToolOutcome{Text: text, IsError: failed}
	case "edit":
		text, failed := e.edit(ctx, inv, args)
		return ToolOutcome{Text: text, IsError: failed}
	case "glob":
		text, failed := e.glob(ctx, inv, args)
		return ToolOutcome{Text: text, IsError: failed}
	case "grep":
		text, failed := e.grep(ctx, inv, args)
		return ToolOutcome{Text: text, IsError: failed}
	case askUserQuestionToolName:
		return e.askUserQuestion(ctx, inv, args)
	case workingDirectoryToolName:
		return e.workingDirectory(ctx, inv, args)
	case presentToolName:
		return e.present(ctx, inv, args)
	case skillToolName:
		return e.skill(ctx, inv, args)
	case getGoalToolName:
		return e.getGoal(ctx, inv)
	case createGoalToolName:
		return e.createGoal(ctx, inv, args)
	case updateGoalToolName:
		return e.updateGoal(ctx, inv, args)
	case "todo_write":
		return e.todoWrite(inv, args)
	case "job_output":
		return e.jobOutput(ctx, inv, args)
	case "job_list":
		return e.jobList(inv)
	case "job_kill":
		return e.jobKill(inv, args)
	case "read_image":
		return e.readImage(ctx, inv, args)
	case "web_search":
		return e.webSearch(ctx, args)
	case "web_fetch":
		return e.webFetch(ctx, args)
	default:
		return ToolOutcome{Text: "unknown tool: " + inv.Name, IsError: true}
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
