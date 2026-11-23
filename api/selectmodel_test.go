package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// recordingModel answers one completion and records the model it was asked for.
type recordingModel struct {
	answer string
	gate   chan struct{}

	mu     sync.Mutex
	models []string
}

// seen reports the models asked for so far, safely.
func (m *recordingModel) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.models...)
}

func (m *recordingModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.models = append(m.models, body.Model)
	first := len(m.models) == 1
	m.mu.Unlock()
	if m.gate != nil && first {
		// The first request waits, so a test can switch the route while it runs.
		<-m.gate
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"` + m.answer + `"},"finish_reason":"stop"}]}` + "\n\n"))
	_, _ = w.Write([]byte("data\n"))
}

// TestSelectModelWhileRunningTakesEffectNextStep is the runtime contract: a swap
// mid-turn must not cut the step already committed, but the next step uses it.
func TestSelectModelWhileRunningTakesEffectNextStep(t *testing.T) {
	gate := make(chan struct{})
	first := &recordingModel{answer: "first", gate: gate}
	second := &recordingModel{answer: "second"}
	serverOne := httptest.NewServer(first)
	t.Cleanup(serverOne.Close)
	serverTwo := httptest.NewServer(second)
	t.Cleanup(serverTwo.Close)

	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Two routes, each with its own client: a swap has to change the client, not
	// just the recorded name.
	catalog := &modelconfig.Catalog{
		Default: modelconfig.Selection{Provider: "p", Model: "one"},
		Providers: []modelconfig.Provider{{
			ID: "p", Name: "p", Protocol: llm.ProtocolOpenAIChat,
			Models: []modelconfig.Model{{ID: "one", Name: "one", ContextWindow: 128000}, {ID: "two", Name: "two", ContextWindow: 128000}},
		}},
	}
	// The runner keys clients as "<provider>\x00<model>".
	clients := map[string]*llm.Client{
		"p\x00one": llm.New(llm.Config{BaseURL: serverOne.URL, Model: "one", APIKey: "k"}),
		"p\x00two": llm.New(llm.Config{BaseURL: serverTwo.URL, Model: "two", APIKey: "k"}),
	}
	runner := agent.New(agent.Config{
		Catalog: catalog, Clients: clients, Provider: "p", Model: "one", Store: store,
	})
	t.Cleanup(runner.Close)
	service := New(Config{
		Store: store, Runner: runner, Catalog: catalog,
		DefaultCwd: filepath.Join(root, "project"), AgentPreset: "std-no-subagent", HomeDir: root,
		ReloadModels: func(string) (*modelconfig.Catalog, map[string]*llm.Client, error) {
			return catalog, clients, nil
		},
	})
	f := &fixture{service: service, store: store, runner: runner, root: root}
	opened := f.createSession(t)

	// Start a turn that blocks on the model, then switch the route from outside.
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)
	deadline := time.Now().Add(5 * time.Second)
	for len(first.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(first.seen()) == 0 {
		t.Fatal("the first request never arrived")
	}

	var selected struct {
		Selected struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"selected"`
	}
	decode(t, f.invoke(t, "session/selectModel", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "provider": "p", "model": "two"},
	}).mustSucceed(t), &selected)
	if selected.Selected.Model != "two" {
		t.Fatalf("selected = %+v", selected.Selected)
	}
	// The swap is durable immediately, even while the turn runs.
	if got := lastSelectedModel(t, opened); got != "two" {
		t.Fatalf("model/selection = %q, want two", got)
	}
	close(gate)
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)

	// The step that was already committed used the old route; the next turn uses
	// the new one, which is what makes a mid-turn swap safe and still effective.
	if seen := first.seen(); len(seen) == 0 || seen[0] != "one" {
		t.Fatalf("the in-flight step used %v, want one", seen)
	}
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "again"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, opened, 2, 10*1000*1000*1000)
	if seen := second.seen(); len(seen) == 0 || seen[0] != "two" {
		t.Fatalf("the next turn used %v, want two", seen)
	}
}

// lastSelectedModel reads the newest recorded selection.
func lastSelectedModel(t *testing.T, opened *session.Session) string {
	t.Helper()
	model := ""
	for _, event := range opened.Events() {
		if event.Type != session.EventModelSelection {
			continue
		}
		var selection struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(event.Data, &selection) == nil {
			model = selection.Model
		}
	}
	return model
}
