package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadsCapturedRealLogs parses two logs captured from a real DeepSeek
// Harness host. They exercise event types, surface markers and projections that
// the synthetic fixtures do not, so a format drift fails here first.
func TestReadsCapturedRealLogs(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(filepath.Join("..", "testdata", "sessions"))); err != nil {
		t.Fatalf("copy fixtures: %v", err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(entries))
	}
	byID := make(map[string]ListEntry, len(entries))
	for _, entry := range entries {
		byID[entry.Header.ID] = entry
	}

	tests := map[string]struct {
		title     string
		lastSeq   int64
		model     *ModelSelection
		assistant bool
	}{
		"3defb2dd-dae4-4a58-bd06-d0bfb275dbac": {
			title:   "Draft Brief Plan for Todo 190",
			lastSeq: 19,
		},
		"39679a1f-ee75-4d30-b642-20db4a354ced": {
			title:     "Greeting",
			lastSeq:   22,
			model:     &ModelSelection{Provider: "deepseek-official", Model: "deepseek-flash", ReasoningEffort: "high"},
			assistant: true,
		},
	}
	for id, want := range tests {
		t.Run(id, func(t *testing.T) {
			entry, ok := byID[id]
			if !ok {
				t.Fatalf("session %s missing from List", id)
			}
			if entry.Blank {
				t.Error("captured session reported as blank")
			}
			if entry.Title != want.title {
				t.Errorf("title = %q, want %q", entry.Title, want.title)
			}
			if entry.LastSeq != want.lastSeq {
				t.Errorf("last seq = %d, want %d", entry.LastSeq, want.lastSeq)
			}
			if entry.Header.AgentPreset != "std-no-subagent" {
				t.Errorf("preset = %q", entry.Header.AgentPreset)
			}

			opened, err := store.Open(id)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			events := opened.Events()
			if got := int64(len(events)); got != want.lastSeq+1 {
				t.Fatalf("read %d events, want %d", got, want.lastSeq+1)
			}
			for index, event := range events {
				if event.Seq != int64(index) {
					t.Fatalf("event %d has seq %d", index, event.Seq)
				}
				if len(event.Data) == 0 || event.Time <= 0 {
					t.Fatalf("event %d is incomplete: %+v", index, event)
				}
			}

			// The baseline is what the chat page reads before any live event.
			baseline := Baseline(opened.Header(), events)
			if baseline.AsOfSeq != want.lastSeq {
				t.Errorf("baseline asOfSeq = %d", baseline.AsOfSeq)
			}
			got, ok := baseline.Values["modelSelection"].(map[string]any)
			if !ok {
				t.Fatalf("modelSelection face missing from %v", baseline.Values)
			}
			if got["lastUsed"] != nil {
				t.Errorf("lastUsed = %v, want nil", got["lastUsed"])
			}
			if want.model == nil {
				if got["next"] != nil {
					t.Errorf("unexpected model selection %v", got["next"])
				}
			} else if next, ok := got["next"].(*ModelSelection); !ok || *next != *want.model {
				t.Errorf("next = %v, want %+v", got["next"], *want.model)
			}

			// A page wide enough for the whole history returns every record.
			page := PageEvents(events, PageRequest{ThroughSeq: want.lastSeq, MaxMessages: 500})
			if page.HasMore {
				t.Error("full page reports more history")
			}
			if len(page.Records) != int(want.lastSeq)+1 {
				t.Fatalf("page has %d records, want %d", len(page.Records), want.lastSeq+1)
			}

			// BeforeSeq is an exclusive upper bound, which is how the client
			// asks for older pages.
			before := want.lastSeq
			older := PageEvents(events, PageRequest{ThroughSeq: want.lastSeq, BeforeSeq: &before})
			if older.HasMore {
				t.Error("oldest page reports more history")
			}
			for _, record := range older.Records {
				if record.Seq >= before {
					t.Fatalf("BeforeSeq page includes seq %d", record.Seq)
				}
			}
			sawUser, sawAssistant := false, false
			for _, record := range page.Records {
				switch record.Type {
				case EventUserMessage:
					sawUser = record.SurfaceOp != ""
				case EventAssistantMessage:
					sawAssistant = record.SurfaceOp != ""
				}
			}
			if !sawUser {
				t.Error("page has no surface user message")
			}
			if sawAssistant != want.assistant {
				t.Errorf("assistant message present = %v, want %v", sawAssistant, want.assistant)
			}
		})
	}
}
