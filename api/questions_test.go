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
func waitForPendingQuestion(t *testing.T, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var mine []string
		for _, key := range agent.PendingQuestions() {
			if strings.HasPrefix(key, sessionID+"\x00") {
				mine = append(mine, key)
			}
		}
		if len(mine) == 1 {
			return mine[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no question became pending for %s: %v", sessionID, agent.PendingQuestions())
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
	// what this test waits on — and it must arrive without an answer.
	promptTurn(t, f, opened, "set up the service")
	waitForTurnEnd(t, opened, 10*time.Second)
	waitForPendingQuestion(t, opened.ID())

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

	// The record is isomorphic to DSH: the call, a result saying what happened, and
	// the person's later reply carrying the batch.
	call := findEvent(t, opened, session.EventToolCall)
	if !strings.Contains(string(call.Data), `"name":"ask_user_question"`) {
		t.Fatalf("tool call = %s", call.Data)
	}
	resultText, resultError := toolResultText(t, findEvent(t, opened, session.EventToolResult))
	if resultError {
		t.Fatalf("asking was recorded as a failure: %s", resultText)
	}
	var asked agent.AskedQuestionResult
	if err := json.Unmarshal([]byte(resultText), &asked); err != nil {
		t.Fatalf("decode ask result %q: %v", resultText, err)
	}
	if !asked.Asked || asked.CallID != "call-ask" {
		t.Fatalf("the call did not report that it asked: %+v", asked)
	}
	// The answer arrives after the call returned, so it is recorded as the message
	// DSH writes for a late reply — and it starts the turn that reads it.
	reply := findQuestionReply(t, opened)
	if reply == nil {
		t.Fatal("the answer was not recorded as a late reply message")
	}
	var replyPayload struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(reply.Data, &replyPayload); err != nil || len(replyPayload.Content) == 0 {
		t.Fatalf("decode reply: %v %s", err, reply.Data)
	}
	var carried struct {
		Answers []agent.QuestionAnswer `json:"answers"`
	}
	if err := json.Unmarshal([]byte(replyPayload.Content[0].Text), &carried); err != nil {
		t.Fatalf("decode reply payload %q: %v", replyPayload.Content[0].Text, err)
	}
	if len(carried.Answers) != 1 || len(carried.Answers[0].Selected) != 1 ||
		carried.Answers[0].Selected[0] != "standalone" {
		t.Fatalf("the reply lost the selection: %+v", carried.Answers)
	}
	if carried.Answers[0].Custom != "and keep the data dir" {
		t.Fatalf("the reply lost the custom text: %+v", carried.Answers[0])
	}
}

// TestAskUserQuestionCannotBeAnsweredTwice pins the must-not: a second submit
// must not hand the model two answers.
func TestAskUserQuestionCannotBeAnsweredTwice(t *testing.T) {
	question := `{"questions":[{"id":"q","question":"Continue?"}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	promptTurn(t, f, opened, "go")
	waitForPendingQuestion(t, opened.ID())

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

// waitForTurnEnd blocks until the session has recorded at least `want` turn ends.
//
// Counting rather than "any turn end" is what lets a test tell the turn that asked
// the question from the turn the answer started: both end the same way, and only
// the count says which one arrived.
func waitForTurnEnd(t *testing.T, opened *session.Session, budget time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ends := countTurnEnds(opened); ends > 0 {
			return ends
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the turn never ended")
	return 0
}

// waitForTurnEnds blocks until the session has recorded `want` turn ends.
func waitForTurnEnds(t *testing.T, opened *session.Session, want int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if countTurnEnds(opened) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d turn ends were recorded, want %d", countTurnEnds(opened), want)
}

// countTurnEnds reports how many turns this session has closed.
func countTurnEnds(opened *session.Session) int {
	total := 0
	for _, event := range opened.Events() {
		if event.Type == session.EventTurnEnd {
			total++
		}
	}
	return total
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

// TestAskUserQuestionDoesNotHoldTheTurn is the corrected requirement: asking must
// not keep the session running. A person may answer in a minute or a day, and a
// backend that waits for them leaves the board showing a live turn that is doing
// nothing.
func TestAskUserQuestionDoesNotHoldTheTurn(t *testing.T) {
	question := `{"questions":[{"id":"q","question":"Ship it?","options":[{"label":"yes"},{"label":"no"}]}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	promptTurn(t, f, opened, "ask me")
	waitForTurnEnd(t, opened, 5*time.Second)

	// The session is free again: no turn is running while the question stands.
	// A busy session after turn/end means the turn never really closed.
	if opened.Busy() {
		t.Fatal("the session is still busy after the ask turn ended")
	}
	if f.runner.Busy(opened.ID()) {
		t.Fatal("the runner still reports a live turn after the ask turn ended")
	}

	// The result says the question was asked, not that it was answered or skipped.
	result := findEvent(t, opened, session.EventToolResult)
	text, isError := toolResultText(t, result)
	if isError {
		t.Fatalf("asking was recorded as a failure: %s", text)
	}
	var asked agent.AskedQuestionResult
	if err := json.Unmarshal([]byte(text), &asked); err != nil {
		t.Fatalf("decode ask result %q: %v", text, err)
	}
	if !asked.Asked || asked.CallID != "call-ask" || asked.Message == "" {
		t.Fatalf("ask result = %+v", asked)
	}
}

// TestAskUserQuestionAnsweredLongAfterTheTurn proves the answer is still accepted
// once the turn is long gone, and that it starts a new turn which reads it.
func TestAskUserQuestionAnsweredLongAfterTheTurn(t *testing.T) {
	question := `{"questions":[{"id":"q","question":"Ship it?","options":[{"label":"yes"}]}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	promptTurn(t, f, opened, "ask me")
	waitForTurnEnd(t, opened, 5*time.Second)
	waitForPendingQuestion(t, opened.ID())

	f.invoke(t, "userQuestions/answer", map[string]any{
		"sessionId": opened.ID(),
		"callId":    "call-ask",
		"answer":    map[string]any{"answers": []map[string]any{{"id": "q", "selected": []string{"yes"}}}},
	}).mustSucceed(t)

	reply := waitForQuestionReply(t, opened, 5*time.Second)
	if !strings.Contains(string(reply.Data), `"kind":"user-question-reply"`) ||
		!strings.Contains(string(reply.Data), `"callId":"call-ask"`) ||
		!strings.Contains(string(reply.Data), `"outcome":"answered"`) {
		t.Fatalf("late answer source = %s", reply.Data)
	}
	if !strings.Contains(string(reply.Data), "answer_to_pending_question") {
		t.Fatalf("late answer payload = %s", reply.Data)
	}
	// The reply starts a second turn, which must end normally too.
	waitForTurnEnds(t, opened, 2, 10*time.Second)
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

// TestQuestionAppearsInTheServedProjection is the page's own path.
//
// The browser renders the question card from session/projections, not from the
// event log, so the contract is what that endpoint answers: an ask whose call has
// returned must be listed as a continued question. A backend that folds the log
// correctly but omits the face leaves the user with no way to answer.
func TestQuestionAppearsInTheServedProjection(t *testing.T) {
	question := `{"questions":[{"id":"color","question":"Which color?","header":"Pick one",` +
		`"options":[{"label":"Blue","description":"calm"},{"label":"Green"}],"multi_select":true}]}`
	f := newAskFixture(t, question)
	opened := f.createSession(t)

	promptTurn(t, f, opened, "ask me")
	waitForTurnEnd(t, opened, 5*time.Second)

	value := f.invoke(t, "session/projections", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	var payload struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(value, &payload); err != nil {
		t.Fatalf("decode projections: %v", err)
	}
	raw, ok := payload.Values["userQuestions"]
	if !ok {
		t.Fatal("the served projections have no userQuestions face")
	}
	var face struct {
		Active []struct {
			CallID    string `json:"callId"`
			State     string `json:"state"`
			Questions []struct {
				ID          string `json:"id"`
				Question    string `json:"question"`
				Header      string `json:"header"`
				MultiSelect bool   `json:"multiSelect"`
				Options     []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
			} `json:"questions"`
		} `json:"active"`
	}
	if err := json.Unmarshal(raw, &face); err != nil {
		t.Fatalf("decode face %s: %v", raw, err)
	}
	if len(face.Active) != 1 {
		t.Fatalf("active = %s, want the one open question", raw)
	}
	entry := face.Active[0]
	if entry.CallID != "call-ask" || entry.State != "continued" {
		t.Fatalf("active entry = %+v", entry)
	}
	if len(entry.Questions) != 1 || entry.Questions[0].ID != "color" || !entry.Questions[0].MultiSelect {
		t.Fatalf("questions = %+v", entry.Questions)
	}
	if len(entry.Questions[0].Options) != 2 || entry.Questions[0].Options[0].Description != "calm" {
		t.Fatalf("options = %+v", entry.Questions[0].Options)
	}
}
