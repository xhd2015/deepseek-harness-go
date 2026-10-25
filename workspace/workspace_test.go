package workspace

import (
	"encoding/hex"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIDOfIsStableAndPathSpecific(t *testing.T) {
	first := IDOf("/Users/xhd2015/Projects/xhd2015/deepseek-harness-go")
	if len(first) != 3+12 {
		t.Fatalf("id = %q, want ws- plus 12 hex characters", first)
	}
	if second := IDOf("/Users/xhd2015/Projects/xhd2015/deepseek-harness-go"); second != first {
		t.Fatalf("id changed between calls: %q vs %q", first, second)
	}
	if other := IDOf("/Users/xhd2015/Projects/xhd2015/deepseek-harness"); other == first {
		t.Fatalf("different paths share the id %q", first)
	}
	if _, err := hex.DecodeString(first[3:]); err != nil {
		t.Fatalf("id suffix is not hex: %v", err)
	}
}

func TestAddIsIdempotentPerPath(t *testing.T) {
	registry := NewRegistry()
	if item, created := registry.Add(""); created || item != nil {
		t.Fatalf("Add(\"\") = %v, %v", item, created)
	}

	dir := filepath.Join(t.TempDir(), "project")
	item, created := registry.Add(dir)
	if !created {
		t.Fatal("first Add did not report a new workspace")
	}
	if item.Path != dir || item.Title != "project" || item.ID != IDOf(dir) {
		t.Fatalf("workspace = %+v", item)
	}
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %+v", item)
	}

	// The same directory spelled with a trailing separator and a parent hop is
	// the same workspace and must not be appended twice.
	again, created := registry.Add(dir + string(filepath.Separator) + ".")
	if created {
		t.Fatal("Add reported a second workspace for the same directory")
	}
	if again != item {
		t.Fatalf("Add returned a different row: %+v vs %+v", again, item)
	}
	if items := registry.List(); len(items) != 1 {
		t.Fatalf("workspaces = %d", len(items))
	}
}

func TestListGetAndSubscribe(t *testing.T) {
	registry := NewRegistry()
	root := t.TempDir()
	first, _ := registry.Add(filepath.Join(root, "one"))
	second, _ := registry.Add(filepath.Join(root, "two"))

	items := registry.List()
	if len(items) != 2 || items[0] != first || items[1] != second {
		t.Fatalf("List = %+v", items)
	}
	if got, ok := registry.Get(first.ID); !ok || got != first {
		t.Fatalf("Get(%s) = %v, %v", first.ID, got, ok)
	}
	if _, ok := registry.Get("ws-nope"); ok {
		t.Fatal("Get reported an unknown workspace")
	}

	changes, stop := registry.Subscribe()
	other, stopOther := registry.Subscribe()
	t.Cleanup(stop)
	t.Cleanup(stopOther)

	third, _ := registry.Add(filepath.Join(root, "three"))
	waitChange(t, changes)
	waitChange(t, other)

	// Touch only republishes known directories.
	registry.Touch(filepath.Join(root, "missing"))
	select {
	case <-changes:
		t.Fatal("Touch of an unregistered path notified subscribers")
	case <-time.After(20 * time.Millisecond):
	}
	registry.Touch(third.Path)
	waitChange(t, changes)
	waitChange(t, other)

	stopOther()
	registry.Add(filepath.Join(root, "four"))
	waitChange(t, changes)
	select {
	case <-other:
		t.Fatal("unsubscribed channel kept receiving changes")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestConcurrentAddRegistersOnce(t *testing.T) {
	registry := NewRegistry()
	dir := filepath.Join(t.TempDir(), "shared")
	const workers = 16
	var created atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if _, ok := registry.Add(dir); ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := created.Load(); got != 1 {
		t.Fatalf("created = %d, want exactly 1", got)
	}
	if items := registry.List(); len(items) != 1 {
		t.Fatalf("workspaces = %d", len(items))
	}
}

func waitChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a workspace change")
	}
}
