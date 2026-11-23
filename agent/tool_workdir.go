package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// workingDirectoryToolName is the DSH tool for reading or changing the session's
// directory.
const workingDirectoryToolName = "working_directory"

// workingDirectoryDescription is the DSH wording, verbatim.
const workingDirectoryDescription = "Read the current working directory, or change it with cd. Relative paths use the current directory. Existing shells and running processes keep their own directories."

// workingDirectory changes the session's directory for later calls.
//
// The change is recorded on the session rather than kept in this process, so it
// survives a restart and so every later call — including one made by a different
// process reading the same session — resolves relative paths against the same
// place. A directory that does not exist is refused: accepting it would make every
// later relative path fail for a reason the model cannot see.
func (e *ToolExecutor) workingDirectory(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	_ = ctx
	raw, hasCD := args["cd"]
	target := ""
	if hasCD && raw != nil {
		value, ok := raw.(string)
		if !ok {
			return ToolOutcome{Text: "cd must be a string", IsError: true}
		}
		target = strings.TrimSpace(value)
	}
	if hasCD && raw != nil && target == "" {
		// An empty cd is a mistake, not a request to read: the tool's contract
		// says omit it to read.
		return ToolOutcome{Text: "cd must not be empty; omit it to read the current directory", IsError: true}
	}
	current := workingDirectoryOf(inv)
	if target == "" {
		return ToolOutcome{Text: renderWorkdir(current)}
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(current, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cd %s: %v", resolved, err), IsError: true}
	}
	if !info.IsDir() {
		return ToolOutcome{Text: fmt.Sprintf("cd %s: not a directory", resolved), IsError: true}
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cd %s: %v", resolved, err), IsError: true}
	}
	if inv.Session == nil {
		return ToolOutcome{Text: "working_directory needs an open session", IsError: true}
	}
	if _, err := inv.Session.Append(session.EventWorkingDirectory, map[string]any{"cwd": absolute}); err != nil {
		return ToolOutcome{Text: "record working directory: " + err.Error(), IsError: true}
	}
	return ToolOutcome{Text: renderWorkdir(absolute)}
}

// renderWorkdir is the tool's textual result: the directory on its own line.
func renderWorkdir(cwd string) string {
	return cwd
}

// workingDirectoryOf reports the directory a call resolves against: the session's
// latest recorded change, or its header when it never changed.
func workingDirectoryOf(inv ToolInvocation) string {
	if inv.Session != nil {
		if cwd := inv.Session.Cwd(); cwd != "" {
			return cwd
		}
	}
	return inv.Cwd
}
