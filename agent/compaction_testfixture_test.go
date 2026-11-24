package agent

import (
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// newTestStore builds a store in a temporary directory.
func newTestStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestSession opens one empty session under that store.
func newTestSession(t *testing.T, store *session.Store) *session.Session {
	t.Helper()
	opened, err := store.Create(t.TempDir(), "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}
