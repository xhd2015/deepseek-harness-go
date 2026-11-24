package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// fillHistory appends n user/assistant pairs so there is something to compact.
func fillHistory(t *testing.T, opened *session.Session, n int) {
	t.Helper()
	fillHistorySized(t, opened, n, "question", "answer")
}

// fillHistorySized writes n exchanges of a given size.
//
// The size matters now that the retained tail is measured in tokens: a handful of
// tiny messages fits inside any retain budget, so nothing is replaceable and a
// compaction is rightly a no-op. A test that wants compaction has to exceed the
// budget, which is what a real session under pressure looks like.
func fillHistorySized(t *testing.T, opened *session.Session, n int, question, answer string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
			"role": "user", "id": session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": question}},
		}); err != nil {
			t.Fatalf("append user: %v", err)
		}
		if _, err := opened.AppendSurface(session.EventAssistantMessage, map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"id": session.NewID(), "role": "assistant",
				"content": []any{map[string]any{"type": "text", "text": answer}},
			},
		}); err != nil {
			t.Fatalf("append assistant: %v", err)
		}
	}
}

// TestManualCompactWritesTheSameEventsAsAutomatic is the acceptance criterion: an
// idle session can be compacted on request and produces the four events.
func TestManualCompactWritesTheSameEventsAsAutomatic(t *testing.T) {
	f := newFixture(t)
	// A manual compaction measures its result against the window, so the fixture
	// has to declare one; a runner without a window refuses rather than run blind.
	// A window has to exist and be smaller than the history: a manual compaction
	// measures its result against it, and a retained tail that already fits means
	// the compaction is correctly a no-op.
	f.runner.SetCompactionWindow(8000)
	opened := f.createSession(t)
	fillPressured(t, opened)

	f.invoke(t, "session/compact", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)

	var kinds []string
	for _, event := range opened.Events() {
		switch event.Type {
		case session.EventCompactionStart, session.EventCompactionSummary, session.EventCompactionEnd:
			kinds = append(kinds, event.Type)
		}
	}
	if got := strings.Join(kinds, ","); got != "compaction/start,compaction/summary,compaction/end" {
		t.Fatalf("events = %q", got)
	}
}

// TestCompactReportsWhatHappened lets the page draw the state it just created.
func TestCompactReportsWhatHappened(t *testing.T) {
	f := newFixture(t)
	f.runner.SetCompactionWindow(8000)
	opened := f.createSession(t)
	fillPressured(t, opened)
	f.invoke(t, "session/compact", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)

	value := f.invoke(t, "session/compaction", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	var report struct {
		Compactions   []map[string]any `json:"compactions"`
		CompactedSeqs []int64          `json:"compactedSeqs"`
		Active        bool             `json:"active"`
		LastEndSeq    int64            `json:"lastEndSeq"`
	}
	if err := json.Unmarshal(value, &report); err != nil {
		t.Fatalf("decode %s: %v", value, err)
	}
	if len(report.Compactions) != 1 {
		t.Fatalf("compactions = %v", report.Compactions)
	}
	if len(report.CompactedSeqs) == 0 {
		t.Fatal("no compacted seqs were reported")
	}
	if report.Active {
		t.Fatal("the compaction is still open")
	}
	if report.LastEndSeq == 0 {
		t.Fatal("lastEndSeq was not reported")
	}
}

// TestCompactRefusesWhileRunning keeps a live turn's context intact: compacting it
// would replace events the model has already been shown this step.
func TestCompactRefusesWhileRunning(t *testing.T) {
	// A model that never answers keeps the turn open for the whole test.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	f := newBlockingFixture(t, block)
	opened := f.createSession(t)
	fillHistory(t, opened, 12)

	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)
	deadline := time.Now().Add(5 * time.Second)
	for !f.runner.Busy(opened.ID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.runner.Busy(opened.ID()) {
		t.Fatal("the turn never started")
	}

	f.invoke(t, "session/compact", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustFail(t, CodeBusy)
}

// TestCompactCommandRunsTheManualPath proves `/compact` is not a no-op: the line
// the page sends produces the same events, plus its own lifecycle.
func TestCompactCommandRunsTheManualPath(t *testing.T) {
	f := newFixture(t)
	f.runner.SetCompactionWindow(8000)
	opened := f.createSession(t)
	fillPressured(t, opened)

	value := f.invoke(t, "commands/execute", map[string]any{
		"sessionId": opened.ID(),
		"line":      "/compact",
	}).mustSucceed(t)
	var execution struct {
		CommandID string `json:"commandId"`
		Result    struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(value, &execution); err != nil {
		t.Fatalf("decode %s: %v", value, err)
	}
	if execution.Result.Kind != "success" {
		t.Fatalf("result = %+v", execution.Result)
	}
	if execution.CommandID == "" {
		t.Fatal("no command id was minted")
	}

	var sawRun, sawDone, sawSummary bool
	for _, event := range opened.Events() {
		switch event.Type {
		case session.EventCommandRun:
			sawRun = true
		case session.EventCommandDone:
			sawDone = true
		case session.EventCompactionSummary:
			sawSummary = true
		}
	}
	if !sawRun || !sawDone {
		t.Fatalf("command lifecycle = run:%v done:%v", sawRun, sawDone)
	}
	if !sawSummary {
		t.Fatal("/compact produced no compaction")
	}
}

// TestCompactCommandRejectsArguments pins the usage line the DSH command enforces.
func TestCompactCommandRejectsArguments(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	value := f.invoke(t, "commands/execute", map[string]any{
		"sessionId": opened.ID(),
		"line":      "/compact now",
	}).mustSucceed(t)
	var execution struct {
		Result struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(value, &execution); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if execution.Result.Kind != "error" || !strings.Contains(execution.Result.Text, "Usage") {
		t.Fatalf("result = %+v", execution.Result)
	}
}

// blockingModel holds its response open until released, so a test can observe the
// running state deterministically.
type blockingModel struct {
	release chan struct{}
}

func (m *blockingModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The block is bounded: a server's Close waits for every in-flight connection, so
	// a handler parked forever turns a test failure into a hung package. The window is
	// long enough that no test can outlive it by accident and short enough that a
	// leaked connection cannot stall the suite.
	select {
	case <-m.release:
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		return
	}
	// Every call is answered once the release closes, including a retry: the
	// executor retries a failed stream, and a handler that blocked again would keep
	// the server's Close waiting forever.
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"done"}}]}` + "\n\n"))
	_, _ = w.Write([]byte("data\n"))
}

// newBlockingFixture builds a service whose model never answers on its own.
func newBlockingFixture(t *testing.T, release chan struct{}) *fixture {
	t.Helper()
	model := httptest.NewServer(&blockingModel{release: release})
	// The server's Close waits for every in-flight connection, so the blocked
	// handler must be released first. Registering the release here, before Close,
	// makes it run after it under last-in-first-out — which is still too late.
	// The close is therefore registered in this order and the caller's channel is
	// drained by a finalizer that runs before Close.
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
		Store:    store,
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

// TestPruneReachesTheDisk proves the prune survives a process boundary: a second
// store over the same root, which can only read what was written, sees it. The
// log is compressed, so reading the file bytes would prove nothing.
func TestPruneReachesTheDisk(t *testing.T) {
	f := newFixture(t)
	// The manual path measures its result against a window, so one has to exist.
	f.runner.SetCompactionWindow(8000)
	opened := f.createSession(t)
	if _, err := opened.AppendSurface(session.EventToolResult, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"id": session.NewID(), "role": "tool", "toolCallId": "c1",
			"content": []any{map[string]any{"type": "text", "text": strings.Repeat("Z", 40000)}},
		},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.invoke(t, "session/compact", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)

	reader, err := session.NewStore(filepath.Join(f.root, "sessions"))
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	defer reader.Close()
	reopened, err := reader.OpenRead(opened.ID())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var prunes int
	for _, event := range reopened.Events() {
		if event.Type == session.EventCompactionPrune {
			prunes++
		}
	}
	if prunes == 0 {
		t.Fatal("a second process would not see the prune")
	}
}

// TestCompactSentAsPromptTextRunsTheCommand closes the second route a slash
// command takes: the composer sometimes sends `/compact` as an ordinary prompt,
// and treating it as chat text would hand the model a line it cannot execute.
func TestCompactSentAsPromptTextRunsTheCommand(t *testing.T) {
	f := newFixture(t)
	f.runner.SetCompactionWindow(8000)
	opened := f.createSession(t)
	fillPressured(t, opened)

	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "/compact"}},
		},
	}).mustSucceed(t)

	var sawRun, sawSummary, sawPromptMessage bool
	for _, event := range opened.Events() {
		switch event.Type {
		case session.EventCommandRun:
			sawRun = true
		case session.EventCompactionSummary:
			sawSummary = true
		case session.EventUserMessage:
			// The line must not also be recorded as something the person said.
			var payload struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && len(payload.Content) > 0 &&
				strings.TrimSpace(payload.Content[0].Text) == "/compact" {
				sawPromptMessage = true
			}
		}
	}
	if !sawRun {
		t.Fatal("/compact was not recorded as a command")
	}
	if !sawSummary {
		t.Fatal("/compact did not compact")
	}
	if sawPromptMessage {
		t.Fatal("the command line was also recorded as a chat message")
	}
}

// TestOrdinarySlashMessagesStillReachTheModel keeps the interception narrow: a
// message that merely starts with a slash is not a command.
func TestOrdinarySlashMessagesStillReachTheModel(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "/etc/hosts is broken, please look"}},
		},
	}).mustSucceed(t)
	for _, event := range opened.Events() {
		if event.Type == session.EventCommandRun {
			t.Fatal("an ordinary message was treated as a command")
		}
	}
}

// fillPressured writes a history that exceeds the fixture model's window, so a
// compaction has something to replace.
func fillPressured(t *testing.T, opened *session.Session) {
	t.Helper()
	fillHistorySized(t, opened, 16, strings.Repeat("q", 40000), strings.Repeat("a", 40000))
}
