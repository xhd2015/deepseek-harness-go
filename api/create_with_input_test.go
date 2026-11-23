package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestCreateWithInputPromptRunsOneTurn is the prompt path: one request, one turn.
func TestCreateWithInputPromptRunsOneTurn(t *testing.T) {
	f := newFixture(t)
	var created struct {
		SessionID  string `json:"sessionId"`
		InputError string `json:"inputError"`
	}
	decode(t, f.invoke(t, "session/createWithInput", map[string]any{
		"request": map[string]any{"prompt": "hello there"},
	}).mustSucceed(t), &created)
	if created.SessionID == "" {
		t.Fatal("no session id was returned")
	}
	if created.InputError != "" {
		t.Fatalf("inputError = %q", created.InputError)
	}
	opened, err := f.store.Open(created.SessionID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)
	// The prompt became a real user message, not a draft.
	var sawUser bool
	for _, event := range opened.Events() {
		if event.Type == session.EventUserMessage && strings.Contains(string(event.Data), "hello there") {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatal("the prompt was never committed as a user message")
	}
}

// TestCreateWithInputDraftLeavesTheComposerAlone is the other path: the text is
// readable as a draft and produces no turn.
func TestCreateWithInputDraftLeavesTheComposerAlone(t *testing.T) {
	f := newFixture(t)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	decode(t, f.invoke(t, "session/createWithInput", map[string]any{
		"request": map[string]any{"draft": "half-typed thought"},
	}).mustSucceed(t), &created)
	if created.SessionID == "" {
		t.Fatal("no session id")
	}
	var draft struct {
		Text string `json:"text"`
	}
	decode(t, f.invoke(t, "session/getDraft", map[string]any{
		"request": map[string]any{"sessionId": created.SessionID},
	}).mustSucceed(t), &draft)
	if draft.Text != "half-typed thought" {
		t.Fatalf("draft = %q", draft.Text)
	}
	// No turn ran: the composer holds text, the transcript does not.
	opened, err := f.store.Open(created.SessionID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, event := range opened.Events() {
		if event.Type == session.EventTurnStart || event.Type == session.EventUserMessage {
			t.Fatalf("the draft produced a transcript row: %s", event.Type)
		}
	}
}

// TestCreateWithInputRefusesBothInputs is the mutual exclusion DSH enforces.
func TestCreateWithInputRefusesBothInputs(t *testing.T) {
	f := newFixture(t)
	f.invoke(t, "session/createWithInput", map[string]any{
		"request": map[string]any{"draft": "a", "prompt": "b"},
	}).mustFail(t, CodeBadRequest)
}

// TestCreateWithInputRefusesImagesWithoutText keeps an image from arriving alone.
func TestCreateWithInputRefusesImagesWithoutText(t *testing.T) {
	f := newFixture(t)
	f.invoke(t, "session/createWithInput", map[string]any{
		"request": map[string]any{"images": []map[string]any{{"mediaType": "image/png", "data": "AA=="}}},
	}).mustFail(t, CodeBadRequest)
}

// TestCreateWithInputKeepsTheSessionOnFailure is the rule that makes a retry
// possible: the id survives so the caller can send the input again.
func TestCreateWithInputKeepsTheSessionOnFailure(t *testing.T) {
	f := newFixture(t)
	// A runner that cannot start leaves the prompt unusable while the session
	// itself is perfectly valid.
	f.service.cfg.Runner = nil
	var created struct {
		SessionID  string `json:"sessionId"`
		InputError string `json:"inputError"`
	}
	decode(t, f.invoke(t, "session/createWithInput", map[string]any{
		"request": map[string]any{"prompt": "will not run"},
	}).mustSucceed(t), &created)
	if created.SessionID == "" {
		t.Fatal("the session was discarded on an input failure")
	}
	if created.InputError == "" {
		t.Fatal("a failed input reported no inputError")
	}
	if _, err := f.store.Open(created.SessionID); err != nil {
		t.Fatalf("the session is gone: %v", err)
	}
}

// TestCreateWithInputWithoutInputIsJustCreate keeps the plain path plain.
func TestCreateWithInputWithoutInputIsJustCreate(t *testing.T) {
	f := newFixture(t)
	var created struct {
		SessionID  string `json:"sessionId"`
		InputError string `json:"inputError"`
	}
	decode(t, f.invoke(t, "session/createWithInput", map[string]any{
		"request": map[string]any{},
	}).mustSucceed(t), &created)
	if created.SessionID == "" {
		t.Fatal("no session id")
	}
	if created.InputError != "" {
		t.Fatalf("inputError = %q", created.InputError)
	}
}

var _ = json.Marshal
