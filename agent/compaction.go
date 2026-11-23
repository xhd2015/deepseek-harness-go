package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// Compaction keeps a long session inside the model's window.
//
// Two ways in, one mechanism: an automatic compaction when the measured context
// crosses the budget, and a manual `/compact` when the user asks for one. Both
// write the same four events, so a reader cannot tell them apart except by the
// reason recorded on the start event.

// Compaction defaults, matching DSH's basic backend.
const (
	// compactionThresholdRatio is the share of the window at which pressure
	// triggers. Below it the session is still working with room to spare.
	compactionThresholdRatio = 0.8
	// compactionRetainRatio is what stays after a compaction: enough recent
	// conversation that the model still knows what it was doing.
	compactionRetainRatio = 0.16
	// compactionMinMessages is the shortest span worth compacting. Replacing two
	// messages with a summary costs more than it saves.
	compactionMinMessages = 6
)

// CompactionConfig configures the runner's automatic compaction.
type CompactionConfig struct {
	// ContextWindowTokens is the routed model's capacity. Zero disables the
	// automatic trigger: without a window there is no pressure to measure.
	ContextWindowTokens int64
	// ThresholdRatio overrides the default trigger share.
	ThresholdRatio float64
	// RetainRatio overrides the default retained share.
	RetainRatio float64
	// Summarize writes the replacement text. Nil falls back to a model-free
	// notice, because a compaction that cannot summarize should still free the
	// window rather than refuse to run.
	Summarize func(ctx context.Context, opened *session.Session, span session.SeqRange) (string, error)
}

// compactionBudget resolves the pressure numbers one run compares against.
func (r *Runner) compactionBudget() *session.CompactionBudget {
	cfg := r.cfg.Compaction
	if cfg == nil || cfg.ContextWindowTokens <= 0 {
		return nil
	}
	threshold := cfg.ThresholdRatio
	if threshold <= 0 {
		threshold = compactionThresholdRatio
	}
	retain := cfg.RetainRatio
	if retain <= 0 {
		retain = compactionRetainRatio
	}
	return &session.CompactionBudget{
		ContextWindowTokens: cfg.ContextWindowTokens,
		ThresholdTokens:     int64(float64(cfg.ContextWindowTokens) * threshold),
		RetainTokens:        int64(float64(cfg.ContextWindowTokens) * retain),
	}
}

// shouldCompact reports whether the session's surface has crossed the threshold.
//
// The measure is the surface's own size, not the whole log: a session that has
// already been compacted keeps its full history on disk and must not keep
// triggering on bytes it no longer sends.
func (r *Runner) shouldCompact(opened *session.Session) (*session.CompactionBudget, bool) {
	budget := r.compactionBudget()
	if budget == nil {
		return nil, false
	}
	surface := opened.Surface()
	measured := int64(0)
	for _, node := range surface {
		measured += estimateTokens(string(node.Data))
	}
	return budget, measured >= budget.ThresholdTokens
}

// compactSurface selects the span to replace and writes the compaction.
//
// The selection keeps the newest messages and the system head. It is deliberately
// conservative: compacting too little costs one more later compaction, while
// compacting too much removes the context the model was still using.
func (r *Runner) compactSurface(ctx context.Context, opened *session.Session, budget *session.CompactionBudget, turn *int64) (bool, error) {
	events := opened.Events()
	candidates := compactableRange(events)
	if candidates == nil {
		return false, nil
	}
	summary := ""
	if r.cfg.Compaction != nil && r.cfg.Compaction.Summarize != nil {
		text, err := r.cfg.Compaction.Summarize(ctx, opened, *candidates)
		if err != nil {
			return false, err
		}
		summary = strings.TrimSpace(text)
	}
	if summary == "" {
		// A model-free replacement still frees the window, and saying so is more
		// honest than a fabricated summary the next turn would read as history.
		summary = fmt.Sprintf("(%d earlier messages were compacted.)", candidates.End-candidates.Start+1)
	}
	tokenCount := int64(0)
	for _, event := range events {
		if event.Seq < candidates.Start || event.Seq > candidates.End {
			continue
		}
		tokenCount += estimateTokens(string(event.Data))
	}
	provider, model := r.Provider(), r.Model()
	id := "compaction-" + session.NewID()
	if err := opened.Compact(session.CompactionInput{
		CompactionID:       id,
		Turn:               turn,
		Budget:             budget,
		Summary:            summary,
		Range:              *candidates,
		ShadowedTokenCount: tokenCount,
		Provider:           provider,
		Model:              model,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// CompactNow answers the manual `/compact` command.
//
// It refuses while a turn is running: compacting the span a live turn is reading
// would replace the very events the model has already been shown this step.
func (r *Runner) CompactNow(sessionID string) (bool, error) {
	if r.cfg.Store == nil {
		// Without a store there is no way to reach a session this call was not
		// handed; saying so beats crashing on a nil handle.
		return false, fmt.Errorf("compaction needs a session store")
	}
	opened, err := r.cfg.Store.Open(sessionID)
	if err != nil {
		return false, err
	}
	if r.Busy(sessionID) {
		return false, fmt.Errorf("compaction is unavailable while a turn is running; wait for it to finish")
	}
	// The manual path prunes too, for the same reason the automatic one does: a
	// user who asked for a smaller session gets the cheap win first.
	if _, err := r.PruneSession(opened); err != nil {
		return false, err
	}
	return r.compactSurface(context.Background(), opened, nil, nil)
}

// compactableRange picks the span a compaction replaces.
//
// It ends before the retained tail and never touches a system message, an open
// turn, or a span an earlier compaction already replaced.
func compactableRange(events []session.Event) *session.SeqRange {
	state := session.FoldCompaction(events)
	var start, end int64
	found := false
	depth := 0
	for _, event := range events {
		switch event.Type {
		case session.EventTurnStart:
			depth++
		case session.EventTurnEnd:
			depth--
		}
		if event.SurfaceOp != nil && event.SurfaceOp.IsReplace() {
			continue
		}
		if state.CompactedSeqs[event.Seq] {
			continue
		}
		if event.Type == session.EventSystemMessage || event.Type == session.EventSessionTitle {
			continue
		}
		if !found {
			start, found = event.Seq, true
		}
		end = event.Seq
	}
	if !found {
		return nil
	}
	// Keep the tail: the newest messages are what the model is working on.
	retain := compactionRetainCount(events)
	candidates := seqsOf(events, start, end, state.CompactedSeqs)
	if len(candidates) < compactionMinMessages*2 {
		return nil
	}
	keepFrom := len(candidates) - retain
	if keepFrom <= compactionMinMessages {
		return nil
	}
	selected := candidates[:keepFrom]
	return &session.SeqRange{Start: selected[0], End: selected[len(selected)-1]}
}

// compactionRetainCount is how many surface nodes the replacement keeps.
func compactionRetainCount(events []session.Event) int {
	if len(events) < 4 {
		return len(events)
	}
	return len(events) / 5
}

// seqsOf lists the surface seqs in one span, skipping compacted ones.
func seqsOf(events []session.Event, start, end int64, compacted map[int64]bool) []int64 {
	var seqs []int64
	for _, event := range events {
		if event.Seq < start || event.Seq > end || compacted[event.Seq] {
			continue
		}
		seqs = append(seqs, event.Seq)
	}
	return seqs
}

// estimateTokens approximates the meter's fixed estimator: one token per four
// bytes. It is not exact, and it does not need to be — the threshold compares a
// budget with a magnitude.
func estimateTokens(text string) int64 {
	if text == "" {
		return 0
	}
	return int64(len(text)/4) + 1
}
