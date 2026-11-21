package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/session"
)

type fakeModel struct {
	mu       sync.Mutex
	prompts  [][]llm.Message
	answer   string
	status   int
	block    chan struct{}
	release  chan struct{}
	requests int
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []llm.Message `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.prompts = append(f.prompts, body.Messages)
	f.requests++
	status, block, release, answer := f.status, f.block, f.release, f.answer
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"fake failure"}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"model":"fake-model","choices":[{"delta":{"content":` + quote(answer) + `}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (f *fakeModel) snapshot() (int, []llm.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return f.requests, nil
	}
	return f.requests, f.prompts[len(f.prompts)-1]
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func newTestAgent(t *testing.T, fake *fakeModel) (*Runner, *session.Session) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened, err := store.Create(t.TempDir(), "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	runner := New(Config{
		Client:   llm.New(llm.Config{BaseURL: server.URL, Model: "fake-model", APIKey: "k"}),
		Provider: "fake-provider",
		Model:    "fake-model",
	})
	t.Cleanup(runner.Close)
	return runner, opened
}

func TestRunRecordsTheFullTurn(t *testing.T) {
	fake := &fakeModel{answer: "Hello from the fake model."}
	runner, opened := newTestAgent(t, fake)

	if !runner.Start(context.Background(), PromptRequest{
		Session: opened, Content: []ContentPart{{Type: "text", Text: "hi there\nsecond line"}},
		RPCID: "rpc-1", ClientTimeZone: "Asia/Shanghai",
	}) {
		t.Fatal("Start reported the session was busy")
	}
	waitIdle(t, runner, opened)

	events := opened.Events()
	wantTypes := []string{
		session.EventPermissionPreset,
		session.EventSandboxMode,
		session.EventApprovalPolicy,
		session.EventModelSelection,
		session.EventTurnStart,
		session.EventUserMessage,
		session.EventSessionTitle,
		session.EventStepStart,
		session.EventRequestHeader,
		session.EventAssistantMessage,
		session.EventStepEnd,
		session.EventTurnEnd,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("recorded %d events: %v", len(events), typesOf(events))
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d = %s, want %s (order %v)", i, events[i].Type, want, typesOf(events))
		}
	}
	for i, event := range events {
		if event.Seq != int64(i) || event.Time <= 0 {
			t.Fatalf("event %d has seq %d time %d", i, event.Seq, event.Time)
		}
	}
	if events[5].SurfaceOp == nil || events[9].SurfaceOp == nil {
		t.Fatalf("messages are not surface events: %+v", events)
	}

	// The user message carries the raw text parts and the rpc metadata.
	var user struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Source map[string]any `json:"source"`
		Role   string         `json:"role"`
		ID     string         `json:"id"`
	}
	decode(t, events[5].Data, &user)
	if len(user.Content) != 1 || user.Content[0].Text != "hi there\nsecond line" {
		t.Fatalf("user content = %+v", user.Content)
	}
	if user.Role != "user" || user.ID == "" || user.Source["kind"] != "user" || user.Source["rpcId"] != "rpc-1" {
		t.Fatalf("user message = %+v", user)
	}

	// The assistant message must satisfy the client-side settlement checks:
	// data.stream is a top-level array and the source names provider + model.
	var assistant struct {
		Turn    int `json:"turn"`
		Step    int `json:"step"`
		Stream  []any
		Usage   map[string]int `json:"usage"`
		Message struct {
			Role    string `json:"role"`
			ID      string `json:"id"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Source struct {
				Kind     string `json:"kind"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"source"`
		} `json:"message"`
	}
	decode(t, events[9].Data, &assistant)
	if assistant.Turn != 1 || assistant.Step != 1 {
		t.Fatalf("assistant turn/step = %d/%d", assistant.Turn, assistant.Step)
	}
	if assistant.Stream == nil {
		t.Fatal("assistant data.stream is missing")
	}
	if assistant.Message.Role != "assistant" || assistant.Message.ID == "" {
		t.Fatalf("assistant message = %+v", assistant.Message)
	}
	if assistant.Message.Source.Kind != "model" || assistant.Message.Source.Provider != "fake-provider" || assistant.Message.Source.Model != "fake-model" {
		t.Fatalf("assistant source = %+v", assistant.Message.Source)
	}
	if len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != fake.answer {
		t.Fatalf("assistant content = %+v", assistant.Message.Content)
	}
	if assistant.Usage["inputTokens"] != 11 || assistant.Usage["outputTokens"] != 7 || assistant.Usage["totalTokens"] != 18 {
		t.Fatalf("assistant usage = %+v", assistant.Usage)
	}

	// Title falls back to the first prompt line, truncated to one line.
	var title struct {
		Title  string `json:"title"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	decode(t, events[6].Data, &title)
	if title.Title != "hi there" || title.Source.Kind != "fallback" {
		t.Fatalf("title = %+v", title)
	}

	requests, prompt := fake.snapshot()
	if requests != 1 {
		t.Fatalf("model calls = %d", requests)
	}
	if len(prompt) != 2 || prompt[0].Role != "system" || prompt[1].Content != "hi there\nsecond line" {
		t.Fatalf("prompt = %+v", prompt)
	}
}

func TestSecondTurnReusesPreambleAndCarriesHistory(t *testing.T) {
	fake := &fakeModel{answer: "first answer"}
	runner, opened := newTestAgent(t, fake)

	runner.Start(context.Background(), PromptRequest{Session: opened, Content: []ContentPart{{Type: "text", Text: "one"}}})
	waitIdle(t, runner, opened)
	runner.Start(context.Background(), PromptRequest{Session: opened, Content: []ContentPart{{Type: "text", Text: "two"}}})
	waitIdle(t, runner, opened)

	events := opened.Events()
	presets := 0
	for _, event := range events {
		if event.Type == session.EventPermissionPreset {
			presets++
		}
	}
	if presets != 1 {
		t.Fatalf("preamble written %d times", presets)
	}
	var turns []int
	for _, event := range events {
		if event.Type != session.EventTurnStart {
			continue
		}
		var data struct {
			Turn int `json:"turn"`
		}
		decode(t, event.Data, &data)
		turns = append(turns, data.Turn)
	}
	if len(turns) != 2 || turns[0] != 1 || turns[1] != 2 {
		t.Fatalf("turns = %v", turns)
	}
	_, prompt := fake.snapshot()
	if len(prompt) != 4 {
		t.Fatalf("second prompt has %d messages: %+v", len(prompt), prompt)
	}
	if prompt[1].Role != "user" || prompt[1].Content != "one" {
		t.Fatalf("prompt[1] = %+v", prompt[1])
	}
	if prompt[2].Role != "assistant" || prompt[2].Content != "first answer" {
		t.Fatalf("prompt[2] = %+v", prompt[2])
	}
	if prompt[3].Role != "user" || prompt[3].Content != "two" {
		t.Fatalf("prompt[3] = %+v", prompt[3])
	}
}

func TestCancelAbortsTheTurn(t *testing.T) {
	fake := &fakeModel{answer: "never", block: make(chan struct{})}
	runner, opened := newTestAgent(t, fake)
	defer close(fake.block)

	runner.Start(context.Background(), PromptRequest{Session: opened, Content: []ContentPart{{Type: "text", Text: "slow"}}})
	waitFor(t, func() bool { return runner.Busy(opened.ID()) && opened.Busy() })
	if runner.Start(context.Background(), PromptRequest{Session: opened, Content: []ContentPart{{Type: "text", Text: "again"}}}) {
		t.Fatal("Start accepted a second concurrent turn")
	}
	if !runner.Cancel(opened.ID()) {
		t.Fatal("Cancel reported no running turn")
	}
	waitIdle(t, runner, opened)

	events := opened.Events()
	last := events[len(events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %s", last.Type)
	}
	var end struct {
		Reason struct {
			Kind   string `json:"kind"`
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		} `json:"reason"`
	}
	decode(t, last.Data, &end)
	if end.Reason.Kind != "aborted" || end.Reason.Reason.Kind != "user" {
		t.Fatalf("turn end reason = %+v", end.Reason)
	}
}

func TestFailureEndsTheTurnWithAnError(t *testing.T) {
	fake := &fakeModel{status: http.StatusBadRequest}
	runner, opened := newTestAgent(t, fake)

	runner.Start(context.Background(), PromptRequest{Session: opened, Content: []ContentPart{{Type: "text", Text: "boom"}}})
	waitIdle(t, runner, opened)

	events := opened.Events()
	last := events[len(events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %s", last.Type)
	}
	var end struct {
		Reason struct {
			Kind  string `json:"kind"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"reason"`
	}
	decode(t, last.Data, &end)
	if end.Reason.Kind != "error" || end.Reason.Error.Code == "" || end.Reason.Error.Message == "" {
		t.Fatalf("turn end reason = %+v", end.Reason)
	}
}

func TestSetModelRejectsUnknownRoutes(t *testing.T) {
	fake := &fakeModel{answer: "ok"}
	runner, opened := newTestAgent(t, fake)
	runner.Start(context.Background(), PromptRequest{Session: opened, Content: []ContentPart{{Type: "text", Text: "hi"}}})
	waitIdle(t, runner, opened)

	// The runner keeps its configured route; this test only pins the accessors
	// the API layer exposes for the model catalog.
	if runner.Provider() != "fake-provider" || runner.Model() != "fake-model" {
		t.Fatalf("route = %s/%s", runner.Provider(), runner.Model())
	}
	if runner.ReasoningEffort() != "" {
		t.Fatalf("reasoning effort = %q", runner.ReasoningEffort())
	}
}

func waitIdle(t *testing.T, runner *Runner, opened *session.Session) {
	t.Helper()
	waitFor(t, func() bool { return !runner.Busy(opened.ID()) })
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func decode(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func typesOf(events []session.Event) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.Type
	}
	return out
}

// TestHistoryAppliesMessageEdits proves the model sees rewritten text and not
// deleted messages, while the log keeps every original row.
func TestHistoryAppliesMessageEdits(t *testing.T) {
	fake := &fakeModel{answer: "ok"}
	runner, opened := newTestAgent(t, fake)
	user, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
		"role":    "user",
		"id":      session.NewID(),
		"content": []any{map[string]any{"type": "text", "text": "original question"}},
	})
	if err != nil {
		t.Fatalf("append user: %v", err)
	}
	reply, err := opened.AppendSurface(session.EventAssistantMessage, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"role":    "assistant",
			"id":      session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "reply that should disappear"}},
			"source":  map[string]any{"kind": "model"},
		},
	})
	if err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	if _, err := opened.Append(session.EventMessageEdit, map[string]any{
		"edits": []map[string]any{
			{"seq": user.Seq, "text": "corrected question"},
			{"seq": reply.Seq, "deleted": true},
		},
	}); err != nil {
		t.Fatalf("append edit: %v", err)
	}

	messages := runner.history(opened)
	if len(messages) != 2 {
		t.Fatalf("history has %d messages, want system plus the edited user message: %+v", len(messages), messages)
	}
	if messages[0].Role != "system" {
		t.Fatalf("first message role = %q", messages[0].Role)
	}
	if messages[1].Content != "corrected question" {
		t.Fatalf("user content = %q, want the rewritten text", messages[1].Content)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "reply that should disappear") {
			t.Fatal("a deleted message is still in model history")
		}
	}
	// The durable log is unchanged: the removal is a projection, not a rewrite.
	if got := len(opened.Events()); got != 3 {
		t.Fatalf("log has %d events, want the two messages and the edit record", got)
	}
}

// TestHistorySkipsEditsForVanishedNodes proves an edit naming a seq that is no
// longer a current node is skipped instead of breaking the whole history.
func TestHistorySkipsEditsForVanishedNodes(t *testing.T) {
	fake := &fakeModel{answer: "ok"}
	runner, opened := newTestAgent(t, fake)
	if _, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
		"role":    "user",
		"id":      session.NewID(),
		"content": []any{map[string]any{"type": "text", "text": "keep me"}},
	}); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if _, err := opened.Append(session.EventMessageEdit, map[string]any{
		"edits": []map[string]any{
			{"seq": 424242, "text": "vanished"},
			{"seq": 424243, "deleted": true},
		},
	}); err != nil {
		t.Fatalf("append edit: %v", err)
	}
	messages := runner.history(opened)
	if len(messages) != 2 || messages[1].Content != "keep me" {
		t.Fatalf("history = %+v", messages)
	}
}

// TestPanickingTurnEndsWithAnError proves a fault inside one turn costs that
// turn only: the log still reaches a terminal event with a failure the client
// can render, instead of the process dying with the turn left open.
func TestPanickingTurnEndsWithAnError(t *testing.T) {
	fake := &fakeModel{answer: "unused"}
	runner, opened := newTestAgent(t, fake)

	func() {
		defer runner.recoverTurn(opened, 7)
		panic("decoder exploded")
	}()

	events := opened.Events()
	if len(events) == 0 {
		t.Fatal("a panicking turn wrote nothing")
	}
	last := events[len(events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %s, want %s", last.Type, session.EventTurnEnd)
	}
	var data struct {
		Turn   int `json:"turn"`
		Reason struct {
			Kind  string `json:"kind"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatalf("turn end data = %s: %v", last.Data, err)
	}
	if data.Turn != 7 || data.Reason.Kind != "error" {
		t.Fatalf("turn end = %+v", data)
	}
	if data.Reason.Error.Code != "PANIC" || !strings.Contains(data.Reason.Error.Message, "decoder exploded") {
		t.Fatalf("failure = %+v", data.Reason.Error)
	}
	// The session is usable again rather than stuck busy.
	if opened.Busy() {
		t.Fatal("session stayed busy after the recovered turn")
	}
}

// scriptedModel answers each request with the next scripted SSE body, so a test
// can drive a real multi-step turn: a tool call first, then whatever follows.
type scriptedModel struct {
	mu     sync.Mutex
	bodies []string
	step   int
}

func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	index := m.step
	if index >= len(m.bodies) {
		index = len(m.bodies) - 1
	}
	m.step++
	body := m.bodies[index]
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(body))
}

func (m *scriptedModel) steps() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.step
}

// TestTurnRunsASecondToolCallingStep is the end-to-end form of the reported
// failure: step 1 writes a file through a tool call, step 2 answers with nothing
// but tool calls. A decoder that only survives the first shape kills the
// process here.
func TestTurnRunsASecondToolCallingStep(t *testing.T) {
	workdir := t.TempDir()
	model := &scriptedModel{bodies: []string{
		// Step 1: a tool call with no prose before it.
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-w","type":"function","function":{"name":"write","arguments":"{\"file_path\":\"probe.txt\",\"content\":\"probe\"}"}}]}}]}` + "\n\n" +
			"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			"data: [DONE]\n\n",
		// Step 2: again no prose, and two tool calls in one response.
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-e","type":"function","function":{"name":"edit","arguments":"{\"file_path\":\"probe.txt\",\"old_string\":\"probe\",\"new_string\":\"probed\"}"}}]}}]}` + "\n\n" +
			"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call-b","type":"function","function":{"name":"bash","arguments":"{\"command\":\"cat probe.txt\"}"}}]}}]}` + "\n\n" +
			"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			"data: [DONE]\n\n",
		// Step 3: the closing answer.
		"data: " + `{"choices":[{"delta":{"content":"done"}}]}` + "\n\n" +
			"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n",
	}}
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened, err := store.Create(workdir, "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	runner := New(Config{
		Client:   llm.New(llm.Config{BaseURL: server.URL, Model: "fake-model", APIKey: "k"}),
		Provider: "fake-provider",
		Model:    "fake-model",
	})
	t.Cleanup(runner.Close)

	if !runner.Start(context.Background(), PromptRequest{
		Session: opened, Content: []ContentPart{{Type: "text", Text: "write, edit and read the probe"}},
		RPCID: "rpc-tools",
	}) {
		t.Fatal("Start reported the session was busy")
	}
	waitIdle(t, runner, opened)

	if model.steps() != 3 {
		t.Fatalf("model was called %d times, want 3", model.steps())
	}
	// The file was written and then edited, which only happens if both steps
	// were dispatched.
	content, err := os.ReadFile(filepath.Join(workdir, "probe.txt"))
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if string(content) != "probed" {
		t.Fatalf("probe.txt = %q, want the edited content", content)
	}
	events := opened.Events()
	last := events[len(events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %s, want %s", last.Type, session.EventTurnEnd)
	}
	var end struct {
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(last.Data, &end); err != nil {
		t.Fatalf("turn end: %v", err)
	}
	if end.Reason.Kind != "done" && end.Reason.Kind != "stop" && end.Reason.Kind != "completed" {
		t.Fatalf("turn end reason = %+v", end.Reason)
	}
	for _, event := range events {
		if event.Type == session.EventTurnEnd {
			var data struct {
				Reason struct {
					Kind  string `json:"kind"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				} `json:"reason"`
			}
			_ = json.Unmarshal(event.Data, &data)
			if data.Reason.Kind == "error" {
				t.Fatalf("turn ended with an error: %s", event.Data)
			}
		}
	}
}
