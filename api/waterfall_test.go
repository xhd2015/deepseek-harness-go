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

// TestApprovalTravelsAsAWaterfall is the transport contract: the page receives an
// answerable frame, not a notification, and its answer unblocks the call.
func TestApprovalTravelsAsAWaterfall(t *testing.T) {
	f := newFixture(t)
	f.service.cfg.EventWait = 2 * time.Second
	opened := f.createSession(t)

	// Open the event stream the page would hold.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := f.service.Open(ctx, EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer stream.Close()
	// The first frame is the readiness announcement.
	if _, err := stream.Next(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}

	type result struct{ outcome agent.ApprovalOutcome }
	done := make(chan result, 1)
	go func() {
		done <- result{outcome: f.runner.RequestApprovalForTest(opened, "bash", "call-w", "needs wider access")}
	}()

	// Read frames until the waterfall arrives.
	var frame struct {
		Type    string         `json:"type"`
		Event   string         `json:"event"`
		EventID string         `json:"eventId"`
		AgentID string         `json:"agentId"`
		Request map[string]any `json:"request"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no waterfall frame arrived")
		}
		raw, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		if frame.Type == "waterfall" {
			break
		}
	}
	if frame.Event != "approval/request" {
		t.Fatalf("event = %q", frame.Event)
	}
	if frame.EventID == "" {
		t.Fatal("the waterfall has no eventId to correlate the answer")
	}
	if frame.Request["toolName"] != "bash" {
		t.Fatalf("request = %+v", frame.Request)
	}

	// Answer it the way the client does.
	answer, _ := json.Marshal(map[string]any{
		"clientId": "client-1",
		"eventId":  frame.EventID,
		"outcome":  map[string]any{"kind": "result", "value": "allowed-once"},
	})
	f.invoke(t, "$events/result", json.RawMessage(answer)).mustSucceed(t)

	select {
	case got := <-done:
		if got.outcome != agent.ApprovalAllowedOnce {
			t.Fatalf("outcome = %q, want allowed-once", got.outcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the answer did not unblock the call")
	}

	// The audit pair was written around the wait.
	var asked, decided int
	for _, event := range opened.Events() {
		switch event.Type {
		case session.EventApprovalAsked:
			asked++
		case session.EventApprovalDecided:
			decided++
		}
	}
	if asked != 1 || decided != 1 {
		t.Fatalf("asked=%d decided=%d", asked, decided)
	}
}

// TestApprovalAnswerUnblocksImmediately is the regression this fixes: answering a
// waterfall must release the waiting call at once, not at the end of the window.
//
// The previous wiring had two waits — the notifier blocked on the waterfall while
// the tool blocked on the runner — so the answer settled a channel nobody was
// reading. The symptom was a page with no pending item and a turn stuck for the
// full timeout.
func TestApprovalAnswerUnblocksImmediately(t *testing.T) {
	f := newFixture(t)
	// A window long enough that waiting it out would fail the assertion below.
	f.service.cfg.EventWait = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := f.service.Open(ctx, EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}

	homeDir, _ := os.UserHomeDir()
	target := filepath.Join(homeDir, "unblock-now.txt")
	_ = os.RemoveAll(target)
	t.Cleanup(func() { _ = os.RemoveAll(target) })
	if err := os.MkdirAll(filepath.Join(f.root, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	opened := f.createSession(t)

	type res struct{ outcome agent.ToolOutcome }
	done := make(chan res, 1)
	start := time.Now()
	go func() {
		done <- res{outcome: f.runner.ToolsExecuteForTest(ctx, opened, "bash",
			`{"description":"escalate","command":"echo x > `+target+`","sandbox_permissions":"danger-full-access","justification":"needs a path outside"}`)}
	}()

	// Answer the frame as soon as it arrives.
	var eventID string
	for eventID == "" {
		raw, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("no waterfall frame: %v", err)
		}
		var frame struct {
			Type    string `json:"type"`
			Event   string `json:"event"`
			EventID string `json:"eventId"`
		}
		if json.Unmarshal(raw, &frame) == nil && frame.Type == "waterfall" && frame.Event == "approval/request" {
			eventID = frame.EventID
		}
	}
	answer, _ := json.Marshal(map[string]any{
		"clientId": "c", "eventId": eventID,
		"outcome": map[string]any{"kind": "result", "value": "allowed-once"},
	})
	f.invoke(t, "$events/result", json.RawMessage(answer)).mustSucceed(t)

	select {
	case got := <-done:
		if got.outcome.IsError {
			t.Fatalf("the approved call failed: %s", got.outcome.Text)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("the call unblocked after %v: the answer is not what released it", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call never unblocked after a granted answer")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the approved command did not run: %v", err)
	}
}

// TestApprovalWaterfallTimesOutFailClosed covers a page that went away.
func TestApprovalWaterfallTimesOutFailClosed(t *testing.T) {
	f := newFixture(t)
	// Both windows are shortened: the waterfall publish and the runner's wait are
	// separate timers, and the call waits on the runner's.
	f.service.cfg.EventWait = 120 * time.Millisecond
	f.runner.SetApprovalWindow(120 * time.Millisecond)
	opened := f.createSession(t)
	start := time.Now()
	outcome := f.runner.RequestApprovalForTest(opened, "bash", "call-t", "reason")
	if outcome != agent.ApprovalUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the wait took %v: it is not bounded", elapsed)
	}
	// The question is still recorded, and the decision says it went unanswered.
	var sawDecided bool
	for _, event := range opened.Events() {
		if event.Type == session.EventApprovalDecided {
			sawDecided = true
			if !contains(string(event.Data), "unavailable") {
				t.Fatalf("decided = %s", event.Data)
			}
		}
	}
	if !sawDecided {
		t.Fatal("a timed-out approval left no decision record")
	}
}

// contains is a local substring check to keep the assertion readable.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

// indexOf reports the first index of needle in haystack, or -1.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestEventResultRejectsUnknownIDs keeps a stale page from believing it decided.
func TestEventResultRejectsUnknownIDs(t *testing.T) {
	f := newFixture(t)
	f.invoke(t, "$events/result", json.RawMessage(`{"eventId":"remote-event-999","outcome":{"kind":"result","value":"allowed-once"}}`)).
		mustFail(t, CodeNotFound)
}

// TestExpiredApprovalPublishesCancel is what keeps a card from outliving its
// question. Without the cancel frame a consumer keeps the button on screen and the
// user answers something already recorded.
//
// The dispatch is driven synchronously on its own goroutine while the test drains
// the stream, and the goroutine is joined before returning: a detached request would
// keep a connection open against the model server and make the fixture's cleanup
// wait for it, which is a hang in the suite rather than a finding.
func TestExpiredApprovalPublishesCancel(t *testing.T) {
	f := newFixture(t)
	f.runner.SetApprovalWindow(150 * time.Millisecond)
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
	stream.Next(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.runner.ToolsExecuteForTest(context.Background(), opened, "bash",
			`{"description":"e","command":"echo x > /tmp/cancel-probe.txt","sandbox_permissions":"danger-full-access","justification":"because"}`)
	}()

	var sawWaterfall, sawCancel bool
	deadline := time.After(8 * time.Second)
drain:
	for !sawCancel {
		select {
		case <-deadline:
			break drain
		default:
		}
		raw, err := stream.Next(ctx)
		if err != nil {
			break
		}
		var frame struct {
			Type    string `json:"type"`
			EventID string `json:"eventId"`
		}
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "waterfall":
			sawWaterfall = true
		case "cancel":
			if !sawWaterfall {
				t.Fatal("a cancel arrived before the question it cancels")
			}
			if frame.EventID == "" {
				t.Fatal("the cancel frame carries no eventId to correlate")
			}
			sawCancel = true
		}
	}
	// The dispatch already returned: the wait expired and wrote its decision.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch never returned after the window expired")
	}
	if !sawCancel {
		t.Fatal("an expired approval published no cancel frame")
	}
}
