package api

import (
	"testing"
	"time"
)

// TestPermissionSetRejectedWhileRunning is the backend half of "you cannot change
// the gear mid-turn".
//
// The policy is folded per tool call, so a write that lands while a turn runs would
// be honoured by the steps that have not executed yet: two calls in one turn checked
// against different rules, and a transcript that shows one mode while half the turn
// ran under another. The page disables its control, but the page is not the only
// caller — this refusal is what makes the rule true for every caller.
func TestPermissionSetRejectedWhileRunning(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	f := newBlockingFixture(t, block)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)

	deadline := time.Now().Add(5 * time.Second)
	for !f.runner.Busy(opened.ID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.runner.Busy(opened.ID()) {
		t.Fatal("the turn never started")
	}

	f.invoke(t, "permissionPresets/set", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "preset": "danger-full-access"},
	}).mustFail(t, CodeBusy)

	// The refusal must not have written anything: the three records are still the
	// ones the session started with.
	for _, event := range opened.Events() {
		if event.Type == "permission/preset" {
			var payload map[string]any
			_ = payload
			t.Fatalf("a refused set still wrote %s", event.Type)
		}
	}
}
