package api

import (
	"encoding/json"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestApprovalAsksThenDecides is the audit-pair contract: one question, one
// decision, in that order, naming the same id.
func TestApprovalAsksThenDecides(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	// Wire a decision that approves once, as the page would.
	answered := make(chan string, 1)
	f.runner.SetApprovalAnswerer(func(request agent.ApprovalRequest) agent.ApprovalOutcome {
		answered <- request.ID
		return agent.ApprovalAllowedOnce
	})
	outcome := f.runner.RequestApprovalForTest(opened, "bash", "call-1", "needs /etc")
	if outcome != agent.ApprovalAllowedOnce {
		t.Fatalf("outcome = %q", outcome)
	}
	select {
	case <-answered:
	default:
		t.Fatal("the answerer was never consulted")
	}

	var asked, decided int
	askID, decidedID := "", ""
	for _, event := range opened.Events() {
		var payload map[string]any
		_ = json.Unmarshal(event.Data, &payload)
		switch event.Type {
		case session.EventApprovalAsked:
			asked++
			askID, _ = payload["id"].(string)
			if payload["toolName"] != "bash" {
				t.Fatalf("asked payload = %v", payload)
			}
		case session.EventApprovalDecided:
			decided++
			decidedID, _ = payload["id"].(string)
			if payload["outcome"] != string(agent.ApprovalAllowedOnce) {
				t.Fatalf("decided payload = %v", payload)
			}
		}
	}
	if asked != 1 || decided != 1 {
		t.Fatalf("asked=%d decided=%d, want 1 each", asked, decided)
	}
	if askID == "" || askID != decidedID {
		t.Fatalf("the pair does not share an id: asked %q decided %q", askID, decidedID)
	}
}

// TestApprovalUnderNeverDoesNotAsk keeps the page out of a policy that has already
// decided no prompt will be shown.
func TestApprovalUnderNeverDoesNotAsk(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "permissionPresets/set", map[string]any{
		"sessionId": opened.ID(), "preset": "danger-full-access",
	}).mustSucceed(t)
	consulted := false
	f.runner.SetApprovalAnswerer(func(agent.ApprovalRequest) agent.ApprovalOutcome {
		consulted = true
		return agent.ApprovalAllowedOnce
	})
	if got := f.runner.RequestApprovalForTest(opened, "bash", "c", "reason"); got != agent.ApprovalRejected {
		t.Fatalf("outcome = %q, want rejected", got)
	}
	if consulted {
		t.Fatal("the answerer was consulted under `never`")
	}
}

// TestApprovalWithoutAnAnswererFailsClosed is the safe default: no one to decide
// means no grant.
func TestApprovalWithoutAnAnswererFailsClosed(t *testing.T) {
	f := newFixture(t)
	// The fixture wires a page hook by default; remove it so this covers the
	// deployment where nothing is listening at all.
	f.runner.ClearApprovalNotifier()
	opened := f.createSession(t)
	if got := f.runner.RequestApprovalForTest(opened, "bash", "c", "reason"); got != agent.ApprovalUnavailable {
		t.Fatalf("outcome = %q, want unavailable", got)
	}
}

// TestApprovalAnswerRejectsUnknownIds keeps a stale page from believing it acted.
//
// The answer channel is `$events/result`, the same one a native host exposes; the
// invented `approval/answer` RPC is gone because it settled a second channel the
// page could not see, so a granted decision left the call waiting.
func TestApprovalAnswerRejectsUnknownIds(t *testing.T) {
	f := newFixture(t)
	f.invoke(t, "$events/result", json.RawMessage(`{"eventId":"remote-event-404","outcome":{"kind":"result","value":"allowed-once"}}`)).
		mustFail(t, CodeNotFound)
}
