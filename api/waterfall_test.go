package api

import (
	"context"
	"encoding/json"
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

// TestApprovalWaterfallTimesOutFailClosed covers a page that went away.
func TestApprovalWaterfallTimesOutFailClosed(t *testing.T) {
	f := newFixture(t)
	f.service.cfg.EventWait = 120 * time.Millisecond
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
