package session

import (
	"encoding/json"
	"testing"
)

// appendQuestionCall appends one ask_user_question call and returns its event.
func appendQuestionCall(t *testing.T, opened *Session, callID, arguments string) Event {
	t.Helper()
	event, err := opened.Append(EventToolCall, map[string]any{
		"turn": 1, "step": 1, "callId": callID, "name": askUserQuestionName, "arguments": arguments,
	})
	if err != nil {
		t.Fatalf("append tool call: %v", err)
	}
	return event
}

// appendQuestionResult appends the result that says the question was recorded.
func appendQuestionResult(t *testing.T, opened *Session, callID string, isError bool) {
	t.Helper()
	if _, err := opened.Append(EventToolResult, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"id": NewID(), "role": "tool", "toolCallId": callID,
			"content": []any{map[string]any{"type": "text", "text": `{"asked":true}`}},
			"isError": isError,
		},
	}); err != nil {
		t.Fatalf("append tool result: %v", err)
	}
}

// userQuestionsOf reads the projection face out of a baseline.
func userQuestionsOf(t *testing.T, values map[string]any) (active, settled []any) {
	t.Helper()
	raw, ok := values[QuestionProjectionKey]
	if !ok {
		t.Fatal("the baseline has no userQuestions projection")
	}
	face, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("projection is %T, want an object", raw)
	}
	active, _ = face["active"].([]any)
	settled, _ = face["settled"].([]any)
	return active, settled
}

// TestQuestionStaysAnswerableAfterItsCallReturns is why the card exists at all.
//
// The page renders the question from this projection, so a call that returned
// without leaving the question active shows the user nothing to answer — the model
// has asked, the log holds the question, and the page looks empty.
func TestQuestionStaysAnswerableAfterItsCallReturns(t *testing.T) {
	opened := newProjectionSession(t)
	arguments := `{"questions":[{"id":"color","question":"Which color?","header":"Pick one",` +
		`"options":[{"label":"Blue","description":"calm"},{"label":"Green"}],"multi_select":true}]}`
	appendQuestionCall(t, opened, "call-1", arguments)
	appendQuestionResult(t, opened, "call-1", false)

	active, settled := userQuestionsOf(t, Baseline(opened.Header(), opened.Events()).Values)
	if len(settled) != 0 {
		t.Fatalf("nothing was answered, yet %d questions are settled", len(settled))
	}
	if len(active) != 1 {
		t.Fatalf("active = %v, want the one open question", active)
	}
	item := active[0].(map[string]any)
	if item["callId"] != "call-1" {
		t.Fatalf("callId = %v", item["callId"])
	}
	// "continued" is the page's word for "the call returned, you can still answer".
	if item["state"] != "continued" {
		t.Fatalf("state = %v, want continued", item["state"])
	}
	questions := item["questions"].([]any)
	if len(questions) != 1 {
		t.Fatalf("questions = %v", questions)
	}
	question := questions[0].(map[string]any)
	if question["id"] != "color" || question["question"] != "Which color?" || question["header"] != "Pick one" {
		t.Fatalf("question = %v", question)
	}
	if question["multiSelect"] != true {
		t.Fatalf("multiSelect was not carried over: %v", question)
	}
	options := question["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("options = %v", options)
	}
	if first := options[0].(map[string]any); first["label"] != "Blue" || first["description"] != "calm" {
		t.Fatalf("first option = %v", first)
	}
}

// TestQuestionReplySettlesIt proves the answer moves the question out of the
// answerable set and keeps what the user chose.
func TestQuestionReplySettlesIt(t *testing.T) {
	opened := newProjectionSession(t)
	appendQuestionCall(t, opened, "call-1", `{"questions":[{"id":"color","question":"Which color?"}]}`)
	appendQuestionResult(t, opened, "call-1", false)

	payload, _ := json.Marshal(map[string]any{
		"answers": []map[string]any{{"id": "color", "selected": []string{"Blue"}, "custom": "and notes"}},
	})
	if _, err := opened.AppendSurface(EventUserMessage, map[string]any{
		"role":    "user",
		"id":      NewID(),
		"content": []any{map[string]any{"type": "text", "text": string(payload)}},
		"source":  map[string]any{"kind": "user-question-reply", "callId": "call-1", "outcome": "answered"},
	}); err != nil {
		t.Fatalf("append reply: %v", err)
	}

	active, settled := userQuestionsOf(t, Baseline(opened.Header(), opened.Events()).Values)
	if len(active) != 0 {
		t.Fatalf("the answered question is still active: %v", active)
	}
	if len(settled) != 1 {
		t.Fatalf("settled = %v, want one", settled)
	}
	entry := settled[0].(map[string]any)
	if entry["callId"] != "call-1" {
		t.Fatalf("settled callId = %v", entry["callId"])
	}
	answers := entry["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("answers = %v", answers)
	}
	answer := answers[0].(map[string]any)
	if answer["id"] != "color" || answer["custom"] != "and notes" {
		t.Fatalf("answer = %v", answer)
	}
}

// TestFailedQuestionIsNotAnswerable keeps a refusal from leaving a card behind.
func TestFailedQuestionIsNotAnswerable(t *testing.T) {
	opened := newProjectionSession(t)
	appendQuestionCall(t, opened, "call-1", `{"questions":[{"id":"color","question":"Which color?"}]}`)
	appendQuestionResult(t, opened, "call-1", true)

	active, settled := userQuestionsOf(t, Baseline(opened.Header(), opened.Events()).Values)
	if len(active) != 0 || len(settled) != 0 {
		t.Fatalf("a refused question left state behind: active=%v settled=%v", active, settled)
	}
}

// TestNoQuestionFoldsToAnEmptyView keeps the face present and empty.
func TestNoQuestionFoldsToAnEmptyView(t *testing.T) {
	opened := newProjectionSession(t)
	active, settled := userQuestionsOf(t, Baseline(opened.Header(), opened.Events()).Values)
	if len(active) != 0 || len(settled) != 0 {
		t.Fatalf("active=%v settled=%v, want empty", active, settled)
	}
}

// newProjectionSession creates an empty session whose projections a test can fold.
func newProjectionSession(t *testing.T) *Session {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened, err := store.Create(t.TempDir(), "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return opened
}
