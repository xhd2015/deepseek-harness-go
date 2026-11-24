package agent

import (
	"context"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// budgetCatalog is two models with different windows, so a test can tell which one
// resolved: the deployment default is deliberately larger than the session's model.
func budgetCatalog() *modelconfig.Catalog {
	return &modelconfig.Catalog{
		Default: modelconfig.Selection{Provider: "big", Model: "big-model"},
		Providers: []modelconfig.Provider{
			{ID: "big", Models: []modelconfig.Model{{ID: "big-model", ContextWindow: 200000}}},
			{ID: "small", Models: []modelconfig.Model{{ID: "small-model", ContextWindow: 32000}}},
		},
	}
}

// TestBudgetFollowsTheSessionModel is the bug that made a "successful" compaction
// useless: the window was resolved once at boot from the catalog default, so a
// session routed to a smaller model was compacted against the wrong capacity.
func TestBudgetFollowsTheSessionModel(t *testing.T) {
	store := newTestStore(t)
	runner := New(Config{
		Catalog:    budgetCatalog(),
		Provider:   "big",
		Model:      "big-model",
		Store:      store,
		Compaction: &CompactionConfig{ContextWindowTokens: 200000},
	})
	opened := newTestSession(t, store)
	if _, err := opened.Append(session.EventModelSelection, session.ModelSelection{
		Provider: "small", Model: "small-model",
	}); err != nil {
		t.Fatal(err)
	}
	budget := runner.compactionBudget(opened)
	if budget == nil {
		t.Fatal("no budget for a session whose model declares a window")
	}
	if budget.ContextWindowTokens != 32000 {
		t.Fatalf("window = %d, want the session model's 32000", budget.ContextWindowTokens)
	}
	if budget.WindowSource == "" || budget.WindowSource == "catalog default" {
		t.Fatalf("window source = %q, want it to name the session model", budget.WindowSource)
	}
	// The retain budget follows the same window: 16% of 32000.
	if want := int64(32000 * compactionRetainRatio); budget.RetainTokens != want {
		t.Fatalf("retain = %d, want %d", budget.RetainTokens, want)
	}
}

// TestBudgetFallsBackToTheCatalogDefault keeps a session that never selected a
// model measurable, and names where the number came from.
func TestBudgetFallsBackToTheCatalogDefault(t *testing.T) {
	store := newTestStore(t)
	runner := New(Config{
		Catalog:    budgetCatalog(),
		Provider:   "big",
		Model:      "big-model",
		Store:      store,
		Compaction: &CompactionConfig{ContextWindowTokens: 200000},
	})
	opened := newTestSession(t, store)
	budget := runner.compactionBudget(opened)
	if budget == nil || budget.ContextWindowTokens != 200000 {
		t.Fatalf("budget = %+v", budget)
	}
	if budget.WindowSource != "catalog default" {
		t.Fatalf("source = %q", budget.WindowSource)
	}
}

// TestNoWindowDisablesCompaction keeps an unknown window from triggering work that
// cannot be measured.
func TestNoWindowDisablesCompaction(t *testing.T) {
	store := newTestStore(t)
	runner := New(Config{Catalog: budgetCatalog(), Provider: "big", Model: "big-model", Store: store})
	opened := newTestSession(t, store)
	if budget := runner.compactionBudget(opened); budget != nil {
		t.Fatalf("a catalog with no window produced a budget: %+v", budget)
	}
	if _, pressure := runner.shouldCompact(opened); pressure {
		t.Fatal("compaction triggered with no window to measure against")
	}
}

// TestRetainedTailIsMeasuredInTokens is what stops a large-message session from
// keeping a fifth of its messages and still overflowing the window.
func TestRetainedTailIsMeasuredInTokens(t *testing.T) {
	events := make([]session.Event, 0, 100)
	candidates := make([]int64, 0, 100)
	for i := 0; i < 100; i++ {
		events = append(events, session.Event{Seq: int64(i), Data: make([]byte, 4000)})
		candidates = append(candidates, int64(i))
	}
	// 100 nodes at ~1000 tokens each; a 5000-token budget keeps only a handful,
	// where the old len/5 fraction would have kept 20.
	keepFrom := retainedFrom(candidates, events, 5000)
	kept := len(candidates) - keepFrom
	if kept >= 20 {
		t.Fatalf("kept %d nodes for a 5000-token budget; the fraction is still in charge", kept)
	}
	if kept < compactionMinMessages {
		t.Fatalf("kept %d nodes, below the minimum that keeps a conversation usable", kept)
	}
}

// TestOversizedNodeStillKeepsTheMinimum is the rule for one message larger than the
// whole retain budget: keep it rather than emptying the tail.
func TestOversizedNodeStillKeepsTheMinimum(t *testing.T) {
	events := make([]session.Event, 0, 20)
	candidates := make([]int64, 0, 20)
	for i := 0; i < 20; i++ {
		size := 100
		if i >= 19 {
			size = 400000 // one node worth far more than the budget
		}
		events = append(events, session.Event{Seq: int64(i), Data: make([]byte, size)})
		candidates = append(candidates, int64(i))
	}
	keepFrom := retainedFrom(candidates, events, 1000)
	kept := len(candidates) - keepFrom
	if kept < compactionMinMessages {
		t.Fatalf("kept %d nodes; an oversized message must not empty the tail", kept)
	}
}

// TestIneffectiveCompactionFailsLoudly is the `/compact` that reported success and
// left a session that still could not send: the result is measured against the same
// window the next request is measured against, and both numbers are reported.
func TestIneffectiveCompactionFailsLoudly(t *testing.T) {
	store := newTestStore(t)
	runner := New(Config{
		Catalog: budgetCatalog(), Provider: "big", Model: "big-model", Store: store,
		Compaction: &CompactionConfig{ContextWindowTokens: 200000},
	})
	opened := newTestSession(t, store)
	// One message larger than any window it could be compacted against, and too few
	// messages for the span to be worth replacing: nothing can free this.
	for i := 0; i < 4; i++ {
		if _, err := opened.Append(session.EventUserMessage, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(make([]byte, 200000))}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	budget := &session.CompactionBudget{ContextWindowTokens: 1000, RetainTokens: 160, ThresholdTokens: 800, WindowSource: "test"}
	_, err := runner.compactSurface(context.Background(), opened, budget, nil)
	if err == nil {
		if remaining := surfaceTokens(opened); remaining > budget.ContextWindowTokens {
			t.Fatalf("compaction reported success with %d tokens still queued against a %d-token window", remaining, budget.ContextWindowTokens)
		}
	}
}
