package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestCompactedResetIsSmallerThanTheSource is the point of the operation: the child
// starts at the compacted context, not at a copy of the transcript.
func TestCompactedResetIsSmallerThanTheSource(t *testing.T) {
	f := newFixture(t)
	source := f.createSession(t)
	// Enough turns that the retention window drops the oldest ones.
	for i := 0; i < 14; i++ {
		if _, err := source.AppendSurface(session.EventUserMessage, map[string]any{
			"role": "user", "id": session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "question"}},
			"source":  map[string]any{"kind": "user"},
		}); err != nil {
			t.Fatalf("user: %v", err)
		}
		if _, err := source.AppendSurface(session.EventAssistantMessage, map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"id": session.NewID(), "role": "assistant",
				"content": []any{map[string]any{"type": "text", "text": "answer"}},
			},
		}); err != nil {
			t.Fatalf("assistant: %v", err)
		}
	}
	// Compact the oldest turns away: that is the state a reset is for, and the
	// state in which the child is dramatically smaller than the source.
	if err := source.Compact(session.CompactionInput{
		CompactionID: "compaction-reset",
		Summary:      "the first ten questions were about setup",
		Range:        session.SeqRange{Start: 0, End: 19},
	}); err != nil {
		t.Fatalf("compact source: %v", err)
	}
	sourceCount := len(source.Events())

	childDir := t.TempDir()
	var value struct {
		SessionID string `json:"sessionId"`
		Compacted bool   `json:"compacted"`
	}
	decode(t, f.invoke(t, "session/compactIntoNew", map[string]any{
		"request": map[string]any{"sessionId": source.ID(), "cwd": childDir},
	}).mustSucceed(t), &value)
	if value.SessionID == "" || value.SessionID == source.ID() {
		t.Fatalf("child id = %q", value.SessionID)
	}

	child, err := f.store.Open(value.SessionID)
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	// A fork would copy the source's whole prefix; a reset starts from the surface.
	if len(child.Events()) >= sourceCount {
		t.Fatalf("child has %d events, source has %d: this is a fork, not a reset", len(child.Events()), sourceCount)
	}
	// The child carries the summary but not the events the summary replaced.
	var summarySeen, oldQuestionSeen bool
	for _, event := range child.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		body := string(event.Data)
		if strings.Contains(body, "the first ten questions were about setup") {
			summarySeen = true
		}
		if strings.Contains(body, `"question"`) {
			oldQuestionSeen = true
		}
	}
	if !summarySeen {
		t.Fatal("the child dropped the summary")
	}
	if !oldQuestionSeen {
		t.Fatal("the child dropped the retained tail")
	}
	// The child is a real session: seeded, and closed at the inherited cut.
	header := child.Header()
	if !header.IsSeeded {
		t.Fatal("the child is not marked seeded")
	}
	if header.Cwd != childDir {
		t.Fatalf("cwd = %q, want %q", header.Cwd, childDir)
	}
	last := child.Events()[len(child.Events())-1]
	if last.Type != session.EventSessionEndSeed {
		t.Fatalf("the child's last event is %q", last.Type)
	}
	// It has no surface event pointing back at the source log: every seq is its own.
	for _, event := range child.Events() {
		if event.SurfaceOp != nil && event.SurfaceOp.IsReplace() {
			t.Fatal("the child inherits a surface replacement; its log should be self-contained")
		}
	}
	// The source is untouched.
	if reopened, err := f.store.Open(source.ID()); err != nil || len(reopened.Events()) != sourceCount {
		t.Fatalf("the source changed: %v", err)
	}
}

// TestCompactedResetKeepsACheckpointWhole keeps the summary that replaced the old
// history: dropping it would silently reintroduce what compaction removed.
func TestCompactedResetKeepsACheckpointWhole(t *testing.T) {
	f := newFixture(t)
	source := f.createSession(t)
	for i := 0; i < 4; i++ {
		if _, err := source.AppendSurface(session.EventUserMessage, map[string]any{
			"role": "user", "id": session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "old question"}},
			"source":  map[string]any{"kind": "user"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A compaction writes the checkpoint as the replacement.
	if err := source.Compact(session.CompactionInput{
		CompactionID: "compaction-seed",
		Summary:      "the old questions were about setup",
		Range:        session.SeqRange{Start: 0, End: 3},
	}); err != nil {
		t.Fatalf("compact: %v", err)
	}

	var value struct {
		SessionID string `json:"sessionId"`
		Compacted bool   `json:"compacted"`
	}
	decode(t, f.invoke(t, "session/compactIntoNew", map[string]any{
		"request": map[string]any{"sessionId": source.ID(), "cwd": t.TempDir()},
	}).mustSucceed(t), &value)
	if !value.Compacted {
		t.Fatal("compacted = false for a source that carries a checkpoint")
	}
	child, err := f.store.Open(value.SessionID)
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	var checkpoint string
	for _, event := range child.Events() {
		if event.Type == session.EventUserMessage && sourceKindOf(event) == "compact-checkpoint" {
			checkpoint = string(event.Data)
		}
	}
	if checkpoint == "" {
		t.Fatal("the child lost the compaction checkpoint")
	}
	if !strings.Contains(checkpoint, "the old questions were about setup") {
		t.Fatalf("the checkpoint is not the summary: %s", checkpoint)
	}
}

// TestCompactedResetChildIsPromptable is what makes it usable: a reset that cannot
// continue is a snapshot, not a session.
func TestCompactedResetChildIsPromptable(t *testing.T) {
	f := newFixture(t)
	source := f.createSession(t)
	for i := 0; i < 3; i++ {
		if _, err := source.AppendSurface(session.EventUserMessage, map[string]any{
			"role": "user", "id": session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "question"}},
			"source":  map[string]any{"kind": "user"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	var value struct {
		SessionID string `json:"sessionId"`
	}
	decode(t, f.invoke(t, "session/compactIntoNew", map[string]any{
		"request": map[string]any{"sessionId": source.ID(), "cwd": t.TempDir()},
	}).mustSucceed(t), &value)

	child, err := f.store.Open(value.SessionID)
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": child.ID(),
			"content":   []map[string]any{{"type": "text", "text": "continue here"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, child, 1, 10*1000*1000*1000)
}

// TestCompactedResetRefusesWhileRunning protects the surface it reads.
func TestCompactedResetRefusesWhileRunning(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	f := newBlockingFixture(t, block)
	source := f.createSession(t)
	if _, err := source.AppendSurface(session.EventUserMessage, map[string]any{
		"role": "user", "id": session.NewID(),
		"content": []any{map[string]any{"type": "text", "text": "go"}},
		"source":  map[string]any{"kind": "user"},
	}); err != nil {
		t.Fatal(err)
	}
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": source.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)
	deadline := time.Now().Add(5 * time.Second)
	for !f.runner.Busy(source.ID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.runner.Busy(source.ID()) {
		t.Fatal("the turn never started")
	}
	f.invoke(t, "session/compactIntoNew", map[string]any{
		"request": map[string]any{"sessionId": source.ID()},
	}).mustFail(t, CodeBusy)
}

var _ = json.Marshal
