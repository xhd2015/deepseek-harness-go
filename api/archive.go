package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/xhd2015/deepseek-harness-go/mux"
)

// The archive set is registry-global state: which sessions a grouping surface
// hides. It is not a session's own property, and archiving deletes nothing — the
// transcript stays on disk and unarchiving puts the row back exactly where it was.
//
// DSH keeps this set above every session and replays it into each surface, so two
// backends must agree on what "archived" means even though only one of them holds
// the set. The file lives in this backend's data directory and is rewritten as a
// whole, because a partially written set would hide the wrong rows.

// archiveStore is the durable archive set.
type archiveStore struct {
	mu   sync.Mutex
	path string
	ids  []string
}

// archiveFileName is the file under the data directory that holds the set.
const archiveFileName = "archived-sessions.json"

// openArchive loads the archive set, tolerating a missing file.
func openArchive(dataDir string) (*archiveStore, error) {
	store := &archiveStore{}
	if strings.TrimSpace(dataDir) == "" {
		// No data directory means no durability; the set still works in memory so a
		// test or an embedded host is not forced to provide one.
		return store, nil
	}
	store.path = filepath.Join(dataDir, archiveFileName)
	body, err := os.ReadFile(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read archived sessions: %w", err)
	}
	var file struct {
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		// A corrupt set must not silently become an empty one: that would unhide
		// every archived row and look like the user's choice was ignored.
		return nil, fmt.Errorf("parse %s: %w", archiveFileName, err)
	}
	store.ids = dedupeIDs(file.ArchivedSessionIDs)
	return store, nil
}

// snapshot reports the set, sorted for a stable reply.
//
// It never returns nil: the wire contract is an array, and `null` would make a
// client that iterates the value crash on exactly the state a fresh install has.
func (a *archiveStore) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.ids))
	out = append(out, a.ids...)
	sort.Strings(out)
	return out
}

// contains reports whether one session is archived.
func (a *archiveStore) contains(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range a.ids {
		if id == sessionID {
			return true
		}
	}
	return false
}

// add includes one session and persists. An id already present is a no-op.
func (a *archiveStore) add(sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range a.ids {
		if id == sessionID {
			return nil
		}
	}
	next := append(append([]string(nil), a.ids...), sessionID)
	return a.commitLocked(next)
}

// remove drops one session and persists. A missing id is a no-op, which is what
// makes unarchive idempotent.
func (a *archiveStore) remove(sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := make([]string, 0, len(a.ids))
	for _, id := range a.ids {
		if id != sessionID {
			next = append(next, id)
		}
	}
	if len(next) == len(a.ids) {
		return nil
	}
	return a.commitLocked(next)
}

// commitLocked writes the set, replacing the file atomically.
func (a *archiveStore) commitLocked(next []string) error {
	if a.path == "" {
		a.ids = next
		return nil
	}
	body, err := json.MarshalIndent(map[string]any{"archivedSessionIds": next}, "", "  ")
	if err != nil {
		return err
	}
	// A rename is what makes the write visible-or-not, never half: a reader either
	// sees the old set or the new one.
	temp := a.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(temp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, a.path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	a.ids = next
	return nil
}

// dedupeIDs removes repeats while keeping the first spelling.
func dedupeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// archiveSession answers workspace/archiveSession.
//
// The order is contractual and comes from DSH: existence, then activity, then the
// write. Checking activity after the write would hide a row and then discover it
// could not be hidden, leaving the surface contradicting the host.
func (s *Service) archiveSession(args json.RawMessage) (any, error) {
	var request struct {
		SessionID    string `json:"sessionId"`
		StopActivity bool   `json:"stopActivity"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	sessionID := strings.TrimSpace(request.SessionID)
	if sessionID == "" {
		return nil, fail(CodeBadRequest, "sessionId is required")
	}
	if s.archived == nil {
		return nil, fail(CodeUnimplemented, "archive needs a data directory")
	}
	// An id already in the set resolves without checking anything: DSH returns
	// before asking, so a session that went active after being archived cannot make
	// a retry fail.
	if s.archived.contains(sessionID) {
		return map[string]any{"archivedSessionIds": s.archived.snapshot()}, nil
	}
	if _, err := s.openSession(sessionID); err != nil {
		if failure, ok := err.(*mux.Failure); ok && failure.Code == CodeNotFound {
			return nil, &mux.Failure{
				Code:    CodeNotFound,
				Message: sessionID + " does not exist",
				Details: map[string]any{"sessionId": sessionID},
			}
		}
		return nil, err
	}
	if !request.StopActivity && s.cfg.Runner != nil && s.cfg.Runner.Busy(sessionID) {
		return nil, &mux.Failure{
			Code:    CodeSessionActive,
			Message: "session is running; wait for it to finish or ask it to stop",
			Details: map[string]any{
				"sessionId": sessionID,
				// DSH reports the activity it observed; this backend has one
				// kind of running work, and naming it keeps the shape the same.
				"activity": []string{"working"},
			},
		}
	}
	if err := s.archived.add(sessionID); err != nil {
		return nil, fail(CodeInternal, "archive: "+err.Error())
	}
	if request.StopActivity && s.cfg.Runner != nil {
		// The stop is requested after the write and NOT awaited: the archive set is
		// what a provider's pre-step gate reads, so every wake the stop induces is
		// already blocked. Waiting would make the reply depend on how fast the turn
		// unwinds.
		s.cfg.Runner.Cancel(sessionID)
	}
	return map[string]any{"archivedSessionIds": s.archived.snapshot()}, nil
}

// unarchiveSession answers workspace/unarchiveSession.
//
// No existence check: removing an id cannot introduce an unknown one, so a set
// entry whose session was deleted still resolves. That is also why this is
// idempotent — an id that is not archived writes nothing and still returns the set.
func (s *Service) unarchiveSession(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	sessionID := strings.TrimSpace(request.SessionID)
	if sessionID == "" {
		return nil, fail(CodeBadRequest, "sessionId is required")
	}
	if s.archived == nil {
		return nil, fail(CodeUnimplemented, "archive needs a data directory")
	}
	if err := s.archived.remove(sessionID); err != nil {
		return nil, fail(CodeInternal, "unarchive: "+err.Error())
	}
	return map[string]any{"archivedSessionIds": s.archived.snapshot()}, nil
}

// archiveOrNil loads the set for one data directory, reporting a broken file to
// the log rather than failing construction: a corrupt archive list must not stop
// the backend from serving every other request.
func archiveOrNil(dataDir string) *archiveStore {
	store, err := openArchive(dataDir)
	if err != nil {
		return &archiveStore{}
	}
	return store
}
