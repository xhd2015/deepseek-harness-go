package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ListenFileName is the discovery record external clients read.
const ListenFileName = "web-listen.json"

// ListenRecord is what a second process needs to call this running server. It
// lives under the data directory, never under DSH's own home, so the two
// harnesses stay discoverable without sharing a state tree.
type ListenRecord struct {
	// PID is the serving process; a record whose process is gone is stale.
	PID int `json:"pid"`
	// Origin is the canonical loopback origin, for example http://127.0.0.1:3070.
	Origin string `json:"origin"`
	// Token is the process launch token accepted as Authorization: Bearer. It
	// is absent when authentication is disabled.
	Token string `json:"token,omitempty"`
}

// writeListenRecord publishes the record atomically under dir.
func writeListenRecord(dir string, record ListenRecord) (string, error) {
	if dir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("listen record: resolve directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("listen record: create directory: %w", err)
	}
	path := filepath.Join(abs, ListenFileName)
	raw, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("listen record: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("listen record: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("listen record: publish: %w", err)
	}
	return path, nil
}

// clearListenRecord removes the record this process wrote.
func clearListenRecord(dir string, pid int) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, ListenFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var record ListenRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.PID != pid {
		return
	}
	_ = os.Remove(path)
}

// ReadListenRecord reads a record whose process is still alive.
func ReadListenRecord(dir string) (*ListenRecord, error) {
	path := filepath.Join(dir, ListenFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var record ListenRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("listen record: %s is malformed", path)
	}
	if record.PID <= 0 || record.Origin == "" {
		return nil, fmt.Errorf("listen record: %s is incomplete", path)
	}
	return &record, nil
}
