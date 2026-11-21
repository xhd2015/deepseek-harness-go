package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadsRealHarnessRoot is opt-in and points at a real DeepSeek Harness
// sessions directory. It is the only test that proves the reader against
// thousands of artifacts written by the Node host rather than by this code.
func TestReadsRealHarnessRoot(t *testing.T) {
	root := os.Getenv("DSH_GO_REAL_ROOT")
	if root == "" {
		t.Skip("DSH_GO_REAL_ROOT not set")
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.Compression() != CompressionZstd {
		t.Fatalf("real root compression = %q, want zstd", store.Compression())
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no sessions listed from a real root")
	}
	checked := 0
	for _, entry := range entries {
		if !filepath.IsAbs(entry.Header.Cwd) {
			t.Fatalf("session %s has a relative cwd %q", entry.Header.ID, entry.Header.Cwd)
		}
		// A real root still holds older readable generations; the reader accepts
		// v3 and v4 and refuses anything newer.
		if entry.Header.Version < MinReadableVersion || entry.Header.Version > FormatVersion {
			t.Fatalf("session %s has unsupported format v%d", entry.Header.ID, entry.Header.Version)
		}
		if checked >= 3 {
			continue
		}
		opened, err := store.OpenRead(entry.Header.ID)
		if err != nil {
			t.Fatalf("OpenRead(%s): %v", entry.Header.ID, err)
		}
		events := opened.Events()
		if len(events) == 0 {
			t.Fatalf("session %s opened with no events", entry.Header.ID)
		}
		for index, event := range events {
			if event.Seq != int64(index) {
				t.Fatalf("session %s event %d has seq %d", entry.Header.ID, index, event.Seq)
			}
		}
		if got := Baseline(opened.Header(), events); got.AsOfSeq != events[len(events)-1].Seq {
			t.Fatalf("baseline asOfSeq = %d", got.AsOfSeq)
		}
		if err := opened.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		store.Forget(entry.Header.ID)
		checked++
	}
	t.Logf("read %d listed sessions, decoded %d in full", len(entries), checked)
}
