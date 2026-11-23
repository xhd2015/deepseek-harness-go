package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Compaction replaces an old span of the conversation with a summary.
//
// A long session eventually stops fitting the model's window, and the only way to
// continue is to replace what is oldest with what it said. The replacement is
// written as one surface `replace` event over a seq range, priced by the event
// immediately before it — so a reader can subtract the compacted span's cost
// without keeping per-node prices.

// The four events of one compaction, spelled as DSH spells them.
const (
	EventCompactionStart   = "compaction/start"
	EventCompactionSummary = "compaction/summary"
	EventCompactionPrune   = "compaction/prune"
	EventCompactionEnd     = "compaction/end"
	// EventGoalChange records a goal's creation or transition.
	EventGoalChange = "goal/change"
)

// CompactionBudget is the pressure an automatic trigger resolved.
type CompactionBudget struct {
	// ContextWindowTokens is the routed model's capacity.
	ContextWindowTokens int64 `json:"contextWindowTokens"`
	// ThresholdTokens is the pressure at or above which compaction fires.
	ThresholdTokens int64 `json:"thresholdTokens"`
	// RetainTokens is what the replacement keeps from the recent end.
	RetainTokens int64 `json:"retainTokens"`
}

// SeqRange is a span of surface positions, inclusive.
type SeqRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// CompactionInput is one compaction request.
type CompactionInput struct {
	// SessionID names the session; the caller passes the open handle instead.
	CompactionID string
	// Turn is the turn the compaction belongs to, or nil between turns.
	Turn *int64
	// Budget is set for an automatic trigger that resolved pressure.
	Budget *CompactionBudget
	// Summary is the replacement text.
	Summary string
	// Range is the span being replaced.
	Range SeqRange
	// ShadowedSeqs lists every replaced surface node, in surface order.
	ShadowedSeqs []int64
	// ShadowedTokenCount is the heuristic price of the replaced content.
	ShadowedTokenCount int64
	// Provider and Model identify the route that wrote the summary.
	Provider string
	Model    string
}

// Compact replaces one range with a summary and records the four events.
//
// The order is contractual: start, summary, the replacement, end. A reader that
// sees the replacement prices it from the summary immediately before, so writing
// them out of order makes the arithmetic wrong even though every event is present.
func (s *Session) Compact(input CompactionInput) error {
	if input.CompactionID == "" {
		return fmt.Errorf("compaction id is required")
	}
	if input.Range.End < input.Range.Start {
		return fmt.Errorf("compaction range end is before its start")
	}
	if strings.TrimSpace(input.Summary) == "" {
		return fmt.Errorf("compaction summary is empty")
	}
	seqs := input.ShadowedSeqs
	if len(seqs) == 0 {
		for seq := input.Range.Start; seq <= input.Range.End; seq++ {
			seqs = append(seqs, seq)
		}
	} else {
		sorted := make([]int64, len(seqs))
		copy(sorted, seqs)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		seqs = sorted
	}

	start := map[string]any{
		"compactionId": input.CompactionID,
		"turn":         input.Turn,
	}
	if input.Budget != nil {
		start["budget"] = input.Budget
	}
	if _, err := s.Append(EventCompactionStart, start); err != nil {
		return err
	}
	summary := map[string]any{
		"compactionId":       input.CompactionID,
		"summary":            []any{map[string]any{"type": "text", "text": input.Summary}},
		"shadowedRange":      input.Range,
		"shadowedSeqs":       seqs,
		"shadowedTokenCount": input.ShadowedTokenCount,
		"provider":           input.Provider,
		"model":              input.Model,
	}
	if _, err := s.Append(EventCompactionSummary, summary); err != nil {
		return err
	}
	// The replacement is a surface event with an explicit replace op, so a reader
	// rebuilds the conversation from the summary instead of from the originals.
	replacement := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": input.Summary}},
		// The kind is the marker a compacted reset looks for: it distinguishes the
		// summary that replaced old history from a question the person asked. A
		// reader that guessed from the text instead would keep summarizing summaries.
		"source": map[string]any{"kind": "compact-checkpoint", "compactionId": input.CompactionID},
		"role":   "user",
		"id":     NewID(),
	}
	raw, err := EncodeData(replacement)
	if err != nil {
		return err
	}
	if _, err := s.append(Event{
		Type:      EventUserMessage,
		Data:      raw,
		SurfaceOp: &SurfaceOp{Replace: true, StartSeq: input.Range.Start, EndSeq: input.Range.End},
	}); err != nil {
		return err
	}
	if _, err := s.Append(EventCompactionEnd, map[string]any{
		"compactionId": input.CompactionID,
		"turn":         input.Turn,
	}); err != nil {
		return err
	}
	return nil
}

// CompactPrune replaces one range with a model-free notice.
//
// A prune exists for content whose value is its existence rather than its wording
// (a tool result that has been read once), so nothing is summarized and no model
// is called.
func (s *Session) CompactPrune(span SeqRange, seqs []int64, tokenCount int64, notice string) error {
	if span.End < span.Start {
		return fmt.Errorf("prune range end is before its start")
	}
	if len(seqs) == 0 {
		for seq := span.Start; seq <= span.End; seq++ {
			seqs = append(seqs, seq)
		}
	}
	if _, err := s.Append(EventCompactionPrune, map[string]any{
		"shadowedRange":      span,
		"shadowedSeqs":       seqs,
		"shadowedTokenCount": tokenCount,
	}); err != nil {
		return err
	}
	replacement := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": notice}},
		"source":  map[string]any{"kind": "compaction-prune"},
		"role":    "user",
		"id":      NewID(),
	}
	raw, err := EncodeData(replacement)
	if err != nil {
		return err
	}
	_, err = s.append(Event{
		Type:      EventUserMessage,
		Data:      raw,
		SurfaceOp: &SurfaceOp{Replace: true, StartSeq: span.Start, EndSeq: span.End},
	})
	return err
}

// CompactionState is what a log says has already been compacted.
type CompactionState struct {
	// CompactedSeqs is every seq a completed compaction replaced.
	CompactedSeqs map[int64]bool
	// LastEndSeq is the newest compaction/end, or 0 when none completed.
	LastEndSeq int64
	// Started is a compaction whose end is missing, so a retry must not start a
	// second one over the same span.
	Started bool
}

// FoldCompaction reads the compaction events out of one log.
func FoldCompaction(events []Event) CompactionState {
	state := CompactionState{CompactedSeqs: map[int64]bool{}}
	for _, event := range events {
		switch event.Type {
		case EventCompactionStart:
			state.Started = true
		case EventCompactionSummary, EventCompactionPrune:
			var payload struct {
				ShadowedSeqs []int64 `json:"shadowedSeqs"`
			}
			if json.Unmarshal(event.Data, &payload) != nil {
				continue
			}
			for _, seq := range payload.ShadowedSeqs {
				state.CompactedSeqs[seq] = true
			}
		case EventCompactionEnd:
			state.Started = false
			state.LastEndSeq = event.Seq
		}
	}
	return state
}
