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
	for i := 0; i < n; i++ {
		if _, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
			"role": "user", "id": session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "question"}},
		}); err != nil {
			t.Fatalf("append user: %v", err)
		}
		if _, err := opened.AppendSurface(session.EventAssistantMessage, map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"id": session.NewID(), "role": "assistant",
				"content": []any{map[string]any{"type": "text", "text": "answer"}},
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
	opened := f.createSession(t)
	fillHistory(t, opened, 12)

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
	opened := f.createSession(t)
	fillHistory(t, opened, 12)
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
	opened := f.createSession(t)
	fillHistory(t, opened, 12)

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
	select {
	case <-m.release:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"done"}}]}` + "\n\n"))
	_, _ = w.Write([]byte("data\n"))
}

// newBlockingFixture builds a service whose model never answers on its own.
func newBlockingFixture(t *testing.T, release chan struct{}) *fixture {
	t.Helper()
	model := httptest.NewServer(&blockingModel{release: release})
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
