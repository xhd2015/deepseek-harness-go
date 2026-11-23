package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestFullApprovalChainRunsTheApprovedCommand exercises the production topology end
// to end: the executor denies, asks, the page answers through the waterfall, the
// decision is recorded, and the very same command then runs.
//
// This is the chain that was broken on the real host — the hook closed over a nil
// runner, so every escalation answered `unavailable` without ever asking.
func TestFullApprovalChainRunsTheApprovedCommand(t *testing.T) {
	f := newFixture(t)
	f.service.cfg.EventWait = 3 * time.Second
	// The session's cwd has to exist: the tool resolves its workspace before the
	// sandbox gate runs, so a missing directory fails for an unrelated reason.
	if err := os.MkdirAll(filepath.Join(f.root, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	opened := f.createSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := f.service.Open(ctx, EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(ctx); err != nil {
		t.Fatal(err)
	}

	home, _ := os.UserHomeDir()
	target := filepath.Join(home, "live-chain.txt")
	_ = os.RemoveAll(target)
	t.Cleanup(func() { _ = os.RemoveAll(target) })

	type res struct {
		outcome agent.ToolOutcome
	}
	done := make(chan res, 1)
	go func() {
		outcome := f.runner.ToolsExecuteForTest(ctx, opened, "bash",
			`{"description":"escalate","command":"echo x > `+target+`","sandbox_permissions":"danger-full-access","justification":"needs a path outside"}`)
		done <- res{outcome: outcome}
	}()

	// Answer whatever waterfall arrives.
	deadline := time.Now().Add(6 * time.Second)
	answered := false
	for !answered && time.Now().Before(deadline) {
		raw, err := stream.Next(ctx)
		if err != nil {
			break
		}
		var frame struct {
			Type    string `json:"type"`
			Event   string `json:"event"`
			EventID string `json:"eventId"`
		}
		if json.Unmarshal(raw, &frame) != nil || frame.Type != "waterfall" {
			continue
		}
		if frame.Event != "approval/request" {
			continue
		}
		answer, _ := json.Marshal(map[string]any{
			"clientId": "c", "eventId": frame.EventID,
			"outcome": map[string]any{"kind": "result", "value": "allowed-once"},
		})
		f.invoke(t, "$events/result", json.RawMessage(answer)).mustSucceed(t)
		answered = true
	}
	if !answered {
		t.Fatal("no approval/request waterfall was published")
	}

	select {
	case got := <-done:
		if got.outcome.IsError {
			t.Fatalf("the approved call still failed: %s", got.outcome.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call never unblocked")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the approved write did not run: %v", err)
	}
	var asked, decided int
	for _, e := range opened.Events() {
		switch e.Type {
		case session.EventApprovalAsked:
			asked++
		case session.EventApprovalDecided:
			decided++
		}
	}
	t.Logf("asked=%d decided=%d", asked, decided)
	if asked != 1 || decided != 1 {
		t.Fatalf("audit pair asked=%d decided=%d", asked, decided)
	}
}
