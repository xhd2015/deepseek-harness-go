package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// askModel answers the first request with an ask_user_question call and the next
// one with plain text, the way a real model behaves when it wants a decision
// before it can continue.
type askModel struct {
	mu    sync.Mutex
	calls int
	// question is the JSON arguments the tool call carries.
	question string
}

func (m *askModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.calls++
	first := m.calls == 1
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	if first {
		args, _ := json.Marshal(m.question)
		frame := `{"model":"fake-model","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-ask","type":"function","function":{"name":"ask_user_question","arguments":` + string(args) + `}}]}}]}` + "\n\n"
		_, _ = w.Write([]byte("data: " + frame))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
	} else {
		answer, _ := json.Marshal("thanks, continuing")
		_, _ = w.Write([]byte(`data: {"model":"fake-model","choices":[{"delta":{"content":` + string(answer) + `}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

// newAskFixture builds a service whose model asks one question.
func newAskFixture(t *testing.T, question string) *fixture {
	t.Helper()
	model := httptest.NewServer(&askModel{question: question})
	t.Cleanup(model.Close)
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runner := agent.New(agent.Config{
		Client:   llm.New(llm.Config{BaseURL: model.URL, Model: "fake-model", APIKey: "k"}),
		Provider: "fake-provider",
		Model:    "fake-model",
	})
	t.Cleanup(runner.Close)
	service := New(Config{
		Store:       store,
		Runner:      runner,
		DefaultCwd:  filepath.Join(root, "project"),
		AgentPreset: "std-no-subagent",
		HomeDir:     root,
	})
	return &fixture{service: service, store: store, runner: runner, root: root}
}

// waitForPendingQuestion waits until the ask is registered, without sleeping a
// fixed amount: the turn is asynchronous, and a fixed sleep would make this test
// report the machine's load instead of the code's behaviour.
func waitForPendingQuestion(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending := agent.PendingQuestions(); len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no question became pending: %v", agent.PendingQuestions())
	return ""
}

// TestAskUserQuestionRoundTrip is the acceptance criterion: the model asks, the
// user answers, the answer reaches the model as that call's result, and the same
// turn continues to a normal end.
func TestAskUserQuestionRoundTrip(t *testing.T) {
	question := `{"questions":[{"id":"mode","question":"Which mode should the service use?","header":"Choose Mode",` +
		`"options":[{"label":"embed (Recommended)","description":"No extra port"},{"label":"standalone"}],"multi_select":false}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	// session/prompt answers as soon as the turn starts, so the turn's own end is
	// what this test waits on.
	promptTurn(t, f, opened, "set up the service")
	waitForPendingQuestion(t)

	answered := f.invoke(t, "userQuestions/answer", map[string]any{
		"sessionId": opened.ID(),
		"callId":    "call-ask",
		"answer": map[string]any{
			"answers": []map[string]any{{
				"id":       "mode",
				"selected": []string{"standalone"},
				"custom":   "and keep the data dir",
			}},
		},
	})
	answered.mustSucceed(t)

	waitForTurnEnd(t, opened, 10*time.Second)

	// The turn ran to a normal end, which is what "继续 turn" means.
	if end := lastTurnEnd(t, opened); end != session.TurnCompleted {
		t.Fatalf("turn ended as %q, want %q", end, session.TurnCompleted)
	}

	// The record is isomorphic to DSH: the call, the person's reply naming it, and
	// the result carrying the batch.
	call := findEvent(t, opened, session.EventToolCall)
	if !strings.Contains(string(call.Data), `"name":"ask_user_question"`) {
		t.Fatalf("tool call = %s", call.Data)
	}
	// A real DSH session records the answer in the tool result and nowhere else:
	// the turn never ended, so there is no user message to write. Asserting the
	// absence keeps this backend from growing a record Node DSH would not have.
	if reply := findQuestionReply(t, opened); reply != nil {
		t.Fatalf("the answer was recorded as a user message DSH does not write: %s", reply.Data)
	}
	result := findEvent(t, opened, session.EventToolResult)
	if !strings.Contains(string(result.Data), `"callId":"call-ask"`) {
		t.Fatalf("tool result does not answer the call: %s", result.Data)
	}
	if !strings.Contains(string(result.Data), `"selected":["standalone"]`) {
		t.Fatalf("tool result lost the selection: %s", result.Data)
	}
	if !strings.Contains(string(result.Data), `"custom":"and keep the data dir"`) {
		t.Fatalf("tool result lost the custom text: %s", result.Data)
	}
}

// TestAskUserQuestionCannotBeAnsweredTwice pins the must-not: a second submit
// must not hand the model two answers.
func TestAskUserQuestionCannotBeAnsweredTwice(t *testing.T) {
	question := `{"questions":[{"id":"q","question":"Continue?"}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	promptTurn(t, f, opened, "go")
	waitForPendingQuestion(t)

	args := map[string]any{
		"sessionId": opened.ID(),
		"callId":    "call-ask",
		"answer":    map[string]any{"answers": []map[string]any{{"id": "q", "selected": []string{"yes"}}}},
	}
	f.invoke(t, "userQuestions/answer", args).mustSucceed(t)
	f.invoke(t, "userQuestions/answer", args).mustFail(t, CodeBusy)

	waitForTurnEnd(t, opened, 10*time.Second)
}

// TestAskUserQuestionAnswerWithNoWaitingQuestion fails loudly: the browser must
// learn its answer arrived too late, not receive a success it cannot rely on.
func TestAskUserQuestionAnswerWithNoWaitingQuestion(t *testing.T) {
	f := newAskFixture(t, `{"questions":[{"id":"q","question":"x"}]}`)
	opened := f.createSession(t)
	f.invoke(t, "userQuestions/answer", map[string]any{
		"sessionId": opened.ID(),
		"callId":    "call-never-asked",
		"answer":    map[string]any{"answers": []map[string]any{{"id": "q", "selected": []string{"a"}}}},
	}).mustFail(t, CodeBusy)
}

// TestAskUserQuestionIsOfferedToTheModel is the gap the verification agent
// measured: without this tool in the definitions the model can only answer
// NO_ASK_TOOL.
func TestAskUserQuestionIsOfferedToTheModel(t *testing.T) {
	executor := agent.NewToolExecutor(t.TempDir())
	found := false
	for _, def := range executor.Definitions() {
		if def.Name != "ask_user_question" {
			continue
		}
		found = true
		if !strings.Contains(def.Description, "Ask the user") {
			t.Fatalf("description = %q", def.Description)
		}
		encoded, err := json.Marshal(def.Parameters)
		if err != nil {
			t.Fatalf("marshal parameters: %v", err)
		}
		// The field names the card reads; renaming one degrades the UI silently.
		for _, field := range []string{`"questions"`, `"id"`, `"question"`, `"header"`, `"options"`, `"label"`, `"multi_select"`} {
			if !strings.Contains(string(encoded), field) {
				t.Fatalf("parameters are missing %s: %s", field, encoded)
			}
		}
	}
	if !found {
		t.Fatal("ask_user_question is not offered to the model")
	}
}

// promptTurn starts one turn and asserts it was accepted.
func promptTurn(t *testing.T, f *fixture, opened *session.Session, text string) {
	t.Helper()
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": text}},
		},
	}).mustSucceed(t)
}

// waitForTurnEnd blocks until the session records another turn/end.
func waitForTurnEnd(t *testing.T, opened *session.Session, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		for _, event := range opened.Events() {
			if event.Type == session.EventTurnEnd {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the turn never ended")
}

// lastTurnEnd reports how the session's last turn ended.
func lastTurnEnd(t *testing.T, opened *session.Session) string {
	t.Helper()
	events := opened.Events()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != session.EventTurnEnd {
			continue
		}
		var payload struct {
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		}
		if err := json.Unmarshal(events[i].Data, &payload); err != nil {
			t.Fatalf("decode turn end: %v", err)
		}
		return payload.Reason.Kind
	}
	t.Fatal("no turn/end was recorded")
	return ""
}

// findEvent returns the first event of one type.
func findEvent(t *testing.T, opened *session.Session, typ string) session.Event {
	t.Helper()
	for _, event := range opened.Events() {
		if event.Type == typ {
			return event
		}
	}
	t.Fatalf("no %s event", typ)
	return session.Event{}
}

// findQuestionReply returns the user message that carries the answer.
func findQuestionReply(t *testing.T, opened *session.Session) *session.Event {
	t.Helper()
	for _, event := range opened.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		if strings.Contains(string(event.Data), "user-question-reply") {
			found := event
			return &found
		}
	}
	return nil
}

// TestAskUserQuestionWindowExpiresThenLateAnswerContinues pins the second shape:
// a question nobody answered in time must not hold the turn, and the answer that
// arrives afterwards must still reach the model.
//
// This is also what makes the question answerable from a page: the session page's
// question projection only tracks calls whose schema declares a wait window, so a
// call without one would never surface an answerable card.
func TestAskUserQuestionWindowExpiresThenLateAnswerContinues(t *testing.T) {
	// A one-second window keeps the test honest without a long sleep.
	question := `{"timeout":1,"questions":[{"id":"q","question":"Ship it?","options":[{"label":"yes"},{"label":"no"}]}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	promptTurn(t, f, opened, "ask me")
	waitForPendingQuestion(t)

	// The first turn must end on its own, with the pending result recorded.
	waitForTurnEnd(t, opened, 10*time.Second)
	result := findEvent(t, opened, session.EventToolResult)
	text, isError := toolResultText(t, result)
	if isError {
		// A pending result is not a failure: the model must continue, not retry.
		t.Fatalf("a pending question was recorded as an error: %s", text)
	}
	var pending agent.PendingQuestionResult
	if err := json.Unmarshal([]byte(text), &pending); err != nil {
		t.Fatalf("decode pending result %q: %v", text, err)
	}
	if !pending.Pending || pending.CallID != "call-ask" || pending.Message == "" {
		t.Fatalf("pending result = %+v", pending)
	}

	// The ask is still answerable, so the late answer becomes a message and runs
	// the turn that reads it.
	f.invoke(t, "userQuestions/answer", map[string]any{
		"sessionId": opened.ID(),
		"callId":    "call-ask",
		"answer":    map[string]any{"answers": []map[string]any{{"id": "q", "selected": []string{"yes"}}}},
	}).mustSucceed(t)

	reply := waitForQuestionReply(t, opened, 5*time.Second)
	if !strings.Contains(string(reply.Data), `"kind":"user-question-reply"`) {
		t.Fatalf("late answer source = %s", reply.Data)
	}
	if !strings.Contains(string(reply.Data), `"callId":"call-ask"`) {
		t.Fatalf("late answer lost its call id: %s", reply.Data)
	}
	if !strings.Contains(string(reply.Data), `"outcome":"answered"`) {
		t.Fatalf("late answer outcome = %s", reply.Data)
	}
	if !strings.Contains(string(reply.Data), "answer_to_pending_question") {
		t.Fatalf("late answer payload = %s", reply.Data)
	}
	// And the turn it started reached a normal end.
	waitForTurnEnd(t, opened, 10*time.Second)
}

// toolResultText returns a tool result's text and failure flag.
func toolResultText(t *testing.T, event session.Event) (string, bool) {
	t.Helper()
	var payload struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"message"`
	}
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(payload.Message.Content) == 0 {
		t.Fatalf("tool result has no content: %s", event.Data)
	}
	return payload.Message.Content[0].Text, payload.Message.IsError
}

// waitForQuestionReply waits for the user message that carries a late answer.
func waitForQuestionReply(t *testing.T, opened *session.Session, budget time.Duration) *session.Event {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if reply := findQuestionReply(t, opened); reply != nil {
			return reply
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the late answer was never recorded as a message")
	return nil
}
