package api

import (
	"encoding/json"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// Compaction answers three questions: compact this session now, report what has
// already been compacted, and reset the session into a new one at its compacted
// size. They are separate methods because each is useful alone.

// compact answers session/compact: one manual compaction, no arguments.
//
// The ambiguity a caller can create is bounded: the method refuses while a turn is
// running, and a compaction that has nothing worth replacing reports that instead
// of writing an empty replacement.
func (s *Service) compact(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return nil, fail(CodeBadRequest, "sessionId is required")
	}
	if s.cfg.Runner == nil {
		return nil, fail(CodeUnimplemented, "compaction is not configured")
	}
	compacted, err := s.cfg.Runner.CompactNow(request.SessionID)
	if err != nil {
		return nil, fail(CodeBusy, err.Error())
	}
	return map[string]any{"compacted": compacted}, nil
}

// compaction answers session/compaction: what this session's log says about it.
//
// The page reads this to show that a span was replaced rather than deleted, so it
// reports the spans, the events, and whether an attempt is still open.
func (s *Service) compaction(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	events := opened.Events()
	state := session.FoldCompaction(events)

	compactedSeqs := make([]int64, 0, len(state.CompactedSeqs))
	for seq := range state.CompactedSeqs {
		compactedSeqs = append(compactedSeqs, seq)
	}
	sortInt64(compactedSeqs)

	type span struct {
		CompactionID string `json:"compactionId"`
		StartSeq     int64  `json:"startSeq"`
		SummarySeq   int64  `json:"summarySeq,omitempty"`
		EndSeq       int64  `json:"endSeq,omitempty"`
		Range        any    `json:"shadowedRange,omitempty"`
		TokenCount   int64  `json:"shadowedTokenCount,omitempty"`
		Error        string `json:"error,omitempty"`
	}
	spans := make([]span, 0, 4)
	open := map[string]*span{}
	for _, event := range events {
		var header struct {
			CompactionID string `json:"compactionId"`
		}
		if json.Unmarshal(event.Data, &header) != nil || header.CompactionID == "" {
			continue
		}
		switch event.Type {
		case session.EventCompactionStart:
			entry := &span{CompactionID: header.CompactionID, StartSeq: event.Seq}
			spans = append(spans, *entry)
			open[header.CompactionID] = &spans[len(spans)-1]
		case session.EventCompactionSummary, session.EventCompactionPrune:
			entry := open[header.CompactionID]
			if entry == nil {
				continue
			}
			var payload struct {
				ShadowedRange      any   `json:"shadowedRange"`
				ShadowedTokenCount int64 `json:"shadowedTokenCount"`
			}
			if json.Unmarshal(event.Data, &payload) != nil {
				continue
			}
			entry.SummarySeq = event.Seq
			entry.Range = payload.ShadowedRange
			entry.TokenCount = payload.ShadowedTokenCount
		case session.EventCompactionEnd:
			entry := open[header.CompactionID]
			if entry == nil {
				continue
			}
			var payload struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(event.Data, &payload)
			entry.EndSeq = event.Seq
			entry.Error = payload.Error
			delete(open, header.CompactionID)
		}
	}
	for id := range open {
		_ = id
	}
	return map[string]any{
		"compactions":   spans,
		"compactedSeqs": compactedSeqs,
		"active":        state.Started,
		"lastEndSeq":    state.LastEndSeq,
	}, nil
}

// compactIntoNew answers session/compactIntoNew: compact the source, then fork
// only its post-compaction surface into a child.
//
// The child starts at the compacted size instead of copying the source history,
// which is the difference between "reset into a smaller session" and "clone the
// whole thing". The source stays readable.
func (s *Service) compactIntoNew(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return nil, fail(CodeBadRequest, "sessionId is required")
	}
	if s.cfg.Runner == nil || s.cfg.Store == nil {
		return nil, fail(CodeUnimplemented, "a compacted reset needs a session store")
	}
	if s.cfg.Runner.Busy(request.SessionID) {
		// The surface must not move while it is being read into a request.
		return nil, fail(CodeBusy, "session is running; wait for the current turn before resetting")
	}
	source, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	// A reset does not compact first: the surface already carries whatever
	// compaction produced, and with no context pressure a fresh /compact retains
	// nothing and would swallow the turns this seed exists to keep.
	events := source.Events()
	surface := source.Surface()
	tokensBefore := compactedSeedTokens(surface)
	hadCheckpoint := false
	for _, event := range surface {
		if event.Type == session.EventUserMessage && sourceKindOf(event) == "compact-checkpoint" {
			hadCheckpoint = true
			break
		}
	}
	seed := buildCompactedSeed(events, surface)
	if len(seed) == 0 {
		return nil, fail(CodeForkUnavailable, "this session has no history to seed from")
	}
	cwd := strings.TrimSpace(request.Cwd)
	if cwd == "" {
		cwd = source.Cwd()
	}
	child, err := s.cfg.Store.CreateSeeded(cwd, source.Header(), seed)
	if err != nil {
		return nil, fail(CodeInternal, "compacted session: "+err.Error())
	}
	s.Workspaces.Add(cwd)
	value := map[string]any{
		"sessionId": child.ID(),
		// `compacted` reports whether the source carried a compaction checkpoint,
		// which is what DSH returns: the reset always runs, the flag says whether
		// a summary was part of what it kept.
		"compacted": hadCheckpoint,
	}
	if removed := tokensBefore - compactedSeedTokens(child.Events()); removed > 0 {
		value["compactedTokens"] = removed
	}
	return value, nil
}

// sortInt64 orders a seq list ascending without importing a generic sort.
func sortInt64(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
