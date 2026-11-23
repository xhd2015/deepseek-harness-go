package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestPresentRecordsDeclaredFiles proves a delivery is visible after a reload.
//
// The tool result alone is not enough: the page builds its deliverable list from
// the durable event, so a backend that answers with a result and writes nothing
// shows the model a success the user never sees.
func TestPresentRecordsDeclaredFiles(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	report := filepath.Join(root, "report.md")
	writeTestFile(t, report, "# report\n")

	opened := newOpenSession(t, root)
	inv := ToolInvocation{SessionID: opened.ID(), Session: opened, CallID: "call-present", Cwd: root}
	output, isError := callTool(t, executor, inv, "present", map[string]any{
		"files": []map[string]any{{"path": "report.md", "description": "the report"}},
	})
	if isError {
		t.Fatalf("present failed: %s", output)
	}
	var result struct {
		Turn  int64 `json:"turn"`
		Files []struct {
			Path        string `json:"path"`
			Description string `json:"description"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if len(result.Files) != 1 || result.Files[0].Description != "the report" {
		t.Fatalf("files = %+v", result.Files)
	}
	// The path is absolute in the record: a relative one would stop meaning
	// anything once the session's cwd moved.
	if !filepath.IsAbs(result.Files[0].Path) {
		t.Fatalf("path = %q, want absolute", result.Files[0].Path)
	}
	// The delivery is durable, which is what the page reads.
	var found bool
	for _, event := range opened.Events() {
		if event.Type == session.EventDeliverablesPresented {
			found = true
		}
	}
	if !found {
		t.Fatal("no deliverables/presented event was recorded")
	}
}

// TestPresentRefusesMissingFiles keeps the model from announcing a deliverable the
// user cannot open.
func TestPresentRefusesMissingFiles(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	opened := newOpenSession(t, root)
	inv := ToolInvocation{SessionID: opened.ID(), Session: opened, CallID: "call-2", Cwd: root}
	output, isError := callTool(t, executor, inv, "present", map[string]any{
		"files": []map[string]any{{"path": "missing.md"}},
	})
	if !isError {
		t.Fatalf("presented a file that does not exist: %s", output)
	}
	if !strings.Contains(output, "file not found") {
		t.Fatalf("message = %q", output)
	}
	// A directory is not a deliverable either.
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	output, isError = callTool(t, executor, inv, "present", map[string]any{
		"files": []map[string]any{{"path": "adir"}},
	})
	if !isError || !strings.Contains(output, "not a regular file") {
		t.Fatalf("directory outcome = %q (error=%v)", output, isError)
	}
}

// TestPresentRejectsEmptyAndOversizedLists pins the per-call bounds.
func TestPresentRejectsEmptyAndOversizedLists(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	opened := newOpenSession(t, root)
	inv := ToolInvocation{SessionID: opened.ID(), Session: opened, CallID: "call-3", Cwd: root}
	if _, isError := callTool(t, executor, inv, "present", map[string]any{"files": []any{}}); !isError {
		t.Fatal("an empty list was accepted")
	}
	files := make([]map[string]any, presentedFileMax+1)
	for i := range files {
		files[i] = map[string]any{"path": "x"}
	}
	if _, isError := callTool(t, executor, inv, "present", map[string]any{"files": files}); !isError {
		t.Fatal("an oversized list was accepted")
	}
}

// newOpenSession creates a writable session rooted at a test directory.
func newOpenSession(t *testing.T, cwd string) *session.Session {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened, err := store.Create(cwd, "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return opened
}

var _ = context.Background
