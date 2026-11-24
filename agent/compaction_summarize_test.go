package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestSummaryFallsBackWhenTheModelFails keeps a compaction working even when the
// summary call does not: refusing to compact would leave the session over its
// window and unable to run at all.
func TestSummaryFallsBackWhenTheModelFails(t *testing.T) {
	store := newTestStore(t)
	runner := New(Config{
		Catalog: budgetCatalog(), Provider: "big", Model: "big-model", Store: store,
		Compaction: &CompactionConfig{ContextWindowTokens: 200000},
	})
	runner.SetSummarizer(func(ctx context.Context, opened *session.Session, span session.SeqRange) (string, error) {
		return "", errors.New("summarizer exploded")
	})
	opened := newTestSession(t, store)
	for i := 0; i < 20; i++ {
		if _, err := opened.Append(session.EventUserMessage, map[string]any{
			"content": []map[string]any{{"type": "text", "text": strings.Repeat("x", 2000)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	budget := &session.CompactionBudget{ContextWindowTokens: 100000, RetainTokens: 500, ThresholdTokens: 80000, WindowSource: "test"}
	compacted, err := runner.compactSurface(context.Background(), opened, budget, nil)
	if err != nil && !strings.Contains(err.Error(), "ineffective") {
		t.Fatalf("a summarizer failure must not fail the compaction: %v", err)
	}
	if !compacted {
		t.Fatal("nothing was compacted after the summarizer failed")
	}
	// The replacement is the model-free notice, not an empty summary.
	var found bool
	for _, event := range opened.Events() {
		if event.Type == session.EventCompactionSummary {
			found = true
			if !strings.Contains(string(event.Data), "compacted") {
				t.Fatalf("summary = %s", event.Data)
			}
		}
	}
	if !found {
		t.Fatal("no summary event was written")
	}
}

// TestSpanTranscriptKeepsTheNewestText is why the cap is applied from the end: the
// span nearest the kept tail is what the summary has to carry forward.
func TestSpanTranscriptKeepsTheNewestText(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Data: []byte(`{"content":[{"type":"text","text":"OLDEST"}]}`)},
		{Seq: 2, Type: session.EventUserMessage, Data: []byte(`{"content":[{"type":"text","text":"NEWEST"}]}`)},
	}
	text := spanTranscript(events, session.SeqRange{Start: 1, End: 2}, 20)
	if strings.Contains(text, "OLDEST") {
		t.Fatalf("the cap kept the oldest text: %q", text)
	}
	if !strings.Contains(text, "NEWEST") {
		t.Fatalf("the cap dropped the newest text: %q", text)
	}
}
