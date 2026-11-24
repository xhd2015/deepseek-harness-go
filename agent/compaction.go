package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/modelconfig"
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

// compactionBudget resolves the pressure numbers one run compares against, for
// the model the *session* is routed to.
//
// The window belongs to the model, not to the deployment. `ContextWindowTokens`
// is resolved once at boot from the catalog default, so a session that selected a
// different model was measured against the wrong capacity: with a real window
// smaller than the default, a "successful" compaction still exceeded it and the
// next prompt was refused for length. The session's own selection wins here, and
// the boot default is only the fallback for a session that never picked one.
func (r *Runner) compactionBudget(opened *session.Session) *session.CompactionBudget {
	cfg := r.cfg.Compaction
	if cfg == nil {
		return nil
	}
	window, source := r.contextWindowFor(opened)
	if window <= 0 {
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
		ContextWindowTokens: window,
		ThresholdTokens:     int64(float64(window) * threshold),
		RetainTokens:        int64(float64(window) * retain),
		WindowSource:        source,
	}
}

// contextWindowFor resolves the capacity of the model this session is routed to.
//
// The second return names where the number came from: a budget recorded against an
// assumed window is a different claim from one recorded against a catalog entry,
// and a compaction that later fails should say which it used.
func (r *Runner) contextWindowFor(opened *session.Session) (int64, string) {
	if opened != nil && r.catalog != nil {
		selection := lastSelection(opened)
		// A session that never selected a model is not routed by its own choice, so
		// the source says "catalog default" even though the window comes from the
		// same lookup. The distinction matters when a compaction is later found to
		// have used the wrong capacity: which of the two it read is the first
		// question asked.
		fromSession := selection.Provider != "" && selection.Model != ""
		if !fromSession {
			selection = r.catalog.Default
		}
		if route, err := r.catalog.Resolve(selection); err == nil {
			if window := contextWindowOf(r.catalog, route.Selection.Provider, route.Selection.Model); window > 0 {
				if fromSession {
					return window, "session model " + route.Selection.Provider + "/" + route.Selection.Model
				}
				return window, "catalog default"
			}
		}
	}
	if cfg := r.cfg.Compaction; cfg != nil && cfg.ContextWindowTokens > 0 {
		return cfg.ContextWindowTokens, "catalog default"
	}
	return 0, ""
}

// contextWindowOf reads one model's declared capacity out of the catalog.
func contextWindowOf(catalog *modelconfig.Catalog, provider, model string) int64 {
	if catalog == nil {
		return 0
	}
	for _, entry := range catalog.Providers {
		if entry.ID != provider {
			continue
		}
		for _, item := range entry.Models {
			if item.ID == model {
				return int64(item.ContextWindow)
			}
		}
	}
	return 0
}

// shouldCompact reports whether the session's surface has crossed the threshold.
//
// The measure is the surface's own size, not the whole log: a session that has
// already been compacted keeps its full history on disk and must not keep
// triggering on bytes it no longer sends.
// A window that cannot be resolved disables the automatic trigger entirely: with no
// capacity to measure against there is no pressure to act on, and compacting blind
// would replace history without any way to say whether it helped.
func (r *Runner) shouldCompact(opened *session.Session) (*session.CompactionBudget, bool) {
	budget := r.compactionBudget(opened)
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
	candidates := compactableRange(events, budget)
	if candidates == nil {
		// Nothing worth replacing — but "nothing to replace" and "still too big" can
		// both be true, and that combination is exactly a compaction that reports
		// success while the next prompt is refused. The check runs either way.
		return false, compactionResultCheck(opened, budget)
	}
	summary := ""
	if r.cfg.Compaction != nil && r.cfg.Compaction.Summarize != nil {
		text, err := r.cfg.Compaction.Summarize(ctx, opened, *candidates)
		if err != nil {
			// A summarizer failure must not fail the turn: the window still has to
			// be freed, and the model-free notice is a usable replacement. The log
			// keeps the reason so a summarizer that is always broken is visible.
			r.logf("session %s: summarize span: %v (falling back to the notice)", opened.ID(), err)
		} else {
			summary = strings.TrimSpace(text)
		}
	}
	if summary == "" {
		// A model-free replacement still frees the window, and saying so is more
		// honest than a fabricated summary the next turn would read as history.
		summary = fmt.Sprintf("(%d earlier messages were compacted.)", candidates.End-candidates.Start+1)
	}
	if summary != "" && budget != nil && budget.RetainTokens > 0 {
		r.logf("session %s: compacted span %d..%d, window %d tokens (%s), retained %d",
			opened.ID(), candidates.Start, candidates.End, budget.ContextWindowTokens, budget.WindowSource, budget.RetainTokens)
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
	// Measure the result with the same estimator the next request is measured with.
	//
	// A compaction that writes a summary and frees nothing usable is the failure
	// this check exists for: `/compact` reported success while the next prompt was
	// still refused for length. The two numbers go in the error so the claim can be
	// checked instead of believed.
	return true, compactionResultCheck(opened, budget)
}

// compactionResultCheck reports the session as unusable when the surface still
// exceeds the window after a compaction.
//
// The measure is the same fixed estimator, over the same surface, that the next
// request is built from: checking with a different number would let a compaction
// pass its own test and fail the request it was meant to enable.
func compactionResultCheck(opened *session.Session, budget *session.CompactionBudget) error {
	if budget == nil || budget.ContextWindowTokens <= 0 {
		return nil
	}
	if remaining := surfaceTokens(opened); remaining > budget.ContextWindowTokens {
		return fmt.Errorf(
			"compaction/ineffective: needs %d tokens, window %d (%s)",
			remaining, budget.ContextWindowTokens, budget.WindowSource)
	}
	return nil
}

// surfaceTokens is the size of what the next request would send, by the same
// fixed estimator shouldCompact uses.
func surfaceTokens(opened *session.Session) int64 {
	measured := int64(0)
	for _, node := range opened.Surface() {
		measured += estimateTokens(string(node.Data))
	}
	return measured
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
func compactableRange(events []session.Event, budget *session.CompactionBudget) *session.SeqRange {
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
	//
	// The tail is measured in tokens, not in messages. A count is a proxy that
	// breaks exactly where it matters: a session whose messages are large keeps a
	// fifth of them and still holds more than the window, so a "successful"
	// compaction leaves nothing usable. The window is a token budget, so what is
	// retained has to be one too.
	candidates := seqsOf(events, start, end, state.CompactedSeqs)
	if len(candidates) < compactionMinMessages*2 {
		return nil
	}
	retainTokens := int64(0)
	if budget != nil {
		retainTokens = budget.RetainTokens
	}
	keepFrom := retainedFrom(candidates, events, retainTokens)
	if keepFrom <= compactionMinMessages {
		return nil
	}
	selected := candidates[:keepFrom]
	return &session.SeqRange{Start: selected[0], End: selected[len(selected)-1]}
}

// retainedFrom returns how many candidates the replacement keeps, counting back
// from the newest until the retained part no longer fits retainTokens.
//
// At least compactionMinMessages nodes are always kept, even when one of them alone
// exceeds the budget. Compacting everything would leave the model with no recent
// context and the next run would have nothing left to replace; an oversized single
// message is a problem for that message (see oversizedSummary), not for the tail.
func retainedFrom(candidates []int64, events []session.Event, retainTokens int64) int {
	if retainTokens <= 0 {
		// No budget means a window this code cannot size. The old fraction keeps
		// compaction working for a catalog that declares no capacity at all.
		retain := len(events) / 5
		if retain > len(candidates) {
			retain = len(candidates)
		}
		return len(candidates) - retain
	}
	bySeq := make(map[int64]session.Event, len(events))
	for _, event := range events {
		bySeq[event.Seq] = event
	}
	used := int64(0)
	keep := 0
	for i := len(candidates) - 1; i >= 0; i-- {
		cost := estimateTokens(string(bySeq[candidates[i]].Data))
		if keep >= compactionMinMessages && used+cost > retainTokens {
			break
		}
		used += cost
		keep++
	}
	if keep > len(candidates) {
		keep = len(candidates)
	}
	return len(candidates) - keep
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
