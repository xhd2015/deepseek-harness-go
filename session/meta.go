package session

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ModelSelection is one complete model choice.
type ModelSelection struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// ListEntry is a header-only view of a cold session.
type ListEntry struct {
	Header         Header
	LastSeq        int64
	UpdatedAt      int64
	Blank          bool
	Title          string
	LastPromptAt   *int64
	ModelSelection *ModelSelection
	Busy           bool
}

// summarize folds the persisted events into the facts a list row needs.
func summarize(header Header, events []Event) ListEntry {
	entry := ListEntry{
		Header:    header,
		LastSeq:   -1,
		UpdatedAt: header.CreatedAt,
		Blank:     true,
	}
	for _, event := range events {
		entry.LastSeq = event.Seq
		if event.Time > 0 {
			entry.UpdatedAt = event.Time
		}
		switch event.Type {
		case EventTurnStart:
			entry.Blank = false
		case EventSessionTitle:
			if title, ok := eventData[struct {
				Title string `json:"title"`
			}](event); ok && strings.TrimSpace(title.Title) != "" {
				entry.Title = title.Title
			}
		case EventUserMessage:
			if event.SurfaceOp != "" && event.Time > 0 {
				at := event.Time
				entry.LastPromptAt = &at
			}
		case EventModelSelection:
			if selection, ok := eventData[ModelSelection](event); ok && selection.Provider != "" {
				current := selection
				entry.ModelSelection = &current
			}
		}
	}
	return entry
}

// DisplayTitle returns the title shown in lists.
func (e ListEntry) DisplayTitle() string {
	if strings.TrimSpace(e.Title) != "" {
		return e.Title
	}
	if cwd := strings.TrimSpace(e.Header.Cwd); cwd != "" {
		return cwd
	}
	return e.Header.ID
}

func eventData[T any](event Event) (T, bool) {
	var value T
	if len(event.Data) == 0 {
		return value, false
	}
	if err := json.Unmarshal(event.Data, &value); err != nil {
		return value, false
	}
	return value, true
}

// NewID returns a random lowercase UUID v4 string.
func NewID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand is effectively infallible here; stay useful if it is not.
		binary.BigEndian.PutUint64(raw[0:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(raw[8:16], uint64(os.Getpid()))
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16],
	)
}

// Slug converts a working directory into the directory name the session log
// lives under, matching the Harness layout: /a/b/c becomes --a-b-c--.
func Slug(cwd string) string {
	trimmed := strings.TrimSpace(cwd)
	trimmed = strings.Trim(trimmed, "/\\")
	if trimmed == "" {
		return "--root--"
	}
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"\x00", "-",
	)
	return "--" + replacer.Replace(trimmed) + "--"
}
