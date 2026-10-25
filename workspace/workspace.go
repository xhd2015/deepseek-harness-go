// Package workspace tracks the directories the web client groups Sessions by.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sync"
	"time"
)

// Workspace is one directory Sessions belong to.
type Workspace struct {
	ID        string
	Path      string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Registry holds the Workspaces one backend serves. The set only grows: a
// Workspace is the directory a Session was created in, and Sessions outlive the
// process.
type Registry struct {
	mu    sync.Mutex
	items []*Workspace
	subs  map[chan struct{}]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{subs: make(map[chan struct{}]struct{})}
}

// Add registers path once and returns the Workspace. A path already served
// returns the existing row unchanged.
func (r *Registry) Add(path string) (*Workspace, bool) {
	if path == "" {
		return nil, false
	}
	path = filepath.Clean(path)
	r.mu.Lock()
	for _, item := range r.items {
		if item.Path == path {
			existing := item
			r.mu.Unlock()
			return existing, false
		}
	}
	now := time.Now().UTC()
	item := &Workspace{
		ID:        IDOf(path),
		Path:      path,
		Title:     filepath.Base(path),
		CreatedAt: now,
		UpdatedAt: now,
	}
	r.items = append(r.items, item)
	r.mu.Unlock()
	r.notify()
	return item, true
}

// Touch republishes one Workspace whose Session membership changed. The row
// itself is derived from the Session store, so callers only signal that readers
// must recompute it.
func (r *Registry) Touch(path string) {
	if path == "" {
		return
	}
	path = filepath.Clean(path)
	r.mu.Lock()
	found := false
	for _, item := range r.items {
		if item.Path == path {
			found = true
			break
		}
	}
	r.mu.Unlock()
	if found {
		r.notify()
	}
}

// Get returns one registered Workspace.
func (r *Registry) Get(id string) (*Workspace, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.items {
		if item.ID == id {
			return item, true
		}
	}
	return nil, false
}

// List returns the registered Workspaces in registration order.
func (r *Registry) List() []*Workspace {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]*Workspace, len(r.items))
	copy(items, r.items)
	return items
}

// Subscribe reports registry changes and a function that stops reporting them.
func (r *Registry) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

func (r *Registry) notify() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// IDOf derives the stable Workspace identity of one canonical path. Host
// identities are opaque and never reused, which a hash of the path gives us
// without a registry file.
func IDOf(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "ws-" + hex.EncodeToString(sum[:6])
}
