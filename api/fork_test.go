package api

import (
	"encoding/json"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestForkChildIsSeededAndPromptable is the fork contract: the child copies the
// source's prefix, is marked seeded, records where it came from, and — the part
// that matters to a user — can be prompted on its own.
func TestForkChildIsSeededAndPromptable(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	// One completed turn, so there is a default cut.
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "hello"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)

	// A child needs a real directory: the cwd is where its own calls will run.
	childDir := t.TempDir()
	value := f.invoke(t, "session/fork", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "cwd": childDir},
	}).mustSucceed(t)
	var forked struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(value, &forked); err != nil {
		t.Fatalf("decode %s: %v", value, err)
	}
	if forked.SessionID == "" || forked.SessionID == opened.ID() {
		t.Fatalf("forked id = %q", forked.SessionID)
	}

	child, err := f.store.Open(forked.SessionID)
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	header := child.Header()
	// The child knows it is a fork, and of which session: a session that cannot
	// say where it came from cannot be told apart from one the user started.
	if !header.IsSeeded {
		t.Fatal("the child is not marked seeded")
	}
	if header.ParentSession != opened.ID() {
		t.Fatalf("parentSession = %q, want %q", header.ParentSession, opened.ID())
	}
	if header.Cwd != childDir {
		t.Fatalf("cwd = %q, want %q", header.Cwd, childDir)
	}
	// The prefix is copied, and closed with a fork marker so no turn is left open.
	if len(child.Events()) == 0 {
		t.Fatal("the child has no history")
	}
	last := child.Events()[len(child.Events())-1]
	if last.Type != session.EventSessionEndSeed {
		t.Fatalf("the child's last event is %q, want the fork closer", last.Type)
	}

	// The parent still exists and is readable.
	if _, err := f.store.Open(opened.ID()); err != nil {
		t.Fatalf("the parent is gone after the fork: %v", err)
	}

	// And the child can be prompted: a child that cannot be continued is a copy,
	// not a fork.
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": forked.SessionID,
			"content":   []map[string]any{{"type": "text", "text": "continue here"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, child, 1, 10*1000*1000*1000)
}

// TestForkCutMustExist keeps an out-of-range cut from producing a child with
// history the source never had.
func TestForkCutMustExist(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "hello"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)
	f.invoke(t, "session/fork", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "atSeq": 9999},
	}).mustFail(t, CodeBadRequest)
}

// TestForkWithoutACompletedTurnIsUnavailable states the smaller failure plainly.
func TestForkWithoutACompletedTurnIsUnavailable(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "session/fork", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustFail(t, CodeForkUnavailable)
}
