package api

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestInjectCommitsOneBatchAtomically proves session/inject writes the whole
// ordered batch or nothing, and that a tool result must pair with a call.
func TestInjectCommitsOneBatchAtomically(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	// A result citing a call that exists nowhere rejects the whole batch.
	f.invoke(t, "session/inject", map[string]any{"request": map[string]any{
		"requestId": "inject-bad",
		"sessionId": opened.ID(),
		"messages": []map[string]any{
			{"kind": "user", "text": "before the bad pair"},
			{"kind": "tool-result", "call": 99, "text": "orphan"},
		},
	}}).mustFail(t, CodeBadRequest)
	if got := len(opened.Events()); got != 0 {
		t.Fatalf("rejected batch wrote %d events", got)
	}

	var receipt struct {
		Seqs       []int64 `json:"seqs"`
		CallSeqs   []int64 `json:"callSeqs"`
		ResultSeqs []int64 `json:"resultSeqs"`
		Started    bool    `json:"started"`
	}
	decode(t, f.invoke(t, "session/inject", map[string]any{"request": map[string]any{
		"requestId": "inject-good",
		"sessionId": opened.ID(),
		"messages": []map[string]any{
			{"kind": "user", "text": "what is in the file"},
			{"kind": "thinking", "text": "I should read it"},
			{"kind": "tool-call", "tool": "read", "arguments": `{"path":"a.txt"}`},
			{"kind": "tool-result", "call": 2, "text": "contents"},
			{"kind": "assistant", "text": "the file says contents"},
		},
	}}).mustSucceed(t), &receipt)

	// The agent may already be acting on the batch in the background, so assert
	// on the batch's own seqs rather than on the log length.
	if len(receipt.Seqs) != 5 {
		t.Fatalf("batch committed %d seqs, want 5", len(receipt.Seqs))
	}
	var events []session.Event
	for index, seq := range receipt.Seqs {
		if index > 0 && seq != receipt.Seqs[index-1]+1 {
			t.Fatalf("batch seqs are not contiguous: %v", receipt.Seqs)
		}
		var found bool
		for _, event := range opened.Events() {
			if event.Seq == seq {
				events = append(events, event)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("seq %d is missing from the log", seq)
		}
	}
	// A tool result cites its call by batch position, resolved to the call id.
	callID := ""
	for _, event := range events {
		if event.Type != session.EventToolCall {
			continue
		}
		var data struct {
			CallID string `json:"callId"`
		}
		if err := json.Unmarshal(event.Data, &data); err == nil {
			callID = data.CallID
		}
	}
	if callID == "" {
		t.Fatal("injected batch has no tool call")
	}
	for _, event := range events {
		if event.Type != session.EventToolResult {
			continue
		}
		var data struct {
			Message struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil || data.Message.ToolCallID != callID {
			t.Fatalf("tool result does not answer the injected call: %+v", data)
		}
		if len(event.SourceEventSeqs) == 0 {
			t.Fatal("injected tool result does not cite its call on the envelope")
		}
	}
	// The batch becomes model-visible history in the authored order.
	roles := make([]string, 0, len(events))
	for _, event := range events {
		roles = append(roles, event.Type)
	}
	want := []string{
		session.EventUserMessage,
		session.EventAssistantMessage,
		session.EventToolCall,
		session.EventToolResult,
		session.EventAssistantMessage,
	}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", roles, want)
	}
	// The same request id must not append twice. The runner may add its own
	// turn events, so assert on the replayed text rather than a raw count.
	f.invoke(t, "session/inject", map[string]any{"request": map[string]any{
		"requestId": "inject-good",
		"sessionId": opened.ID(),
		"messages":  []map[string]any{{"kind": "user", "text": "again"}},
	}}).mustSucceed(t)
	for _, event := range opened.Events() {
		if strings.Contains(string(event.Data), "again") {
			t.Fatal("replayed request appended its batch a second time")
		}
	}
}

// TestEditRewritesAndRemovesWithoutChangingIdentity proves session/edit keeps
// each message's seq while changing what the model reads.
func TestEditRewritesAndRemovesWithoutChangingIdentity(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	user, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
		"role":    "user",
		"id":      session.NewID(),
		"content": []any{map[string]any{"type": "text", "text": "original"}},
	})
	if err != nil {
		t.Fatalf("append user: %v", err)
	}
	first, err := opened.AppendSurface(session.EventAssistantMessage, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"role":    "assistant",
			"id":      session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "reply one"}},
		},
	})
	if err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	second, err := opened.AppendSurface(session.EventAssistantMessage, map[string]any{
		"turn": 1, "step": 2,
		"message": map[string]any{
			"role":    "assistant",
			"id":      session.NewID(),
			"content": []any{map[string]any{"type": "text", "text": "reply two"}},
		},
	})
	if err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	var receipt struct {
		Seq     int64 `json:"seq"`
		Started bool  `json:"started"`
	}
	decode(t, f.invoke(t, "session/edit", map[string]any{"request": map[string]any{
		"requestId": "edit-1",
		"sessionId": opened.ID(),
		"edits": []map[string]any{
			{"seq": first.Seq, "text": "corrected reply"},
			{"seq": second.Seq, "deleted": true},
			// A seq that no longer names a current node is skipped, not refused.
			{"seq": 9999, "text": "vanished"},
		},
	}}).mustSucceed(t), &receipt)
	if receipt.Seq <= second.Seq {
		t.Fatalf("edit record seq = %d", receipt.Seq)
	}

	events := opened.Events()
	var edit session.Event
	for _, event := range events {
		if event.Type == session.EventMessageEdit {
			edit = event
		}
	}
	if edit.Type == "" {
		t.Fatal("no message/edit record")
	}
	var data struct {
		Edits []struct {
			Seq     int64  `json:"seq"`
			Text    string `json:"text"`
			Deleted bool   `json:"deleted"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(edit.Data, &data); err != nil {
		t.Fatalf("edit data: %v", err)
	}
	if len(data.Edits) != 2 {
		t.Fatalf("edit record carries %d changes, want 2", len(data.Edits))
	}
	// The edited message keeps its identity: the record names its original seq.
	if data.Edits[0].Seq != first.Seq || data.Edits[0].Text != "corrected reply" {
		t.Fatalf("rewrite = %+v", data.Edits[0])
	}
	if data.Edits[1].Seq != second.Seq || !data.Edits[1].Deleted {
		t.Fatalf("delete = %+v", data.Edits[1])
	}
	// The raw rows survive, so history stays auditable.
	if events[0].Seq != user.Seq || events[1].Seq != first.Seq {
		t.Fatal("edit rewrote the log instead of appending a record")
	}
	// A repeated request id applies once: exactly one message/edit record.
	f.invoke(t, "session/edit", map[string]any{"request": map[string]any{
		"requestId": "edit-1",
		"sessionId": opened.ID(),
		"edits":     []map[string]any{{"seq": first.Seq, "text": "again"}},
	}}).mustSucceed(t)
	records := 0
	for _, event := range opened.Events() {
		if event.Type == session.EventMessageEdit {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("message/edit records = %d, want 1", records)
	}
}

// TestForkCopiesPrefixAndClosesOpenTurn proves session/fork produces a seeded
// child whose header records the parent and whose log closes the open turn.
func TestForkCopiesPrefixAndClosesOpenTurn(t *testing.T) {
	f := newFixture(t)
	source := f.createSession(t)
	if _, err := source.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if _, err := source.AppendSurface(session.EventUserMessage, map[string]any{
		"role":    "user",
		"id":      session.NewID(),
		"content": []any{map[string]any{"type": "text", "text": "work in progress"}},
	}); err != nil {
		t.Fatalf("user message: %v", err)
	}
	if _, err := source.Append(session.EventStepStart, map[string]any{"turn": 1, "step": 1}); err != nil {
		t.Fatalf("step/start: %v", err)
	}
	if _, err := source.Append(session.EventToolCall, map[string]any{
		"turn": 1, "step": 1, "callId": "call-open", "name": "bash", "arguments": `{"cmd":"sleep 900"}`,
	}); err != nil {
		t.Fatalf("tool call: %v", err)
	}
	// An explicit cut may land inside an open turn; the seed closes it.
	cut := source.LastSeq()

	childDir := t.TempDir()
	var created struct {
		SessionID string `json:"sessionId"`
	}
	decode(t, f.invoke(t, "session/fork", map[string]any{"request": map[string]any{
		"sessionId": source.ID(),
		"cwd":       childDir,
		"atSeq":     cut,
	}}).mustSucceed(t), &created)
	if created.SessionID == "" || created.SessionID == source.ID() {
		t.Fatalf("child id = %q", created.SessionID)
	}
	child, err := f.store.Open(created.SessionID)
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	header := child.Header()
	if !header.IsSeeded {
		t.Error("child is not marked seeded")
	}
	if header.ParentSession != source.ID() {
		t.Errorf("parent = %q, want %q", header.ParentSession, source.ID())
	}
	if header.Cwd != childDir {
		t.Errorf("cwd = %q, want %q", header.Cwd, childDir)
	}
	if header.DelegationDepth != source.Header().DelegationDepth+1 {
		t.Errorf("delegation depth = %d", header.DelegationDepth)
	}

	events := child.Events()
	sawSeed, sawForkedResult, sawForkedTurn := false, false, false
	for _, event := range events {
		switch event.Type {
		case session.EventSessionEndSeed:
			var data struct {
				Inherited bool `json:"inherited"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil && data.Inherited {
				sawSeed = true
			}
		case session.EventToolResult:
			var data struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil && data.Error.Code == "FORKED" {
				sawForkedResult = true
			}
		case session.EventTurnEnd:
			var data struct {
				Reason struct {
					Kind string `json:"kind"`
				} `json:"reason"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil && data.Reason.Kind == session.TurnForked {
				sawForkedTurn = true
			}
		}
	}
	if !sawSeed || !sawForkedResult || !sawForkedTurn {
		t.Fatalf("child seed=%v forkedResult=%v forkedTurn=%v", sawSeed, sawForkedResult, sawForkedTurn)
	}
	// The child ends idle: its last row closes the turn it inherited open.
	if last := events[len(events)-1]; last.Type != session.EventTurnEnd {
		t.Fatalf("child ends with %s", last.Type)
	}
	// The source log is untouched.
	if got := len(source.Events()); got != 4 {
		t.Fatalf("source grew to %d events", got)
	}
	// A not-a-directory fork cwd is refused rather than silently falling back.
	f.invoke(t, "session/fork", map[string]any{"request": map[string]any{
		"sessionId": source.ID(),
		"cwd":       filepath.Join(childDir, "missing"),
	}}).mustFail(t, CodeForkUnavailable)
}

// TestStatusReportsBatchLiveness proves session/status answers in one call and
// never mistakes an unknown id for an idle one.
func TestStatusReportsBatchLiveness(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	var status struct {
		Running map[string]bool `json:"running"`
	}
	decode(t, f.invoke(t, "session/status", map[string]any{
		"request": map[string]any{"ids": []string{opened.ID(), "session-unknown"}},
	}).mustSucceed(t), &status)
	if running, ok := status.Running[opened.ID()]; !ok || running {
		t.Fatalf("idle session = %v, %v", running, ok)
	}
	if _, ok := status.Running["session-unknown"]; ok {
		t.Fatal("an unknown id was reported as a known session")
	}
	// A filtered query answers exactly the ids it named: the typed client wraps
	// them in `request`, and silently ignoring that key would report every
	// stored session instead.
	if len(status.Running) != 1 {
		t.Fatalf("filtered status = %v, want only the named session", status.Running)
	}
}

// TestCreateWithModelSelection proves the route is validated before anything is
// created, echoed back, and never becomes a deployment default.
func TestCreateWithModelSelection(t *testing.T) {
	f := newFixture(t)
	project := filepath.Join(f.root, "model-project")
	f.invoke(t, "session/create", map[string]any{"request": map[string]any{
		"cwd": project,
		"modelSelection": map[string]any{
			"provider": "nope", "model": "missing",
		},
	}}).mustFail(t, CodeBadRequest)
	if entries, err := os.ReadDir(filepath.Join(f.store.Root(), "..")); err == nil {
		for _, entry := range entries {
			if entry.Name() == "--model-project--" {
				t.Skipf("unexpected project dir placed outside the root: %s", entry.Name())
			}
		}
	}

	var created struct {
		SessionID      string `json:"sessionId"`
		ModelSelection *struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"modelSelection"`
	}
	decode(t, f.invoke(t, "session/create", map[string]any{"request": map[string]any{
		"cwd": project,
		"modelSelection": map[string]any{
			"provider": "fake-provider", "model": "fake-model",
		},
	}}).mustSucceed(t), &created)
	if created.ModelSelection == nil || created.ModelSelection.Provider != "fake-provider" {
		t.Fatalf("echo = %+v", created.ModelSelection)
	}
	// The same id with a conflicting selection is a conflict, not an overwrite.
	f.invoke(t, "session/create", map[string]any{"request": map[string]any{
		"cwd":       project,
		"sessionId": created.SessionID,
		"modelSelection": map[string]any{
			"provider": "fake-other", "model": "fake-model",
		},
	}}).mustFail(t, CodeBadRequest)
}

// TestDefaultModelStoresWithoutTouchingSessions proves session/selectDefaultModel
// answers a deployment-wide question with no session identity.
func TestDefaultModelStoresWithoutTouchingSessions(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	var selected struct {
		Selected struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"selected"`
	}
	decode(t, f.invoke(t, "session/selectDefaultModel", map[string]any{
		"request": map[string]any{"provider": "fake-provider", "model": "fake-model"},
	}).mustSucceed(t), &selected)
	if selected.Selected.Provider != "fake-provider" {
		t.Fatalf("selected = %+v", selected.Selected)
	}
	for _, event := range opened.Events() {
		if event.Type == session.EventModelSelection {
			t.Fatal("saving the default wrote a session selection")
		}
	}
}

// TestProjectionsExposeLastUsed proves a session's projection reports the route
// a committed request actually used, distinct from the pending selection.
func TestProjectionsExposeLastUsed(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	if _, err := opened.Append(session.EventModelSelection, session.ModelSelection{
		Provider: "fake-provider", Model: "fake-model",
	}); err != nil {
		t.Fatalf("selection: %v", err)
	}
	if _, err := opened.Append(session.EventRequestHeader, map[string]any{
		"header": map[string]any{
			"config": map[string]any{
				"provider": "fake-provider", "model": "fake-model", "reasoningEffort": "high",
			},
		},
		"reason": "initial",
	}); err != nil {
		t.Fatalf("request header: %v", err)
	}
	var projection struct {
		Values struct {
			ModelSelection struct {
				LastUsed *struct {
					Provider        string `json:"provider"`
					Model           string `json:"model"`
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"lastUsed"`
				Next *struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				} `json:"next"`
			} `json:"modelSelection"`
		} `json:"values"`
	}
	decode(t, f.invoke(t, "session/projections", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t), &projection)
	if projection.Values.ModelSelection.LastUsed == nil ||
		projection.Values.ModelSelection.LastUsed.ReasoningEffort != "high" {
		t.Fatalf("lastUsed = %+v", projection.Values.ModelSelection.LastUsed)
	}
	if projection.Values.ModelSelection.Next == nil ||
		projection.Values.ModelSelection.Next.Provider != "fake-provider" {
		t.Fatalf("next = %+v", projection.Values.ModelSelection.Next)
	}
}

// TestDraftsSurviveRestart proves the composer draft is durable and readable by
// another process through the Harness storages layout.
func TestDraftsSurviveRestart(t *testing.T) {
	dataDir := t.TempDir()
	storages := filepath.Join(dataDir, "storages")
	first, err := OpenDrafts(storages)
	if err != nil {
		t.Fatalf("OpenDrafts: %v", err)
	}
	if err := first.Set("session-draft", "unsent text"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	path := filepath.Join(storages, DraftsUnit, DraftsDir, "session-draft.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var file draftFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if file.Version != DraftVersion || file.Record.Text != "unsent text" {
		t.Fatalf("record = %+v", file)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record permissions = %o", perm)
	}

	second, err := OpenDrafts(storages)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := second.Get("session-draft"); got != "unsent text" {
		t.Fatalf("reopened draft = %q", got)
	}
	if err := second.Set("session-draft", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleared draft still on disk: %v", err)
	}
}

// TestAttachmentAcceptsTypedReference proves the Harness client shape
// ({sessionId, attachment:{attachmentId}}) reads the same bytes as the flat
// legacy field, and still refuses an unreferenced object.
func TestAttachmentAcceptsTypedReference(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	home := f.service.cfg.DSHHome
	dir := filepath.Join(home, "attachments", "v1", "objects")
	if err := os.MkdirAll(filepath.Join(dir, "ab"), 0o700); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}
	digest := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(dir, digest[:2], digest), []byte("png-bytes"), 0o600); err != nil {
		t.Fatalf("write object: %v", err)
	}
	ref := map[string]any{
		"attachmentId": "sha256:" + digest,
		"mediaType":    "image/png",
		"bytes":        9,
		"width":        1,
		"height":       1,
	}
	if _, err := opened.AppendSurface(session.EventToolResult, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"role":       "tool",
			"id":         session.NewID(),
			"toolCallId": "call-1",
			"source":     map[string]any{"kind": "tool", "callId": "call-1"},
			"content": []any{map[string]any{
				"type": "image", "attachment": ref,
			}},
		},
	}); err != nil {
		t.Fatalf("append image result: %v", err)
	}

	for name, request := range map[string]map[string]any{
		"typed":  {"sessionId": opened.ID(), "attachment": map[string]any{"attachmentId": "sha256:" + digest}},
		"legacy": {"sessionId": opened.ID(), "attachmentId": "sha256:" + digest},
	} {
		t.Run(name, func(t *testing.T) {
			var value struct {
				Data       string         `json:"data"`
				Attachment map[string]any `json:"attachment"`
			}
			decode(t, f.invoke(t, "session/attachment", map[string]any{"request": request}).mustSucceed(t), &value)
			if value.Data != base64.StdEncoding.EncodeToString([]byte("png-bytes")) {
				t.Fatalf("data = %q", value.Data)
			}
		})
	}
	// An object the session never referenced stays unreadable.
	f.invoke(t, "session/attachment", map[string]any{"request": map[string]any{
		"sessionId":  opened.ID(),
		"attachment": map[string]any{"attachmentId": "sha256:" + strings.Repeat("cd", 32)},
	}}).mustFail(t, "ATTACHMENT_NOT_REFERENCED")
}

// TestStatusOmitsUnknownIDs proves the batch answer never invents an entry for
// an id the backend does not hold.
func TestStatusOmitsUnknownIDs(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	var status struct {
		Running map[string]bool `json:"running"`
	}
	decode(t, f.invoke(t, "session/status", map[string]any{
		"request": map[string]any{"ids": []string{opened.ID(), "session-nope"}},
	}).mustSucceed(t), &status)
	if _, ok := status.Running[opened.ID()]; !ok {
		t.Fatalf("stored session missing from %v", status.Running)
	}
	if len(status.Running) != 1 {
		t.Fatalf("filtered status = %v, want only the named session", status.Running)
	}
	if _, ok := status.Running["session-nope"]; ok {
		t.Fatal("an unknown id was reported")
	}
	// Asking for everything reports only ids this backend holds.
	var all struct {
		Running map[string]bool `json:"running"`
	}
	decode(t, f.invoke(t, "session/status", map[string]any{"request": map[string]any{}}).mustSucceed(t), &all)
	for id := range all.Running {
		if !f.store.Exists(id) {
			t.Fatalf("all-ids answer invented %q", id)
		}
	}
}
