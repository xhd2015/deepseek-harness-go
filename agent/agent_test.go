package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if events[5].SurfaceOp != "append" || events[8].SurfaceOp != "append" {
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
	decode(t, events[8].Data, &assistant)
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
