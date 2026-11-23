package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// appendToolResult appends one tool result with the given text.
func appendToolResult(t *testing.T, opened *session.Session, text string) session.Event {
	t.Helper()
	event, err := opened.AppendSurface(session.EventToolResult, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"id": session.NewID(), "role": "tool", "toolCallId": "call-1",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	})
	if err != nil {
		t.Fatalf("append tool result: %v", err)
	}
	return event
}

// TestPruneCutsTheMiddleOfAnOversizedResult is the shape the real log has: a
// `compaction/prune` shadow price immediately followed by a replacement.
func TestPruneCutsTheMiddleOfAnOversizedResult(t *testing.T) {
	runner := New(Config{Provider: "p", Model: "m"})
	opened := newOpenSession(t, t.TempDir())
	head := strings.Repeat("A", pruneHeadChars)
	middle := strings.Repeat("B", pruneThresholdChars)
	tail := strings.Repeat("C", pruneTailChars)
	original := appendToolResult(t, opened, head+middle+tail)

	result, err := runner.PruneSession(opened)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.Pruned != 1 {
		t.Fatalf("pruned = %d", result.Pruned)
	}
	if result.CharsRemoved <= 0 {
		t.Fatalf("chars removed = %d", result.CharsRemoved)
	}

	// The pricing event comes first, and it prices exactly the node replaced.
	events := opened.Events()
	var pruneSeq, replaceSeq int64
	for _, event := range events {
		switch event.Type {
		case session.EventCompactionPrune:
			pruneSeq = event.Seq
			var payload struct {
				ShadowedRange      session.SeqRange `json:"shadowedRange"`
				ShadowedSeqs       []int64          `json:"shadowedSeqs"`
				ShadowedTokenCount int64            `json:"shadowedTokenCount"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatalf("decode prune: %v", err)
			}
			if payload.ShadowedRange.Start != original.Seq || payload.ShadowedRange.End != original.Seq {
				t.Fatalf("shadowed range = %+v, want seq %d", payload.ShadowedRange, original.Seq)
			}
			if len(payload.ShadowedSeqs) != 1 || payload.ShadowedSeqs[0] != original.Seq {
				t.Fatalf("shadowed seqs = %v", payload.ShadowedSeqs)
			}
			if payload.ShadowedTokenCount <= 0 {
				t.Fatalf("token count = %d", payload.ShadowedTokenCount)
			}
		case session.EventToolResult:
			if event.SurfaceOp != nil && event.SurfaceOp.IsReplace() {
				replaceSeq = event.Seq
			}
		}
	}
	if pruneSeq == 0 || replaceSeq == 0 {
		t.Fatalf("prune=%d replace=%d, want both", pruneSeq, replaceSeq)
	}
	if replaceSeq != pruneSeq+1 {
		t.Fatalf("the replacement is at %d, want right after the price at %d", replaceSeq, pruneSeq)
	}

	// The replaced node leaves the surface, and the replacement keeps both ends.
	surface := opened.Surface()
	for _, event := range surface {
		if event.Seq == original.Seq {
			t.Fatal("the original result is still on the surface")
		}
	}
	var kept string
	for _, event := range surface {
		if event.Type != session.EventToolResult {
			continue
		}
		var payload struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && len(payload.Message.Content) > 0 {
			kept = payload.Message.Content[0].Text
		}
	}
	if !strings.HasPrefix(kept, "AAAA") || !strings.HasSuffix(kept, "CCCC") {
		t.Fatalf("the replacement lost an end: %q…%q", first(kept, 8), last(kept, 8))
	}
	if !strings.Contains(kept, "tool result middle pruned") {
		t.Fatalf("the replacement does not mark the cut: %q", kept[:120])
	}
	if len(kept) >= len(head+middle+tail) {
		t.Fatalf("the replacement is not smaller: %d vs %d", len(kept), len(head+middle+tail))
	}
}

// TestPruneLeavesSmallResultsAlone avoids spending writes on nothing.
func TestPruneLeavesSmallResultsAlone(t *testing.T) {
	runner := New(Config{Provider: "p", Model: "m"})
	opened := newOpenSession(t, t.TempDir())
	appendToolResult(t, opened, "a short result")
	result, err := runner.PruneSession(opened)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.Pruned != 0 {
		t.Fatalf("pruned = %d, want none", result.Pruned)
	}
	for _, event := range opened.Events() {
		if event.Type == session.EventCompactionPrune {
			t.Fatal("a small result was priced for pruning")
		}
	}
}

// first and last render an edge of a long string for a failure message.
func first(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}

func last(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}
