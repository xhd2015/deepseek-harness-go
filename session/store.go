package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound reports an unknown session id.
var ErrNotFound = errors.New("session: not found")

// Store owns the sessions under one root directory.
type Store struct {
	root string

	mu       sync.Mutex
	createMu sync.Mutex
	open     map[string]*Session
}

// NewStore prepares root and returns an empty store.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("session: root directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("session: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("session: create root: %w", err)
	}
	return &Store{root: abs, open: map[string]*Session{}}, nil
}

// Root returns the absolute sessions root.
func (s *Store) Root() string { return s.root }

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
	if strings.ContainsAny(id, "/\\..\x00") || len(id) > 128 {
		return nil, fmt.Errorf("session: invalid id %q", id)
	}
	return s.create(cwd, agentPreset, id)
}

func (s *Store) create(cwd, agentPreset, forcedID string) (*Session, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()

	id := forcedID
	if id == "" {
		id = NewID()
	}
	if strings.TrimSpace(cwd) == "" {
		if current, err := os.Getwd(); err == nil {
			cwd = current
		}
	}
	dir := filepath.Join(s.root, Slug(cwd), DirPrefix+id)
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create session directory: %w", err)
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
	file, err := os.OpenFile(logPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: create log: %w", err)
	}
	line, err := json.Marshal(header)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("session: encode header: %w", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return nil, fmt.Errorf("session: write header: %w", err)
	}
	opened := newSession(s, dir, header, nil, file)
	s.mu.Lock()
	s.open[id] = opened
	s.mu.Unlock()
	return opened, nil
}

// Open loads a session by id, reusing an already open handle.
func (s *Store) Open(id string) (*Session, error) {
	s.mu.Lock()
	if existing, ok := s.open[id]; ok {
		s.mu.Unlock()
		return existing, nil
	}
	s.mu.Unlock()

	dir, err := s.findDir(id)
	if err != nil {
		return nil, err
	}
	header, events, err := readLog(logPath(dir))
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(logPath(dir), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open log: %w", err)
	}
	opened := newSession(s, dir, header, events, file)

	s.mu.Lock()
	if existing, ok := s.open[id]; ok {
		s.mu.Unlock()
		file.Close()
		return existing, nil
	}
	s.open[id] = opened
	s.mu.Unlock()
	return opened, nil
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
	delete(s.open, id)
	s.mu.Unlock()
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

// List summarizes every session under the root, newest activity first.
func (s *Store) List() ([]ListEntry, error) {
	cwdDirs, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("session: list root: %w", err)
	}
	var entries []ListEntry
	for _, cwdDir := range cwdDirs {
		if !cwdDir.IsDir() {
			continue
		}
		sessionDirs, err := os.ReadDir(filepath.Join(s.root, cwdDir.Name()))
		if err != nil {
			continue
		}
		for _, sessionDir := range sessionDirs {
			if !sessionDir.IsDir() || !strings.HasPrefix(sessionDir.Name(), DirPrefix) {
				continue
			}
			dir := filepath.Join(s.root, cwdDir.Name(), sessionDir.Name())
			header, events, err := readLog(logPath(dir))
			if err != nil {
				// One unreadable log must not hide the rest of the list.
				continue
			}
			entry := summarize(header, events)
			s.mu.Lock()
			if opened, ok := s.open[header.ID]; ok {
				entry.Busy = opened.Busy()
			}
			s.mu.Unlock()
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UpdatedAt != entries[j].UpdatedAt {
			return entries[i].UpdatedAt > entries[j].UpdatedAt
		}
		return entries[i].Header.ID < entries[j].Header.ID
	})
	return entries, nil
}

func (s *Store) findDir(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", ErrNotFound
	}
	if strings.ContainsRune(id, os.PathSeparator) || strings.Contains(id, "..") {
		return "", ErrNotFound
	}
	cwdDirs, err := os.ReadDir(s.root)
	if err != nil {
		return "", fmt.Errorf("session: list root: %w", err)
	}
	for _, cwdDir := range cwdDirs {
		if !cwdDir.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, cwdDir.Name(), DirPrefix+id)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", ErrNotFound
}

// readLog parses a session log file.
func readLog(path string) (Header, []Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return Header{}, nil, fmt.Errorf("session: read log: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	var header Header
	first := true
	var events []Event
	var nextSeq int64
	for {
		line, err := readLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Header{}, nil, fmt.Errorf("session: read log: %w", err)
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if first {
			if err := json.Unmarshal(line, &header); err != nil {
				return Header{}, nil, fmt.Errorf("session: malformed header in %s: %w", path, err)
			}
			if header.Type != "session" {
				return Header{}, nil, fmt.Errorf("session: %s is not a session log", path)
			}
			if header.Version != FormatVersion {
				return Header{}, nil, fmt.Errorf("session: %s uses format version %d, want %d", path, header.Version, FormatVersion)
			}
			nextSeq = 0
			first = false
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return Header{}, nil, fmt.Errorf("session: malformed event in %s: %w", path, err)
		}
		if event.Seq < nextSeq {
			event.Seq = nextSeq
		}
		nextSeq = event.Seq + 1
		events = append(events, event)
	}
	if first {
		return Header{}, nil, fmt.Errorf("session: %s is empty", path)
	}
	return header, events, nil
}

// readLine returns one line without its trailing newline. A final line without
// a newline is returned as-is; a truncated trailing write is ignored.
func readLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			// Ignore a partial final write: the rest of the log is intact.
			return nil, io.EOF
		}
		return nil, err
	}
	return line[:len(line)-1], nil
}
