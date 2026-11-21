package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DraftVersion is the persisted draft record version.
const DraftVersion = 1

// DraftsDirName is the unit directory under a harness home's storages.
const (
	DraftsUnit = "session_composer_drafts"
	DraftsDir  = "drafts"
)

// Drafts stores the composer text a client saves without submitting a prompt.
// With a directory it persists the DeepSeek Harness record layout, so a draft
// survives restart and another Harness process can read it.
type Drafts struct {
	mu    sync.Mutex
	dir   string
	texts map[string]string
}

type draftFile struct {
	Version int `json:"version"`
	Record  struct {
		Text string `json:"text"`
	} `json:"record"`
}

// OpenDrafts opens the draft store under a harness home's storages directory.
// An empty directory keeps drafts in memory.
func OpenDrafts(storagesDir string) (*Drafts, error) {
	store := &Drafts{texts: map[string]string{}}
	if strings.TrimSpace(storagesDir) == "" {
		return store, nil
	}
	abs, err := filepath.Abs(storagesDir)
	if err != nil {
		return nil, fmt.Errorf("drafts: resolve storage directory: %w", err)
	}
	store.dir = filepath.Join(abs, DraftsUnit, DraftsDir)
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return nil, fmt.Errorf("drafts: create storage directory: %w", err)
	}
	return store, nil
}

// Dir returns the record directory, or "" for an in-memory store.
func (d *Drafts) Dir() string { return d.dir }

func (d *Drafts) path(sessionID string) string {
	return filepath.Join(d.dir, sessionID+".json")
}

// Get returns one saved draft, or "" when none exists.
func (d *Drafts) Get(sessionID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if text, ok := d.texts[sessionID]; ok {
		return text
	}
	if d.dir == "" || sessionID == "" {
		return ""
	}
	raw, err := os.ReadFile(d.path(sessionID))
	if err != nil {
		return ""
	}
	var file draftFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return ""
	}
	d.texts[sessionID] = file.Record.Text
	return file.Record.Text
}

// Set replaces one draft durably. Empty text clears the record.
func (d *Drafts) Set(sessionID, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("drafts: session id is required")
	}
	if strings.TrimSpace(text) == "" {
		delete(d.texts, sessionID)
		if d.dir != "" {
			if err := os.Remove(d.path(sessionID)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("drafts: clear record: %w", err)
			}
		}
		return nil
	}
	d.texts[sessionID] = text
	if d.dir == "" {
		return nil
	}
	var file draftFile
	file.Version = DraftVersion
	file.Record.Text = text
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("drafts: encode record: %w", err)
	}
	tmp := d.path(sessionID) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("drafts: write record: %w", err)
	}
	if err := os.Rename(tmp, d.path(sessionID)); err != nil {
		return fmt.Errorf("drafts: publish record: %w", err)
	}
	return nil
}

// getDraft answers session/getDraft.
func (s *Service) getDraft(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if _, err := s.openSession(request.SessionID); err != nil {
		return nil, err
	}
	return map[string]any{"text": s.drafts.Get(request.SessionID)}, nil
}

// setDraft answers session/setDraft. Empty text clears the durable record.
func (s *Service) setDraft(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Text      string `json:"text"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if _, err := s.openSession(request.SessionID); err != nil {
		return nil, err
	}
	if err := s.drafts.Set(request.SessionID, request.Text); err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return map[string]any{"text": request.Text}, nil
}
