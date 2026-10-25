// Package session stores the durable event log of one chat session using the
// DeepSeek Harness v4 JSONL layout, minus the compression and cross-process
// locking the Node host adds.
//
// On-disk shape:
//
//	<root>/<cwd-slug>/session-<id>/session.v4.jsonl
//
// The first line is the session header; every later line is one event.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// FormatVersion is the log format this package reads and writes.
	FormatVersion = 4
	// FileName is the log file inside a session directory.
	FileName = "session.v4.jsonl"
	// DirPrefix prefixes every session directory name.
	DirPrefix = "session-"
)

// Header is the first line of a session log.
type Header struct {
	Type            string `json:"type"`
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	Cwd             string `json:"cwd,omitempty"`
	ParentSession   string `json:"parentSession,omitempty"`
	IsSeeded        bool   `json:"isSeeded"`
	DelegationDepth int    `json:"delegationDepth"`
	AgentPreset     string `json:"agentPreset,omitempty"`
	Origin          string `json:"origin,omitempty"`
}

// Wire returns the browser-safe header the follow snapshot carries.
func (h Header) Wire() WireHeader {
	return WireHeader{
		Version:         h.Version,
		ID:              h.ID,
		CreatedAt:       h.CreatedAt,
		Cwd:             h.Cwd,
		ParentSession:   h.ParentSession,
		IsSeeded:        h.IsSeeded,
		Origin:          h.Origin,
		DelegationDepth: h.DelegationDepth,
		AgentPreset:     h.AgentPreset,
	}
}

// WireHeader mirrors SessionWireHeader in the TypeScript types.
type WireHeader struct {
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	Cwd             string `json:"cwd,omitempty"`
	ParentSession   string `json:"parentSession,omitempty"`
	IsSeeded        bool   `json:"isSeeded"`
	Origin          string `json:"origin,omitempty"`
	DelegationDepth int    `json:"delegationDepth,omitempty"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

// Event is one durable session event in wire form.
type Event struct {
	Type            string          `json:"type"`
	Seq             int64           `json:"seq"`
	Time            int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	SurfaceOp       string          `json:"surfaceOp,omitempty"`
	SourceEventSeqs []int64         `json:"sourceEventSeqs,omitempty"`
	Ignorable       bool            `json:"ignorable,omitempty"`
}

// EncodeData marshals an event payload.
func EncodeData(value any) (json.RawMessage, error) {
	if raw, ok := value.(json.RawMessage); ok {
		return raw, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("session: encode event data: %w", err)
	}
	return data, nil
}

// Session is one opened event log.
type Session struct {
	store  *Store
	dir    string
	header Header

	mu     sync.Mutex
	events []Event
	file   *os.File
	subs   map[uint64]chan Event
	nextID uint64

	feedSubs       map[uint64]chan FeedItem
	nextFeedID     uint64
	revision       int64
	attemptCounter int64
	active         *AssistantAttempt

	busy   bool
	closed bool
}

// ID returns the session id.
func (s *Session) ID() string { return s.header.ID }

// Dir returns the directory that holds the log.
func (s *Session) Dir() string { return s.dir }

// Header returns the session header.
func (s *Session) Header() Header { return s.header }

// Cwd returns the session working directory.
func (s *Session) Cwd() string { return s.header.Cwd }

// LastSeq returns the newest durable seq, or -1 for an empty log.
func (s *Session) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeqLocked()
}

func (s *Session) lastSeqLocked() int64 {
	if len(s.events) == 0 {
		return -1
	}
	return s.events[len(s.events)-1].Seq
}

// Events returns a copy of every durable event.
func (s *Session) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// SetBusy records whether an agent run currently owns this session.
func (s *Session) SetBusy(busy bool) {
	s.mu.Lock()
	s.busy = busy
	s.mu.Unlock()
}

// Busy reports whether an agent run currently owns this session.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// Append records one event, persists it, and notifies subscribers.
func (s *Session) Append(typ string, data any) (Event, error) {
	raw, err := EncodeData(data)
	if err != nil {
		return Event{}, err
	}
	return s.append(Event{Type: typ, Data: raw})
}

// AppendSurface records a surface event that appends to the transcript.
func (s *Session) AppendSurface(typ string, data any) (Event, error) {
	raw, err := EncodeData(data)
	if err != nil {
		return Event{}, err
	}
	return s.append(Event{Type: typ, Data: raw, SurfaceOp: "append"})
}

func (s *Session) append(event Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Event{}, errors.New("session: log is closed")
	}
	event.Seq = s.lastSeqLocked() + 1
	if event.Time == 0 {
		event.Time = time.Now().UnixMilli()
	}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("session: encode event: %w", err)
	}
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return Event{}, fmt.Errorf("session: append event: %w", err)
	}
	s.events = append(s.events, event)
	notify := event
	for _, ch := range s.subs {
		select {
		case ch <- notify:
		default:
			// A subscriber that cannot keep up can re-page from durable state.
		}
	}
	for id, ch := range s.feedSubs {
		select {
		case ch <- FeedItem{Event: &notify}:
		default:
			// Closing forces the follower to reconnect and rebaseline rather
			// than silently skipping a durable event or frame.
			delete(s.feedSubs, id)
			close(ch)
		}
	}
	return event, nil
}

// Subscribe streams events appended after the call. The returned function
// releases the subscription and must be called exactly once.
func (s *Session) Subscribe() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextID == 0 {
		s.nextID = 1
	}
	id := s.nextID
	s.nextID++
	ch := make(chan Event, 256)
	if s.subs == nil {
		s.subs = map[uint64]chan Event{}
	}
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(ch)
		}
	}
}

// SnapshotSubscribe copies the durable events and registers a live
// subscriber in one critical section, so a follower sees no gap and no
// duplicate between the snapshot and the live tail.
func (s *Session) SnapshotSubscribe() ([]Event, <-chan Event, func()) {
	s.mu.Lock()
	events := make([]Event, len(s.events))
	copy(events, s.events)
	if s.nextID == 0 {
		s.nextID = 1
	}
	id := s.nextID
	s.nextID++
	ch := make(chan Event, 256)
	if s.subs == nil {
		s.subs = map[uint64]chan Event{}
	}
	s.subs[id] = ch
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(ch)
		}
	}
	return events, ch, cancel
}

// FollowSnapshot copies the durable events, captures the assistant-stream
// baseline, and registers one ordered live feed in a single critical section.
func (s *Session) FollowSnapshot() ([]Event, AssistantStreamBaseline, <-chan FeedItem, func()) {
	s.mu.Lock()
	events := make([]Event, len(s.events))
	copy(events, s.events)
	baseline := s.assistantBaselineLocked()
	if s.nextFeedID == 0 {
		s.nextFeedID = 1
	}
	id := s.nextFeedID
	s.nextFeedID++
	ch := make(chan FeedItem, 256)
	if s.feedSubs == nil {
		s.feedSubs = map[uint64]chan FeedItem{}
	}
	s.feedSubs[id] = ch
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.feedSubs[id]; ok {
			delete(s.feedSubs, id)
			close(ch)
		}
	}
	return events, baseline, ch, cancel
}

// Close flushes and releases the log file.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	for id, ch := range s.feedSubs {
		delete(s.feedSubs, id)
		close(ch)
	}
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

func newSession(store *Store, dir string, header Header, events []Event, file *os.File) *Session {
	return &Session{store: store, dir: dir, header: header, events: events, file: file}
}

func logPath(dir string) string { return filepath.Join(dir, FileName) }
