// Package workspace tracks the directories the web client groups Sessions by.
//
// The registry is durable in the DeepSeek Harness layout: one `workspace.json`
// under `<home>/storages` holding a versioned unit, a global index of the
// ordered workspace ids, and one record per workspace. A registry opened
// without a directory stays in memory, which keeps tests and one-shot callers
// working without a filesystem.
package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UnitName and UnitVersion identify the persisted unit.
const (
	UnitName    = "workspace"
	UnitVersion = 2
	// FileName is the unit file inside the storages directory.
	FileName = "workspace.json"
)

// Workspace is one directory Sessions belong to.
type Workspace struct {
	ID        string
	Path      string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// SessionIDs is the persisted membership. The API recomputes it from the
	// session store when it can, because sessions outlive the registry.
	SessionIDs []string
}

// Registry holds the Workspaces one backend serves. With a directory it reads
// and writes the DSH unit file; without one it is memory-only.
type Registry struct {
	mu      sync.Mutex
	path    string
	items   []*Workspace
	global  globalIndex
	subs    map[chan struct{}]struct{}
	loading bool
}

// globalIndex is the unit's `global` section.
type globalIndex struct {
	Initialized        bool     `json:"initialized"`
	WorkspaceIDs       []string `json:"workspaceIds"`
	ArchivedSessionIDs []string `json:"archivedSessionIds"`
	PinnedSessionIDs   []string `json:"pinnedSessionIds"`
}

// unitFile is the persisted workspace unit.
type unitFile struct {
	Unit   unitHeader                           `json:"unit"`
	Global globalIndex                          `json:"global"`
	Tables map[string]map[string]map[string]any `json:"tables"`
}

type unitHeader struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// NewRegistry returns an empty in-memory registry.
func NewRegistry() *Registry {
	return &Registry{subs: make(map[chan struct{}]struct{})}
}

// Open loads the durable registry stored in dir, creating the directory when it
// does not exist. A missing unit file starts an empty registry.
func Open(dir string) (*Registry, error) {
	if dir == "" {
		return nil, fmt.Errorf("workspace: storage directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve storage directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create storage directory: %w", err)
	}
	registry := NewRegistry()
	registry.path = filepath.Join(abs, FileName)
	if err := registry.load(); err != nil {
		return nil, err
	}
	return registry, nil
}

// Path returns the unit file this registry persists to, or "" when memory-only.
func (r *Registry) Path() string { return r.path }

func (r *Registry) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("workspace: read %s: %w", r.path, err)
	}
	var unit unitFile
	if err := json.Unmarshal(raw, &unit); err != nil {
		return fmt.Errorf("workspace: %s is not a workspace unit: %w", r.path, err)
	}
	if unit.Unit.Name != UnitName {
		return fmt.Errorf("workspace: %s holds the %q unit, want %q", r.path, unit.Unit.Name, UnitName)
	}
	if unit.Unit.Version > UnitVersion {
		return fmt.Errorf("workspace: %s is version %d, this build reads up to %d",
			r.path, unit.Unit.Version, UnitVersion)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.global = unit.Global
	records := unit.Tables["workspaces"]
	for _, id := range unit.Global.WorkspaceIDs {
		record := records[id]
		if record == nil {
			continue
		}
		item := &Workspace{ID: id}
		item.Path, _ = record["path"].(string)
		item.Title, _ = record["title"].(string)
		if item.Title == "" {
			item.Title = filepath.Base(item.Path)
		}
		item.CreatedAt = parseTime(record["createdAt"])
		item.UpdatedAt = parseTime(record["updatedAt"])
		if ids, ok := record["sessionIds"].([]any); ok {
			for _, value := range ids {
				if text, ok := value.(string); ok {
					item.SessionIDs = append(item.SessionIDs, text)
				}
			}
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = item.UpdatedAt
		}
		r.items = append(r.items, item)
	}
	return nil
}

func parseTime(value any) time.Time {
	text, ok := value.(string)
	if !ok || text == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// persist writes the whole unit. Callers hold r.mu.
func (r *Registry) persistLocked() error {
	if r.path == "" {
		return nil
	}
	if r.global.ArchivedSessionIDs == nil {
		r.global.ArchivedSessionIDs = []string{}
	}
	if r.global.PinnedSessionIDs == nil {
		r.global.PinnedSessionIDs = []string{}
	}
	unit := unitFile{
		Unit:   unitHeader{Name: UnitName, Version: UnitVersion},
		Global: r.global,
		Tables: map[string]map[string]map[string]any{"workspaces": map[string]map[string]any{}},
	}
	records := unit.Tables["workspaces"]
	for _, item := range r.items {
		ids := item.SessionIDs
		if ids == nil {
			ids = []string{}
		}
		records[item.ID] = map[string]any{
			"path":       item.Path,
			"title":      item.Title,
			"sessionIds": ids,
			"createdAt":  item.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			"updatedAt":  item.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		}
	}
	raw, err := json.MarshalIndent(unit, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: encode unit: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("workspace: create storage directory: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("workspace: write unit: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("workspace: publish unit: %w", err)
	}
	return nil
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
	id := IDOf(path)
	if r.path != "" {
		// A persisted unit keys workspaces by an opaque Host UUID.
		id = NewID()
	}
	item := &Workspace{
		ID:        id,
		Path:      path,
		Title:     filepath.Base(path),
		CreatedAt: now,
		UpdatedAt: now,
	}
	r.items = append(r.items, item)
	r.global.WorkspaceIDs = append(r.global.WorkspaceIDs, item.ID)
	r.global.Initialized = true
	err := r.persistLocked()
	r.mu.Unlock()
	r.notify()
	if err != nil {
		// A registry that cannot persist still serves this process; the next
		// write reports the failure to its caller.
		return item, true
	}
	return item, true
}

// SetSessions records one Workspace's session membership.
func (r *Registry) SetSessions(path string, ids []string) {
	if path == "" {
		return
	}
	path = filepath.Clean(path)
	r.mu.Lock()
	changed := false
	for _, item := range r.items {
		if item.Path != path {
			continue
		}
		if len(item.SessionIDs) == len(ids) {
			same := true
			for index := range ids {
				if item.SessionIDs[index] != ids[index] {
					same = false
					break
				}
			}
			if same {
				break
			}
		}
		item.SessionIDs = append([]string(nil), ids...)
		item.UpdatedAt = time.Now().UTC()
		changed = true
		break
	}
	if changed {
		_ = r.persistLocked()
	}
	r.mu.Unlock()
	if changed {
		r.notify()
	}
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

// Global returns the archived and pinned session index the UI reads.
func (r *Registry) Global() (archived, pinned []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.global.ArchivedSessionIDs...),
		append([]string(nil), r.global.PinnedSessionIDs...)
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

// IDOf derives the stable in-memory Workspace identity of one canonical path.
// A persisting registry mints an opaque UUID instead, matching the Host file.
func IDOf(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "ws-" + hex.EncodeToString(sum[:6])
}

// NewID returns a random lowercase UUID v4 string.
func NewID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		now := time.Now().UnixNano()
		for index := range raw {
			raw[index] = byte(now >> (index * 3))
		}
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16],
	)
}
