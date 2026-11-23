// Package session stores the durable event log of one chat session using the
// DeepSeek Harness v4 layout: one immutable generation per session directory,
// concatenated checksummed Zstandard frames by default, and a cross-process
// write lease in session.lock.
//
// On-disk shape:
//
//	<root>/<project-key>/session-<id>/session.v4.jsonl.zstd
//
// The first frame is exactly the session header line; every later frame is one
// append batch of event lines. A plain `session.v4.jsonl` artifact uses the same
// row framing without compression. The two encodings never share one root.
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

// Event is one durable session event in wire form. The envelope is fixed:
// type, seq, time, data, and the optional surfaceOp, sourceEventSeqs and
// ignorable fields.
type Event struct {
	Type            string          `json:"type"`
	Seq             int64           `json:"seq"`
	Time            int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	SurfaceOp       *SurfaceOp      `json:"surfaceOp,omitempty"`
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
	store       *Store
	dir         string
	path        string
	header      Header
	compression Compression
	encoder     *frameEncoder
	hold        *lease
	writable    bool

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

// Path returns the absolute artifact path this session reads and appends.
func (s *Session) Path() string { return s.path }

// Header returns the session header.
func (s *Session) Header() Header { return s.header }

// Cwd returns the session working directory.
// Cwd reports the directory this session's calls resolve against: the newest
// recorded change, or the header's when the model never moved it.
func (s *Session) Cwd() string {
	s.mu.Lock()
	events := make([]Event, len(s.events))
	copy(events, s.events)
	s.mu.Unlock()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != EventWorkingDirectory {
			continue
		}
		var payload struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(events[i].Data, &payload) == nil && payload.Cwd != "" {
			return payload.Cwd
		}
	}
	return s.header.Cwd
}

// Writable reports whether this handle owns the write lease.
func (s *Session) Writable() bool { return s.writable }

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
	return s.append(Event{Type: typ, Data: raw, SurfaceOp: SplitSurface})
}

// AppendWithSource records one surface event that cites earlier sources.
func (s *Session) AppendWithSource(typ string, data any, sources []int64) (Event, error) {
	raw, err := EncodeData(data)
	if err != nil {
		return Event{}, err
	}
	event := Event{Type: typ, Data: raw, SourceEventSeqs: sources}
	if SurfaceType(typ) {
		event.SurfaceOp = SplitSurface
	}
	return s.append(event)
}

// AppendEvents commits one contiguous batch as a single physical frame, so a
// failed write cannot leave half a batch behind.
func (s *Session) AppendEvents(events []Event) ([]Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("session: log is closed")
	}
	if !s.writable {
		return nil, errors.New("session: log is read-only")
	}
	committed := make([]Event, 0, len(events))
	next := s.lastSeqLocked() + 1
	var lines []byte
	for _, event := range events {
		event.Seq = next
		next++
		if event.Time == 0 {
			event.Time = time.Now().UnixMilli()
		}
		if SurfaceType(event.Type) && event.SurfaceOp == nil {
			event.SurfaceOp = SplitSurface
		}
		line, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("session: encode event: %w", err)
		}
		lines = append(lines, line...)
		lines = append(lines, '\n')
		committed = append(committed, event)
	}
	if err := s.writeFrameLocked(lines); err != nil {
		return nil, err
	}
	for _, event := range committed {
		s.events = append(s.events, event)
	}
	for _, event := range committed {
		s.notifyLocked(event)
	}
	return committed, nil
}

func (s *Session) append(event Event) (Event, error) {
	committed, err := s.AppendEvents([]Event{event})
	if err != nil {
		return Event{}, err
	}
	return committed[0], nil
}

// writeFrameLocked encodes one append batch as one physical frame.
func (s *Session) writeFrameLocked(lines []byte) error {
	var payload []byte
	if s.compression == CompressionZstd {
		payload = s.encoder.compressFrame(lines)
	} else {
		payload = lines
	}
	if _, err := s.file.Write(payload); err != nil {
		return fmt.Errorf("session: append event: %w", err)
	}
	return nil
}

func (s *Session) notifyLocked(event Event) {
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

// Close flushes and releases the log file and the write lease.
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
	var firstErr error
	if s.file != nil {
		if err := s.file.Sync(); err != nil {
			firstErr = err
		}
		if err := s.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.file = nil
	}
	if s.encoder != nil {
		s.encoder.release()
		s.encoder = nil
	}
	if err := s.hold.release(); err != nil && firstErr == nil {
		firstErr = err
	}
	s.hold = nil
	return firstErr
}

// ReadHeader reads one header line from an artifact byte prefix.
func readHeaderPrefix(compression Compression, raw []byte) (Header, int, error) {
	if compression == CompressionZstd {
		frames, _, err := zstdFrames(raw)
		if err != nil {
			return Header{}, 0, err
		}
		if len(frames) == 0 {
			return Header{}, 0, errTornTail
		}
		decoder, err := newFrameDecoder()
		if err != nil {
			return Header{}, 0, err
		}
		defer decoder.release()
		plain, err := decoder.decompressFrame(raw[frames[0].start:frames[0].end])
		if err != nil {
			return Header{}, 0, err
		}
		line, _, err := splitLine(plain)
		if err != nil {
			return Header{}, 0, err
		}
		// This store materializes the header alone in its first frame, but an
		// artifact written by an earlier host may carry the header and its
		// first batch together; only the first line is the header either way.
		header, err := DecodeHeader(line)
		return header, frames[0].end, err
	}
	line, _, err := splitLine(raw)
	if err != nil {
		return Header{}, 0, err
	}
	header, err := DecodeHeader(line)
	if err != nil {
		return Header{}, 0, err
	}
	return header, len(line) + 1, nil
}

// splitLine returns the first newline-terminated line and the remaining bytes.
func splitLine(raw []byte) ([]byte, []byte, error) {
	for index, b := range raw {
		if b == '\n' {
			return raw[:index], raw[index+1:], nil
		}
	}
	return nil, nil, errTornTail
}

// decodeArtifact decodes a whole artifact into its header and events.
func decodeArtifact(raw []byte, compression Compression) (Header, []Event, int, error) {
	plain, err := decodeLog(raw, compression)
	if err != nil {
		return Header{}, nil, 0, err
	}
	header, rest, err := splitLine(plain)
	if err != nil {
		return Header{}, nil, 0, err
	}
	parsed, err := DecodeHeader(header)
	if err != nil {
		return Header{}, nil, 0, err
	}
	var events []Event
	next := int64(0)
	for len(rest) > 0 {
		var line []byte
		line, rest, err = splitLine(rest)
		if err != nil {
			break
		}
		if len(line) == 0 {
			continue
		}
		event, err := DecodeEvent(line, next)
		if err != nil {
			return Header{}, nil, 0, err
		}
		next++
		events = append(events, event)
	}
	return parsed, events, len(plain), nil
}

// logDir resolves one session directory for an id, accepting both the canonical
// encoded segment and the legacy `session-<uuid>` spelling already on disk.
func (s *Store) logDir(id string) (string, error) {
	normalized := NormalizeSessionID(id)
	segment, err := encodeSegment(normalized)
	if err != nil {
		return "", err
	}
	projects, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("session: list root: %w", err)
	}
	// A store written before compatible ids carried the bare UUID in the header
	// but the prefixed name on disk; all spellings resolve to one directory.
	names := []string{segment, NormalizeSessionID(id), normalized}
	var matches []string
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectPath := filepath.Join(s.root, project.Name())
		for _, name := range names {
			if name == "" {
				continue
			}
			candidate := filepath.Join(projectPath, name)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				matches = append(matches, candidate)
			}
		}
	}
	unique := map[string]bool{}
	var deduped []string
	for _, match := range matches {
		if unique[match] {
			continue
		}
		unique[match] = true
		deduped = append(deduped, match)
	}
	switch len(deduped) {
	case 0:
		return "", ErrNotFound
	case 1:
		return deduped[0], nil
	default:
		return "", fmt.Errorf("session: id %q appears in %d project directories", id, len(deduped))
	}
}

func newSession(store *Store, dir, path string, header Header, compression Compression,
	events []Event, file *os.File, encoder *frameEncoder, hold *lease) *Session {
	return &Session{
		store: store, dir: dir, path: path, header: header,
		compression: compression, events: events, file: file,
		encoder: encoder, hold: hold, writable: file != nil,
	}
}

// Surface reports the events the model is currently shown, in order.
//
// A compaction replaces a span rather than deleting it, so the surface is what a
// reader reconstructs from the replace ops — not the raw log. It is what a
// pressure check must measure, because bytes already replaced are no longer sent.
func (s *Session) Surface() []Event {
	events := s.Events()
	replaced := map[int64]bool{}
	for _, event := range events {
		if event.SurfaceOp == nil || !event.SurfaceOp.IsReplace() {
			continue
		}
		for seq := event.SurfaceOp.StartSeq; seq <= event.SurfaceOp.EndSeq; seq++ {
			replaced[seq] = true
		}
	}
	surface := make([]Event, 0, len(events))
	for _, event := range events {
		if replaced[event.Seq] {
			continue
		}
		surface = append(surface, event)
	}
	return surface
}

// AppendReplacing appends one surface event that replaces an existing range.
//
// This is how compaction and pruning rewrite history: the replacement is an
// ordinary event with a `replace` op, so a reader rebuilds the conversation from
// it and the replaced span is never sent again.
func (s *Session) AppendReplacing(typ string, data []byte, span SeqRange) (Event, error) {
	return s.append(Event{
		Type:      typ,
		Data:      data,
		SurfaceOp: &SurfaceOp{Replace: true, StartSeq: span.Start, EndSeq: span.End},
	})
}

// AppendIgnorable records one event a compatible reader may skip.
//
// This is how a preference is stored: a session written by a build that predates
// the preference must still open, so the record says of itself that ignoring it is
// allowed.
func (s *Session) AppendIgnorable(typ string, data any) (Event, error) {
	raw, err := EncodeData(data)
	if err != nil {
		return Event{}, err
	}
	return s.append(Event{Type: typ, Data: raw, Ignorable: true})
}
