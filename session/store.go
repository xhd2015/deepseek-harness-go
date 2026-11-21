package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound reports an unknown session id.
var ErrNotFound = errors.New("session: not found")

// Store owns the sessions under one root directory.
type Store struct {
	root        string
	compression Compression

	mu       sync.Mutex
	createMu sync.Mutex
	open     map[string]*Session
}

// Config configures a Store.
type Config struct {
	// Root is the sessions directory. Required.
	Root string
	// Compression selects the physical encoding for sessions this store
	// creates. Empty detects an existing root and otherwise uses zstd.
	Compression Compression
}

// NewStore prepares root and returns a store that writes zstd, matching the
// DeepSeek Harness default.
func NewStore(root string) (*Store, error) {
	return NewStoreWithConfig(Config{Root: root})
}

// NewStoreWithConfig prepares root and returns a store.
func NewStoreWithConfig(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("session: root directory is required")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("session: resolve root: %w", err)
	}
	compression := cfg.Compression
	if compression == "" {
		detected, err := detectCompression(abs)
		if err != nil {
			return nil, err
		}
		compression = detected
	}
	if compression != CompressionZstd && compression != CompressionNone {
		return nil, fmt.Errorf("session: unknown compression %q", compression)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("session: create root: %w", err)
	}
	return &Store{root: abs, compression: compression, open: map[string]*Session{}}, nil
}

// detectCompression inspects an existing root. An empty or absent root uses the
// default; a root whose sessions already use one encoding keeps it so the two
// never mix.
func detectCompression(root string) (Compression, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return CompressionZstd, nil
		}
		return "", fmt.Errorf("session: list root: %w", err)
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectPath := filepath.Join(root, project.Name())
		sessions, err := os.ReadDir(projectPath)
		if err != nil {
			continue
		}
		for _, dir := range sessions {
			if !dir.IsDir() {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(projectPath, dir.Name()))
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if _, compression, ok := ParseGenerationName(entry.Name()); ok {
					return compression, nil
				}
			}
		}
	}
	return CompressionZstd, nil
}

// Root returns the absolute sessions root.
func (s *Store) Root() string { return s.root }

// Compression returns the encoding this store writes.
func (s *Store) Compression() Compression { return s.compression }

// Create writes a new session log and returns the opened session.
func (s *Store) Create(cwd, agentPreset string) (*Session, error) {
	return s.create(cwd, agentPreset, "")
}

// CreateWithID is Create with a caller-supplied id. Retrying with the same
// id and cwd while the session is still blank returns the existing session,
// which lets clients safely retry a create whose response they lost.
func (s *Store) CreateWithID(cwd, agentPreset, id string) (*Session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("session: id is required")
	}
	if len(id) > 256 {
		return nil, fmt.Errorf("session: invalid id %q", id)
	}
	return s.create(cwd, agentPreset, NormalizeSessionID(id))
}

func (s *Store) create(cwd, agentPreset, forcedID string) (*Session, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()

	id := forcedID
	if id == "" {
		id = SessionPrefix + NewID()
	}
	if strings.TrimSpace(cwd) == "" {
		if current, err := os.Getwd(); err == nil {
			cwd = current
		}
	}
	dir, err := SessionDir(s.root, cwd, id)
	if err != nil {
		return nil, err
	}
	if forcedID != "" {
		if _, err := os.Stat(dir); err == nil {
			// A client that retries create with the same id must observe the
			// session it already made, not a spurious "already exists" error.
			// Only a still-blank session in the same cwd is reused; a session
			// that already has history is never silently reopened as new.
			existing, openErr := s.Open(forcedID)
			if openErr == nil && existing.LastSeq() < 0 &&
				sameCwd(existing.Cwd(), cwd) && existing.Header().AgentPreset == agentPreset {
				return existing, nil
			}
			return nil, fmt.Errorf("session: session %s already exists", forcedID)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("session: inspect session directory: %w", err)
		}
	}
	header := Header{
		Type:        "session",
		Version:     FormatVersion,
		ID:          id,
		CreatedAt:   time.Now().UnixMilli(),
		Cwd:         cwd,
		IsSeeded:    false,
		AgentPreset: agentPreset,
	}
	hold, err := acquireLease(dir, id)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, GenerationName(FormatVersion, s.compression))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = hold.release()
		return nil, fmt.Errorf("session: create log: %w", err)
	}
	line, err := EncodeHeader(header)
	if err != nil {
		file.Close()
		_ = hold.release()
		return nil, fmt.Errorf("session: encode header: %w", err)
	}
	encoder, err := newFrameEncoder()
	if err != nil {
		file.Close()
		_ = hold.release()
		return nil, err
	}
	var first []byte
	if s.compression == CompressionZstd {
		first = encoder.compressFrame(append(line, '\n'))
	} else {
		first = append(line, '\n')
	}
	if _, err := file.Write(first); err != nil {
		encoder.release()
		file.Close()
		_ = hold.release()
		return nil, fmt.Errorf("session: write header: %w", err)
	}
	if err := file.Sync(); err != nil {
		encoder.release()
		file.Close()
		_ = hold.release()
		return nil, fmt.Errorf("session: sync log: %w", err)
	}
	opened := newSession(s, dir, path, header, s.compression, nil, file, encoder, hold)
	s.mu.Lock()
	s.open[id] = opened
	s.mu.Unlock()
	return opened, nil
}

// CreateSeeded writes a fork child that inherits an exact event prefix. The
// header records the parent lineage, and the inherited cut is marked in the log
// by a session/end-seed event carrying inherited:true.
func (s *Store) CreateSeeded(cwd string, parent Header, events []Event) (*Session, error) {
	if !parent.IsSeeded {
		// The caller asks for a child; mint a fresh identity for it.
	}
	return s.createSeeded(cwd, parent, events)
}

func (s *Store) createSeeded(cwd string, parent Header, events []Event) (*Session, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()

	id := SessionPrefix + NewID()
	dir, err := SessionDir(s.root, cwd, id)
	if err != nil {
		return nil, err
	}
	header := Header{
		Type:            "session",
		Version:         FormatVersion,
		ID:              id,
		CreatedAt:       time.Now().UnixMilli(),
		Cwd:             cwd,
		ParentSession:   parent.ID,
		IsSeeded:        true,
		DelegationDepth: parent.DelegationDepth + 1,
		AgentPreset:     parent.AgentPreset,
		Origin:          "subagent",
	}
	hold, err := acquireLease(dir, id)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, GenerationName(FormatVersion, s.compression))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = hold.release()
		return nil, fmt.Errorf("session: create log: %w", err)
	}
	encoder, err := newFrameEncoder()
	if err != nil {
		file.Close()
		_ = hold.release()
		return nil, err
	}
	headerLine, err := EncodeHeader(header)
	if err != nil {
		encoder.release()
		file.Close()
		_ = hold.release()
		return nil, err
	}
	var payload []byte
	if s.compression == CompressionZstd {
		payload = encoder.compressFrame(append(headerLine, '\n'))
	} else {
		payload = append(headerLine, '\n')
	}
	if _, err := file.Write(payload); err != nil {
		encoder.release()
		file.Close()
		_ = hold.release()
		return nil, fmt.Errorf("session: write header: %w", err)
	}
	opened := newSession(s, dir, path, header, s.compression, nil, file, encoder, hold)
	if len(events) > 0 {
		if _, err := opened.AppendEvents(events); err != nil {
			_ = opened.Close()
			return nil, err
		}
	}
	s.mu.Lock()
	s.open[id] = opened
	s.mu.Unlock()
	return opened, nil
}

// Open loads a session by id, reusing an already open handle.
func (s *Store) Open(id string) (*Session, error) {
	return s.openSession(id, true)
}

// OpenRead opens a session for reading only, without taking the write lease.
// Observers must not block a live writer, and must never mutate the log.
func (s *Store) OpenRead(id string) (*Session, error) {
	return s.openSession(id, false)
}

func (s *Store) openSession(id string, writable bool) (*Session, error) {
	normalized := NormalizeSessionID(id)
	s.mu.Lock()
	if existing, ok := s.open[normalized]; ok {
		if !writable || existing.Writable() {
			s.mu.Unlock()
			return existing, nil
		}
		// A read handle cannot be promoted in place: drop it so the caller gets
		// the write lease and a fresh append target.
		delete(s.open, normalized)
		s.mu.Unlock()
		_ = existing.Close()
	} else {
		s.mu.Unlock()
	}

	dir, err := s.logDir(normalized)
	if err != nil {
		return nil, err
	}
	artifact, err := resolveGeneration(dir, s.compression)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(artifact.path)
	if err != nil {
		return nil, fmt.Errorf("session: read log: %w", err)
	}
	header, events, _, err := decodeArtifact(raw, artifact.compression)
	if err != nil {
		return nil, fmt.Errorf("session: %s: %w", artifact.path, err)
	}
	// A log written before ids carried the canonical prefix keeps its bare
	// header id; it is still this session, and the next write open repairs it.
	storedID := header.ID
	if NormalizeSessionID(storedID) != normalized {
		return nil, fmt.Errorf("session: corrupt log %s: header id %q does not match %q",
			artifact.path, storedID, normalized)
	}
	header.ID = normalized
	if artifact.compression != s.compression {
		return nil, fmt.Errorf("session: %s uses the %s encoding but this root writes %s",
			artifact.path, artifact.compression, s.compression)
	}

	var (
		file    *os.File
		encoder *frameEncoder
		hold    *lease
	)
	if writable {
		hold, err = acquireLease(dir, normalized)
		if err != nil {
			return nil, err
		}
		if storedID != normalized {
			// A log written before ids carried the canonical prefix keeps its
			// bare header id. Repairing it while this handle owns the lease is
			// what lets the Harness host open the artifact by its own identity
			// rule afterwards.
			if err := rewriteIdentity(artifact, header); err != nil {
				_ = hold.release()
				return nil, err
			}
		}
		file, err = os.OpenFile(artifact.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			_ = hold.release()
			return nil, fmt.Errorf("session: open log: %w", err)
		}
		encoder, err = newFrameEncoder()
		if err != nil {
			file.Close()
			_ = hold.release()
			return nil, err
		}
	}
	opened := newSession(s, dir, artifact.path, header, artifact.compression, events, file, encoder, hold)

	s.mu.Lock()
	if existing, ok := s.open[normalized]; ok {
		s.mu.Unlock()
		_ = opened.Close()
		return existing, nil
	}
	s.open[normalized] = opened
	s.mu.Unlock()
	return opened, nil
}

// rewriteIdentity replaces only the header record of an artifact whose header
// id is not canonical. The log content is preserved byte-for-byte otherwise, and
// the new file is published by rename so a crash cannot leave a partial header.
// The caller holds the session's write lease.
func rewriteIdentity(selected artifact, header Header) error {
	raw, err := os.ReadFile(selected.path)
	if err != nil {
		return fmt.Errorf("session: read log: %w", err)
	}
	plain, err := decodeLog(raw, selected.compression)
	if err != nil {
		return fmt.Errorf("session: %s: %w", selected.path, err)
	}
	_, rest, err := splitLine(plain)
	if err != nil {
		return fmt.Errorf("session: %s: %w", selected.path, err)
	}
	line, err := EncodeHeader(header)
	if err != nil {
		return fmt.Errorf("session: encode header: %w", err)
	}
	body := append(append([]byte{}, line...), '\n')
	body = append(body, rest...)
	var encoded []byte
	if selected.compression == CompressionZstd {
		encoded, err = encodeFrames(body[:len(line)+1], [][]byte{body[len(line)+1:]})
		if err != nil {
			return err
		}
	} else {
		encoded = body
	}
	tmp := selected.path + ".identity.tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return fmt.Errorf("session: write repaired log: %w", err)
	}
	if err := os.Rename(tmp, selected.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("session: publish repaired log: %w", err)
	}
	return nil
}

// sameCwd compares working directories after cleaning so a retry with a
// relative spelling still matches the header written by the first call.
func sameCwd(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// Forget drops the cached handle for a session without closing it; callers
// must have closed it first.
func (s *Store) Forget(id string) {
	s.mu.Lock()
	delete(s.open, NormalizeSessionID(id))
	s.mu.Unlock()
}

// Exists reports whether the store holds any generation of one id, without
// opening or activating it.
func (s *Store) Exists(id string) bool {
	normalized := NormalizeSessionID(id)
	if strings.TrimSpace(normalized) == "" {
		return false
	}
	dir, err := s.logDir(normalized)
	if err != nil {
		return false
	}
	_, err = resolveGeneration(dir, s.compression)
	return err == nil
}

// Close closes every cached session.
func (s *Store) Close() error {
	s.mu.Lock()
	opened := s.open
	s.open = map[string]*Session{}
	s.mu.Unlock()
	var firstErr error
	for _, item := range opened {
		if err := item.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// artifact is one resolved session generation.
type artifact struct {
	path        string
	dir         string
	version     int
	compression Compression
}

// resolveGeneration selects the highest canonical generation in one session
// directory. An unsupported newer generation is refused rather than replaced by
// an older readable one.
func resolveGeneration(dir string, compression Compression) (artifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return artifact{}, fmt.Errorf("session: read session directory: %w", err)
	}
	best := artifact{dir: dir, version: -1}
	for _, entry := range entries {
		version, enc, ok := ParseGenerationName(entry.Name())
		if !ok {
			continue
		}
		if version <= best.version {
			continue
		}
		best = artifact{
			path:        filepath.Join(dir, entry.Name()),
			dir:         dir,
			version:     version,
			compression: enc,
		}
	}
	if best.version < 0 {
		return artifact{}, ErrNotFound
	}
	if best.version > FormatVersion {
		return artifact{}, fmt.Errorf("session: %s uses unsupported format v%d (this build reads v%d)",
			best.path, best.version, FormatVersion)
	}
	if best.version < MinReadableVersion {
		return artifact{}, fmt.Errorf("session: %s uses unsupported format v%d (this build reads v%d or newer)",
			best.path, best.version, MinReadableVersion)
	}
	return best, nil
}

// List summarizes every session under the root, newest activity first. Sessions
// are scanned by a small worker pool: a real root holds over a thousand
// artifacts, and listing is on the path of every page load.
func (s *Store) List() ([]ListEntry, error) {
	dirs, err := s.sessionDirs()
	if err != nil {
		return nil, err
	}
	type result struct {
		entry ListEntry
		err   error
	}
	results := make([]result, len(dirs))
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				selected, err := resolveGeneration(dirs[index], s.compression)
				if err != nil {
					// One unreadable log must not hide the rest of the list.
					continue
				}
				entry, err := summarizeArtifact(selected)
				results[index] = result{entry: entry, err: err}
			}
		}()
	}
	for index := range dirs {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	var entries []ListEntry
	seen := map[string]bool{}
	for _, item := range results {
		if item.err != nil || item.entry.Header.ID == "" {
			continue
		}
		if seen[item.entry.Header.ID] {
			return nil, fmt.Errorf("session: duplicate session id %q under %s", item.entry.Header.ID, s.root)
		}
		seen[item.entry.Header.ID] = true
		s.mu.Lock()
		if opened, ok := s.open[item.entry.Header.ID]; ok {
			item.entry.Busy = opened.Busy()
		}
		s.mu.Unlock()
		entries = append(entries, item.entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UpdatedAt != entries[j].UpdatedAt {
			return entries[i].UpdatedAt > entries[j].UpdatedAt
		}
		return entries[i].Header.ID < entries[j].Header.ID
	})
	return entries, nil
}

// sessionDirs lists every candidate session directory under the root.
func (s *Store) sessionDirs() ([]string, error) {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: list root: %w", err)
	}
	var dirs []string
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectPath := filepath.Join(s.root, project.Name())
		entries, err := os.ReadDir(projectPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(projectPath, entry.Name()))
			}
		}
	}
	return dirs, nil
}

const (
	// summarizeFullLimit is the artifact size up to which a listing folds the
	// whole log. Larger logs are sampled instead of decoded.
	summarizeFullLimit = 1 << 20
	// summarizeHeadBytes bounds the plaintext folded from a large log's head.
	summarizeHeadBytes = 128 << 10
	// summarizeTailBytes bounds the plaintext folded from a large log's tail.
	summarizeTailBytes = 256 << 10
	// headerPrefixBytes is the first read for a header; headerMaxBytes bounds
	// growth when the first frame also carries a fork seed.
	headerPrefixBytes = 64 << 10
	headerMaxBytes    = 8 << 20
)

// summarizeArtifact folds only the facts a list row needs.
func summarizeArtifact(selected artifact) (ListEntry, error) {
	info, err := os.Stat(selected.path)
	if err != nil {
		return ListEntry{}, err
	}
	header, err := readHeaderFile(selected.path, selected.compression)
	if err != nil {
		return ListEntry{}, err
	}
	entry := ListEntry{Header: header, LastSeq: -1, UpdatedAt: header.CreatedAt, Blank: true}
	events, err := foldArtifact(selected, info.Size())
	if err != nil {
		return ListEntry{}, err
	}
	for _, event := range events {
		entry.apply(event)
	}
	if entry.UpdatedAt < header.CreatedAt {
		entry.UpdatedAt = header.CreatedAt
	}
	return entry, nil
}

// apply folds one event into the list row.
func (e *ListEntry) apply(event Event) {
	e.LastSeq = event.Seq
	if event.Time > 0 {
		e.UpdatedAt = event.Time
	}
	switch event.Type {
	case EventTurnStart:
		e.Blank = false
	case EventSessionTitle:
		if title, ok := eventData[struct {
			Title string `json:"title"`
		}](event); ok && strings.TrimSpace(title.Title) != "" {
			e.Title = title.Title
		}
	case EventUserMessage:
		e.Blank = false
		if event.SurfaceOp != nil && event.Time > 0 {
			at := event.Time
			e.LastPromptAt = &at
		}
	case EventModelSelection:
		if selection, ok := eventData[ModelSelection](event); ok && selection.Provider != "" {
			current := selection
			e.ModelSelection = &current
		}
	case EventRequestHeader:
		if used, ok := requestHeaderSelection(event); ok {
			e.LastUsed = used
		}
	}
}

// readHeaderFile reads just the first record of an artifact. The first frame
// may hold more than a header, so the read grows until that frame is complete.
func readHeaderFile(path string, compression Compression) (Header, error) {
	for limit := int64(headerPrefixBytes); limit <= headerMaxBytes; limit *= 2 {
		prefix, err := readRange(path, 0, limit)
		if err != nil {
			return Header{}, err
		}
		header, _, err := readHeaderPrefix(compression, prefix)
		if err == nil {
			return header, nil
		}
		if !errors.Is(err, errTornTail) || int64(len(prefix)) < limit {
			return Header{}, err
		}
	}
	prefix, err := readRange(path, 0, headerMaxBytes)
	if err != nil {
		return Header{}, err
	}
	header, _, err := readHeaderPrefix(compression, prefix)
	return header, err
}

// foldArtifact reads the events needed for one list row. Small logs are folded
// whole. A large log folds its head, where the policy preamble and first model
// selection live, plus its tail, where the latest title and activity live, so a
// root of hundreds of megabytes still lists quickly.
func foldArtifact(selected artifact, size int64) ([]Event, error) {
	if size <= summarizeFullLimit {
		raw, err := os.ReadFile(selected.path)
		if err != nil {
			return nil, err
		}
		_, events, _, err := decodeArtifact(raw, selected.compression)
		return events, err
	}
	head, err := readRange(selected.path, 0, summarizeHeadBytes)
	if err != nil {
		return nil, err
	}
	tail, err := readRange(selected.path, max64(0, size-summarizeTailBytes), summarizeTailBytes)
	if err != nil {
		return nil, err
	}
	headPlain, err := decodePartial(head, selected.compression, false)
	if err != nil {
		return nil, err
	}
	tailPlain, err := decodePartial(tail, selected.compression, true)
	if err != nil {
		return nil, err
	}
	return append(decodeEventTail(headPlain), decodeEventTail(tailPlain)...), nil
}

// decodePartial decompresses the complete frames in a byte range read from the
// middle of an artifact. A tail slice starts before a frame boundary, so the
// leading garbage is skipped to the first frame that scans.
func decodePartial(raw []byte, compression Compression, fromTail bool) ([]byte, error) {
	if compression == CompressionNone {
		return raw, nil
	}
	if fromTail {
		start, err := frameStartIn(raw)
		if err != nil {
			return nil, err
		}
		if start < 0 {
			return nil, nil
		}
		raw = raw[start:]
	}
	frames, _, err := zstdFrames(raw)
	if err != nil {
		return nil, err
	}
	decoder, err := newFrameDecoder()
	if err != nil {
		return nil, err
	}
	defer decoder.release()
	var plain []byte
	for _, frame := range frames {
		part, err := decoder.decompressFrame(raw[frame.start:frame.end])
		if err != nil {
			return nil, err
		}
		plain = append(plain, part...)
	}
	return plain, nil
}

// frameStartIn returns the first offset that begins a complete frame, or -1.
func frameStartIn(raw []byte) (int, error) {
	for offset := 0; offset+4 <= len(raw); offset++ {
		if uint32(raw[offset])|uint32(raw[offset+1])<<8|uint32(raw[offset+2])<<16|uint32(raw[offset+3])<<24 != zstdMagic {
			continue
		}
		frames, torn, err := zstdFrames(raw[offset:])
		if err == nil && len(frames) > 0 {
			return offset, nil
		}
		if err == nil && torn >= 0 && len(frames) == 0 {
			continue
		}
	}
	return -1, nil
}

// readRange reads at most limit bytes starting at offset.
func readRange(path string, offset, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if limit <= 0 {
		return nil, nil
	}
	buf := make([]byte, limit)
	read, err := file.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:read], nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// decodeEventTail parses event rows from a slice that does not start at seq 0.
// Rows that fail validation are skipped: a tail fold is a summary, not proof of
// the whole log.
func decodeEventTail(plain []byte) []Event {
	var events []Event
	for len(plain) > 0 {
		var line []byte
		line, plain, _ = splitLine(plain)
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil || event.Type == "" {
			continue
		}
		if event.Seq < 0 {
			continue
		}
		events = append(events, event)
	}
	return events
}

// readLog parses one session artifact from disk.
func readLog(path string) (Header, []Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Header{}, nil, fmt.Errorf("session: read log: %w", err)
	}
	compression := CompressionNone
	if strings.HasSuffix(path, ".zstd") {
		compression = CompressionZstd
	}
	header, events, _, err := decodeArtifact(raw, compression)
	return header, events, err
}
