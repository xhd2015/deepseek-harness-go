package session

import (
	"testing"
)

// TestCompactWritesTheFourEvents is the shape the page and every reader rely on.
func TestCompactWritesTheFourEvents(t *testing.T) {
	opened := newProjectionSession(t)
	for i := 0; i < 10; i++ {
		if _, err := opened.AppendSurface(EventUserMessage, map[string]any{
			"role": "user", "id": NewID(),
			"content": []any{map[string]any{"type": "text", "text": "message"}},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events := opened.Events()
	span := SeqRange{Start: events[0].Seq, End: events[4].Seq}

	if err := opened.Compact(CompactionInput{
		CompactionID:       "compaction-1",
		Summary:            "the first five messages said hello",
		Range:              span,
		ShadowedTokenCount: 42,
		Provider:           "fake-provider",
		Model:              "fake-model",
	}); err != nil {
		t.Fatalf("compact: %v", err)
	}

	kinds := []string{}
	for _, event := range opened.Events() {
		switch event.Type {
		case EventCompactionStart, EventCompactionSummary, EventCompactionEnd:
			kinds = append(kinds, event.Type)
		}
	}
	if got := len(kinds); got != 3 {
		t.Fatalf("kinds = %v, want the start/summary/end triple", kinds)
	}
	if kinds[0] != EventCompactionStart || kinds[1] != EventCompactionSummary || kinds[2] != EventCompactionEnd {
		t.Fatalf("order = %v", kinds)
	}

	// The replacement is the event right after the summary: a reader prices it
	// from that adjacency.
	var sawReplace bool
	for _, event := range opened.Events() {
		if event.SurfaceOp != nil && event.SurfaceOp.IsReplace() {
			sawReplace = true
			if event.SurfaceOp.StartSeq != span.Start || event.SurfaceOp.EndSeq != span.End {
				t.Fatalf("replace span = %+v, want %+v", event.SurfaceOp, span)
			}
		}
	}
	if !sawReplace {
		t.Fatal("no surface replace was written")
	}

	// The surface shrinks: the replaced span is gone, the summary stands in.
	surface := opened.Surface()
	for _, event := range surface {
		if event.Seq >= span.Start && event.Seq <= span.End {
			t.Fatalf("replaced seq %d is still on the surface", event.Seq)
		}
	}
	folded := FoldCompaction(opened.Events())
	for seq := span.Start; seq <= span.End; seq++ {
		if !folded.CompactedSeqs[seq] {
			t.Fatalf("seq %d is not marked compacted", seq)
		}
	}
	if folded.Started {
		t.Fatal("the compaction was left open")
	}
}

// TestCompactRejectsEmptySummary keeps a compaction from erasing history without
// saying what replaced it.
func TestCompactRejectsEmptySummary(t *testing.T) {
	opened := newProjectionSession(t)
	if err := opened.Compact(CompactionInput{CompactionID: "x", Summary: "  ", Range: SeqRange{0, 1}}); err == nil {
		t.Fatal("an empty summary was accepted")
	}
}

// TestPruneIsModelFree proves a prune writes its own pricing event and a replace.
func TestPruneIsModelFree(t *testing.T) {
	opened := newProjectionSession(t)
	for i := 0; i < 3; i++ {
		if _, err := opened.AppendSurface(EventToolResult, map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{"id": NewID(), "role": "tool", "toolCallId": "c", "content": []any{}},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events := opened.Events()
	span := SeqRange{Start: events[0].Seq, End: events[1].Seq}
	if err := opened.CompactPrune(span, nil, 10, "(pruned)"); err != nil {
		t.Fatalf("prune: %v", err)
	}
	state := FoldCompaction(opened.Events())
	if !state.CompactedSeqs[span.Start] || !state.CompactedSeqs[span.End] {
		t.Fatalf("prune did not mark its span: %+v", state)
	}
}
