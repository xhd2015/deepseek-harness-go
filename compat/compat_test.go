// Package compat proves that this backend's on-disk sessions are readable by
// agent-pro's independent DSH reader, which is the compatibility oracle for the
// DeepSeek Harness persistence format.
package compat

import (
	"os"
	"path/filepath"
	"testing"

	agentpro "github.com/xhd2015/agent-pro/agent/dsh/sessions"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestAgentProReadsOurSessions writes a session with this backend and reads it
// back through agent-pro's reader, including event fields agent-pro folds.
func TestAgentProReadsOurSessions(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cwd := t.TempDir()
	opened, err := store.Create(cwd, "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := opened.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append turn/start: %v", err)
	}
	if _, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
		"role":    "user",
		"id":      session.NewID(),
		"content": []any{map[string]any{"type": "text", "text": "hello from dsh-go"}},
		"source":  map[string]any{"kind": "user", "rpcId": "rpc-compat"},
	}); err != nil {
		t.Fatalf("Append user message: %v", err)
	}
	if _, err := opened.Append(session.EventSessionTitle, map[string]any{
		"title":       "Compat title",
		"messageSeqs": []int64{},
		"source":      map[string]any{"kind": "user"},
	}); err != nil {
		t.Fatalf("Append title: %v", err)
	}
	if _, err := opened.Append(session.EventRequestHeader, map[string]any{
		"header": map[string]any{
			"config": map[string]any{
				"provider": "deepseek-official",
				"model":    "deepseek-flash",
			},
		},
		"reason": "initial",
	}); err != nil {
		t.Fatalf("Append request header: %v", err)
	}
	if _, err := opened.Append(session.EventTurnEnd, map[string]any{
		"turn": 1, "reason": map[string]any{"kind": "completed"},
	}); err != nil {
		t.Fatalf("Append turn/end: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader, err := agentpro.NewStore(agentpro.Options{SessionsDir: root})
	if err != nil {
		t.Fatalf("agent-pro NewStore: %v", err)
	}
	rows, warnings, err := reader.List()
	if err != nil {
		t.Fatalf("agent-pro List: %v", err)
	}
	for _, warning := range warnings {
		t.Logf("agent-pro warning: %s", warning)
	}
	if len(rows) != 1 {
		t.Fatalf("agent-pro listed %d sessions, want 1 (warnings: %v)", len(rows), warnings)
	}
	row := rows[0]
	if row.ID != opened.ID() {
		t.Errorf("id = %q, want %q", row.ID, opened.ID())
	}
	if row.Cwd != cwd {
		t.Errorf("cwd = %q, want %q", row.Cwd, cwd)
	}
	if row.FormatVersion != 4 || row.Encoding != "zstd" {
		t.Errorf("format = v%d/%s, want v4/zstd", row.FormatVersion, row.Encoding)
	}
	if row.Title != "Compat title" {
		t.Errorf("title = %q", row.Title)
	}
	if row.Provider != "deepseek-official" || row.Model != "deepseek-flash" {
		t.Errorf("route = %s/%s", row.Provider, row.Model)
	}
	if row.Outcome != "completed" {
		t.Errorf("outcome = %q", row.Outcome)
	}

	inspection, err := reader.Inspect(opened.ID())
	if err != nil {
		t.Fatalf("agent-pro Inspect: %v", err)
	}
	if len(inspection.Events) != 5 {
		t.Fatalf("inspected %d events, want 5", len(inspection.Events))
	}
	for index, event := range inspection.Events {
		if event.Seq != int64(index) {
			t.Fatalf("event %d has seq %d", index, event.Seq)
		}
	}
}

// TestReadsAgentProFixture is the inverse: the reader this backend ships must
// accept an artifact agent-pro considers canonical.
func TestOurReaderAcceptsAgentProRoot(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "external", "agent-pro")); err != nil {
		t.Skip("external/agent-pro is not brought")
	}
	root := t.TempDir()
	store, err := session.NewStoreWithConfig(session.Config{Root: root})
	if err != nil {
		t.Fatalf("NewStoreWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened, err := store.Create("/tmp/compat", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := opened.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	store.Forget(opened.ID())
	if err := opened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.OpenRead(opened.ID())
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	if reopened.Header().ID != opened.ID() || len(reopened.Events()) != 1 {
		t.Fatalf("reopened %+v with %d events", reopened.Header(), len(reopened.Events()))
	}
}
