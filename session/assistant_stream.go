package session

import (
	"encoding/json"
	"time"
)

// AssistantStreamFrame is one process-local presentation frame. Frames are
// dense: every start, chunk, and end increments Revision by exactly one.
type AssistantStreamFrame struct {
	Type            string `json:"type"`
	AttemptID       string `json:"attemptId,omitempty"`
	Revision        int64  `json:"revision,omitempty"`
	StartedAfterSeq int64  `json:"startedAfterSeq,omitempty"`
	Turn            int    `json:"turn,omitempty"`
	Step            int    `json:"step,omitempty"`
	Index           int    `json:"index,omitempty"`
	Time            int64  `json:"time,omitempty"`
	Chunk           any    `json:"chunk,omitempty"`
	Outcome         any    `json:"outcome,omitempty"`
}

// MarshalJSON emits the exact per-type wire shape. Index and time must survive
// their zero values, so the frame cannot use omitempty blindly.
func (f AssistantStreamFrame) MarshalJSON() ([]byte, error) {
	switch f.Type {
	case "start":
		return json.Marshal(struct {
			Type            string `json:"type"`
			AttemptID       string `json:"attemptId"`
			Revision        int64  `json:"revision"`
			StartedAfterSeq int64  `json:"startedAfterSeq"`
			Turn            int    `json:"turn"`
			Step            int    `json:"step"`
		}{f.Type, f.AttemptID, f.Revision, f.StartedAfterSeq, f.Turn, f.Step})
	case "chunk":
		return json.Marshal(struct {
			Type      string `json:"type"`
			AttemptID string `json:"attemptId"`
			Revision  int64  `json:"revision"`
			Index     int    `json:"index"`
			Time      int64  `json:"time"`
			Chunk     any    `json:"chunk"`
		}{f.Type, f.AttemptID, f.Revision, f.Index, f.Time, f.Chunk})
	case "end":
		return json.Marshal(struct {
			Type      string `json:"type"`
			AttemptID string `json:"attemptId"`
			Revision  int64  `json:"revision"`
			Index     int    `json:"index"`
			Outcome   any    `json:"outcome"`
		}{f.Type, f.AttemptID, f.Revision, f.Index, f.Outcome})
	default:
		return nil, &json.UnsupportedValueError{Str: "assistant stream frame type " + f.Type}
	}
}

// AssistantAttemptSnapshot is the reconnect baseline for the live attempt.
type AssistantAttemptSnapshot struct {
	AttemptID       string            `json:"attemptId"`
	StartedAfterSeq int64             `json:"startedAfterSeq"`
	Turn            int               `json:"turn"`
	Step            int               `json:"step"`
	NextIndex       int               `json:"nextIndex"`
	Stream          []json.RawMessage `json:"stream"`
}

// AssistantStreamBaseline is the opening revision and optional live attempt.
type AssistantStreamBaseline struct {
	Revision      int64                     `json:"revision"`
	ActiveAttempt *AssistantAttemptSnapshot `json:"activeAttempt,omitempty"`
}

// AssistantAttempt emits frames and records the compact stream for reconnect.
type AssistantAttempt struct {
	session   *Session
	id        string
	started   int64
	turn      int
	step      int
	nextIndex int
	records   []json.RawMessage
	done      bool
}

// FeedItem is one ordered item in a session follow feed.
type FeedItem struct {
	Event *Event
	Frame *AssistantStreamFrame
}

// BeginAssistantAttempt starts one attempt after the user message (or the
// previous settlement) has been appended.
func (s *Session) BeginAssistantAttempt(turn, step int) *AssistantAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attemptCounter++
	attempt := &AssistantAttempt{
		session:   s,
		id:        s.header.ID + ":" + itoa(s.attemptCounter),
		started:   s.lastSeqLocked(),
		turn:      turn,
		step:      step,
		nextIndex: 0,
	}
	s.active = attempt
	s.revision++
	frame := &AssistantStreamFrame{
		Type:            "start",
		AttemptID:       attempt.id,
		Revision:        s.revision,
		StartedAfterSeq: attempt.started,
		Turn:            turn,
		Step:            step,
	}
	s.broadcastFrameLocked(frame)
	return attempt
}

// Push records one raw StreamChunk as a compact raw chunk record and emits the
// matching live frame.
func (a *AssistantAttempt) Push(chunk any) error {
	s := a.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.done || s.active != a {
		return nil
	}
	time := time.Now().UnixMilli()
	record, err := json.Marshal(map[string]any{
		"type":  "chunk",
		"time":  time,
		"chunk": chunk,
	})
	if err != nil {
		return err
	}
	a.records = append(a.records, record)
	s.revision++
	frame := &AssistantStreamFrame{
		Type:      "chunk",
		AttemptID: a.id,
		Revision:  s.revision,
		Index:     a.nextIndex,
		Time:      time,
		Chunk:     chunk,
	}
	a.nextIndex++
	s.broadcastFrameLocked(frame)
	return nil
}

// Settle emits the committed terminal frame after the durable settlement event
// has been appended. eventType is assistant/message or assistant/attempt.
func (a *AssistantAttempt) Settle(eventType string, seq int64) {
	s := a.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.done || s.active != a {
		return
	}
	s.revision++
	frame := &AssistantStreamFrame{
		Type:      "end",
		AttemptID: a.id,
		Revision:  s.revision,
		Index:     a.nextIndex,
		Outcome: map[string]any{
			"kind":      "committed",
			"eventType": eventType,
			"seq":       seq,
		},
	}
	a.done = true
	s.active = nil
	s.broadcastFrameLocked(frame)
}

// CompactStream returns a copy of the authoritatively persisted compact
// records, for the durable settlement event.
func (a *AssistantAttempt) CompactStream() []json.RawMessage {
	s := a.session
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, len(a.records))
	copy(out, a.records)
	return out
}

// Abandon emits the abandoned terminal frame.
func (a *AssistantAttempt) Abandon() {
	s := a.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.done || s.active != a {
		return
	}
	s.revision++
	frame := &AssistantStreamFrame{
		Type:      "end",
		AttemptID: a.id,
		Revision:  s.revision,
		Index:     a.nextIndex,
		Outcome:   map[string]any{"kind": "abandoned"},
	}
	a.done = true
	s.active = nil
	s.broadcastFrameLocked(frame)
}

func (s *Session) assistantBaselineLocked() AssistantStreamBaseline {
	baseline := AssistantStreamBaseline{Revision: s.revision}
	if s.active == nil {
		return baseline
	}
	records := make([]json.RawMessage, len(s.active.records))
	copy(records, s.active.records)
	baseline.ActiveAttempt = &AssistantAttemptSnapshot{
		AttemptID:       s.active.id,
		StartedAfterSeq: s.active.started,
		Turn:            s.active.turn,
		Step:            s.active.step,
		NextIndex:       s.active.nextIndex,
		Stream:          records,
	}
	return baseline
}

func (s *Session) broadcastFrameLocked(frame *AssistantStreamFrame) {
	for id, ch := range s.feedSubs {
		select {
		case ch <- FeedItem{Frame: frame}:
		default:
			delete(s.feedSubs, id)
			close(ch)
		}
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
