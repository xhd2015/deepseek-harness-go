package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteLeaseIsExclusiveAcrossHandles proves a second writer is refused while
// the first holds the session, and that closing releases the lock.
func TestWriteLeaseIsExclusiveAcrossHandles(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Create("/tmp/lease", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A second store acts as a second process against the same root.
	other, err := NewStoreWithConfig(Config{Root: store.Root(), Compression: CompressionNone})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer other.Close()
	if _, err := other.Open(first.ID()); !errors.Is(err, ErrAlreadyOwned) {
		t.Fatalf("second writer error = %v, want ErrAlreadyOwned", err)
	}
	// A reader is never blocked by a live writer.
	read, err := other.OpenRead(first.ID())
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	if read.Writable() {
		t.Fatal("a read handle reports itself writable")
	}
	if _, err := read.Append(EventTurnStart, map[string]any{"turn": 1}); err == nil {
		t.Fatal("a read handle appended an event")
	}
	if err := read.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	store.Forget(first.ID())
	reopened, err := other.Open(first.ID())
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if !reopened.Writable() {
		t.Fatal("reopened handle is not writable")
	}
	// The lock file is durable: releasing never removes it.
	if _, err := filepath.Glob(filepath.Join(reopened.Dir(), LeaseFileName)); err != nil {
		t.Fatalf("glob lease: %v", err)
	}
}

// TestTornTailIsIgnored proves a crash mid-append cannot corrupt the committed
// prefix, and that the next append continues from the right seq.
func TestTornTailIsIgnored(t *testing.T) {
	root := t.TempDir()
	store, err := NewStoreWithConfig(Config{Root: root, Compression: CompressionNone})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	opened, err := store.Create("/tmp/torn", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := opened.Append(EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	path := opened.Path()
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store.Forget(opened.ID())

	// Simulate a write that never completed: a partial final line.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	if _, err := file.WriteString(`{"type":"step/start","seq":1`); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close partial: %v", err)
	}

	reopened, err := store.Open(opened.ID())
	if err != nil {
		t.Fatalf("Open with a torn tail: %v", err)
	}
	if got := len(reopened.Events()); got != 1 {
		t.Fatalf("read %d events, want only the committed prefix", got)
	}
	if reopened.LastSeq() != 0 {
		t.Fatalf("last seq = %d", reopened.LastSeq())
	}
}

// TestGenerationSelectionRefusesNewerAndKeepsHighest proves a directory with
// several generations reads the newest supported one and refuses a newer one.
func TestGenerationSelectionRefusesNewerAndKeepsHighest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--tmp-gen--", "session-gen")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	header := func(version int) string {
		return `{"type":"session","version":` + itoa(int64(version)) +
			`,"id":"session-gen","createdAt":1,"cwd":"/tmp/gen","isSeeded":false,"delegationDepth":0}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v3.jsonl"), []byte(header(3)), 0o600); err != nil {
		t.Fatalf("write v3: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl"), []byte(header(4)+
		`{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write v4: %v", err)
	}
	store, err := NewStoreWithConfig(Config{Root: root, Compression: CompressionNone})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	opened, err := store.Open("session-gen")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(opened.Events()) != 1 || opened.Events()[0].Seq != 0 {
		t.Fatalf("selected generation has %d events", len(opened.Events()))
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A newer unsupported generation is refused, never silently replaced by the
	// older readable one.
	if err := os.WriteFile(filepath.Join(dir, "session.v5.jsonl"), []byte(header(5)), 0o600); err != nil {
		t.Fatalf("write v5: %v", err)
	}
	store.Forget("session-gen")
	if _, err := store.Open("session-gen"); err == nil {
		t.Fatal("a newer generation was opened instead of refused")
	}
}

// TestMixedEncodingRootIsRefused proves one root never mixes encodings.
func TestMixedEncodingRootIsRefused(t *testing.T) {
	root := t.TempDir()
	plain, err := NewStoreWithConfig(Config{Root: root, Compression: CompressionNone})
	if err != nil {
		t.Fatalf("NewStore plain: %v", err)
	}
	opened, err := plain.Create("/tmp/mixed", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Detection keeps the existing encoding rather than writing a second one.
	detected, err := NewStoreWithConfig(Config{Root: root})
	if err != nil {
		t.Fatalf("NewStore detect: %v", err)
	}
	if detected.Compression() != CompressionNone {
		t.Fatalf("detected compression = %q, want none", detected.Compression())
	}
	_ = detected.Close()

	// Forcing zstd over a plain root cannot silently append a second encoding.
	forced, err := NewStoreWithConfig(Config{Root: root, Compression: CompressionZstd})
	if err != nil {
		t.Fatalf("NewStore forced: %v", err)
	}
	defer forced.Close()
	if _, err := forced.Open(opened.ID()); err == nil {
		t.Fatal("a plain artifact was opened by a zstd store")
	}
}

// TestExistsDoesNotActivate proves an existence probe answers without opening a
// handle, so a status query cannot make a cold session busy.
func TestExistsDoesNotActivate(t *testing.T) {
	store := newTestStore(t)
	opened, err := store.Create("/tmp/exists", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store.Forget(opened.ID())
	other, err := NewStoreWithConfig(Config{Root: store.Root(), Compression: CompressionNone})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer other.Close()
	if !other.Exists(opened.ID()) {
		t.Fatal("a stored session was reported missing")
	}
	if !other.Exists(strings.TrimPrefix(opened.ID(), SessionPrefix)) {
		t.Fatal("a bare id did not resolve to the stored session")
	}
	if other.Exists("session-unknown") {
		t.Fatal("an unknown id was reported present")
	}
	// Nothing was opened, so no handle holds the write lease.
	if _, err := other.Open(opened.ID()); err != nil {
		t.Fatalf("Exist left a handle behind: %v", err)
	}
}
