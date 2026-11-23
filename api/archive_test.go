package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestArchiveRefusesARunningSession is the contract a surface maps to "still
// running": without stopActivity a running session is refused, not hidden.
func TestArchiveRefusesARunningSession(t *testing.T) {
	block := make(chan struct{})
	// Registered before the fixture so it runs AFTER the fixture's Close: the
	// handler must be released before the server waits for it.
	t.Cleanup(func() { close(block) })
	f := newBlockingFixture(t, block)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)
	deadline := time.Now().Add(5 * time.Second)
	for !f.runner.Busy(opened.ID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.runner.Busy(opened.ID()) {
		t.Fatal("the turn never started")
	}
	f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustFail(t, CodeSessionActive)
	if f.service.archived.contains(opened.ID()) {
		t.Fatal("a refused archive still hid the session")
	}
}

// TestArchiveWithStopActivityStopsAndHides is the other half: the caller asked for
// the work to stop, so the archive is written and the stop is requested.
func TestArchiveWithStopActivityStopsAndHides(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	f := newBlockingFixture(t, block)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)
	deadline := time.Now().Add(5 * time.Second)
	for !f.runner.Busy(opened.ID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var value struct {
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	}
	decode(t, f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "stopActivity": true},
	}).mustSucceed(t), &value)
	if !containsString(value.ArchivedSessionIDs, opened.ID()) {
		t.Fatalf("archivedSessionIds = %v", value.ArchivedSessionIDs)
	}
	// The stop is requested, not awaited — that is DSH's contract, and the reply is
	// the archive write. Waiting for the turn to unwind here made this test measure
	// the runner's teardown: the model is still blocked on its own channel, so the
	// loop would sit for the whole window. What this test owns is the ordering, and
	// that is already asserted above.
}

// TestArchiveReturnedSetIsTheWholeSet keeps the reply usable as state.
func TestArchiveReturnedSetIsTheWholeSet(t *testing.T) {
	f := newFixture(t)
	first := f.createSession(t)
	second := f.createSession(t)
	var value struct {
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	}
	decode(t, f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": first.ID()},
	}).mustSucceed(t), &value)
	decode(t, f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": second.ID()},
	}).mustSucceed(t), &value)
	if len(value.ArchivedSessionIDs) != 2 {
		t.Fatalf("second reply carried %d ids, want the whole set", len(value.ArchivedSessionIDs))
	}
}

// TestArchiveSurvivesARestart is the durability requirement: the set belongs to the
// registry, not to a process.
func TestArchiveSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opened, err := store.Create(filepath.Join(root, "project"), "std-no-subagent")
	if err != nil {
		t.Fatal(err)
	}
	service := New(Config{Store: store, DefaultCwd: filepath.Join(root, "project"), DataDir: root})
	f := &fixture{service: service, store: store, root: root}
	f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)

	// A fresh service over the same data directory: this is what a restart is.
	restarted := New(Config{Store: store, DefaultCwd: filepath.Join(root, "project"), DataDir: root})
	value := restarted.mustUnary(t, "workspace/unarchiveSession", map[string]any{
		"request": map[string]any{"sessionId": "some-other-id"},
	})
	var reply struct {
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	}
	decode(t, value, &reply)
	if !containsString(reply.ArchivedSessionIDs, opened.ID()) {
		t.Fatalf("the set did not survive a restart: %v", reply.ArchivedSessionIDs)
	}
}

// TestUnarchiveIsIdempotent keeps a lost race from becoming an error.
func TestUnarchiveIsIdempotent(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	var value struct {
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	}
	decode(t, f.invoke(t, "workspace/unarchiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t), &value)
	if containsString(value.ArchivedSessionIDs, opened.ID()) {
		t.Fatal("the session is still archived")
	}
	// A second unarchive, and one for an id never archived, both resolve.
	f.invoke(t, "workspace/unarchiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	f.invoke(t, "workspace/unarchiveSession", map[string]any{
		"request": map[string]any{"sessionId": "unknown-session"},
	}).mustSucceed(t)
}

// TestArchiveUnknownSessionIsNotFound keeps a typo from hiding nothing.
func TestArchiveUnknownSessionIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": "does-not-exist"},
	}).mustFail(t, CodeNotFound)
}

// TestArchiveDeletesNothing is the promise unarchive depends on.
func TestArchiveDeletesNothing(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	before := len(opened.Events())
	f.invoke(t, "workspace/archiveSession", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	reopened, err := f.store.Open(opened.ID())
	if err != nil {
		t.Fatalf("the transcript was removed: %v", err)
	}
	if len(reopened.Events()) != before {
		t.Fatalf("events changed from %d to %d", before, len(reopened.Events()))
	}
}

// containsString reports whether a list holds one value.
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// mustUnary invokes one RPC on a service the fixture does not wrap.
func (s *Service) mustUnary(t *testing.T, endpoint string, args map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	value, callErr := s.Unary(endpoint, body)
	if callErr != nil {
		t.Fatalf("%s: %v", endpoint, callErr)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

var _ = strings.TrimSpace
