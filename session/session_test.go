package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCreateWritesHeaderAndAppendAssignsSeqs(t *testing.T) {
	store := newTestStore(t)
	opened, err := store.Create("/tmp/work", "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if opened.ID() == "" || len(opened.ID()) != 36 {
		t.Fatalf("unexpected id %q", opened.ID())
	}
	if opened.LastSeq() != -1 {
		t.Fatalf("empty log last seq = %d, want -1", opened.LastSeq())
	}
	if _, err := opened.AppendSurface(EventUserMessage, map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": "hi"}},
	}); err != nil {
		t.Fatalf("AppendSurface: %v", err)
	}
	second, err := opened.Append(EventTurnEnd, map[string]any{"turn": 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if second.Seq != 1 {
		t.Fatalf("second seq = %d, want 1", second.Seq)
	}
	events := opened.Events()
	if len(events) != 2 || events[0].SurfaceOp != "append" {
		t.Fatalf("events = %+v", events)
	}

	// Reopen from a second store to prove the log round-trips through disk.
	reopened, err := NewStore(store.Root())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer reopened.Close()
	again, err := reopened.Open(opened.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if again.Header().Cwd != "/tmp/work" || again.Header().AgentPreset != "std-no-subagent" {
		t.Fatalf("header = %+v", again.Header())
	}
	if got := len(again.Events()); got != 2 {
		t.Fatalf("reopened %d events, want 2", got)
	}
}

func TestOpenUnknownIDReportsErrNotFound(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Open("missing"); err != ErrNotFound {
		t.Fatalf("Open missing = %v, want ErrNotFound", err)
	}
	if _, err := store.Open("../escape"); err != ErrNotFound {
		t.Fatalf("Open escaping id = %v, want ErrNotFound", err)
	}
}

func TestListSummarizesTitleAndBlank(t *testing.T) {
	store := newTestStore(t)
	blank, err := store.Create("/tmp/a", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	used, err := store.Create("/tmp/b", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := used.Append(EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := used.AppendSurface(EventUserMessage, map[string]any{"role": "user"}); err != nil {
		t.Fatalf("AppendSurface: %v", err)
	}
	if _, err := used.Append(EventSessionTitle, map[string]any{
		"title": "Planning the fork", "source": map[string]any{"kind": "user"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(entries))
	}
	byID := map[string]ListEntry{}
	for _, entry := range entries {
		byID[entry.Header.ID] = entry
	}
	if !byID[blank.ID()].Blank {
		t.Fatalf("blank session reported as used")
	}
	if byID[used.ID()].Blank {
		t.Fatalf("used session reported as blank")
	}
	if got := byID[used.ID()].DisplayTitle(); got != "Planning the fork" {
		t.Fatalf("title = %q", got)
	}
	// Newest activity first.
	if entries[0].Header.ID != used.ID() {
		t.Fatalf("list order = %v", []string{entries[0].Header.ID, entries[1].Header.ID})
	}
}

func TestBaselineEmptySessionUsesMinusOne(t *testing.T) {
	store := newTestStore(t)
	opened, err := store.Create("/tmp/a", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	baseline := Baseline(opened.Header(), opened.Events())
	if baseline.AsOfSeq != -1 {
		t.Fatalf("empty baseline asOfSeq = %d, want -1", baseline.AsOfSeq)
	}
	metadata, ok := baseline.Values["sessionListMetadata"].(map[string]any)
	if !ok {
		t.Fatalf("sessionListMetadata = %#v", baseline.Values["sessionListMetadata"])
	}
	if metadata["blank"] != true || metadata["lastPromptAt"] != nil {
		t.Fatalf("sessionListMetadata = %#v", metadata)
	}
	blankSelection, ok := baseline.Values["modelSelection"].(map[string]any)
	if !ok || blankSelection["lastUsed"] != nil || blankSelection["next"] != nil {
		t.Fatalf("initial modelSelection = %#v", baseline.Values["modelSelection"])
	}

	if _, err := opened.Append(EventModelSelection, ModelSelection{
		Provider: "deepseek", Model: "deepseek-chat",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	baseline = Baseline(opened.Header(), opened.Events())
	if baseline.AsOfSeq != 0 {
		t.Fatalf("asOfSeq = %d, want 0", baseline.AsOfSeq)
	}
	selection, ok := baseline.Values["modelSelection"].(map[string]any)
	if !ok {
		t.Fatalf("modelSelection = %#v", baseline.Values["modelSelection"])
	}
	if selection["lastUsed"] != nil {
		t.Fatalf("modelSelection.lastUsed = %#v, want nil before a request", selection["lastUsed"])
	}
	next, ok := selection["next"].(*ModelSelection)
	if !ok || next.Model != "deepseek-chat" {
		t.Fatalf("modelSelection.next = %#v", selection["next"])
	}
}

func TestSlugIsStableAndFilesystemSafe(t *testing.T) {
	cases := map[string]string{
		"/Users/xhd2015/tmp/tzi": "--Users-xhd2015-tmp-tzi--",
		"/":                      "--root--",
	}
	for input, want := range cases {
		got := Slug(input)
		if got != want {
			t.Fatalf("Slug(%q) = %q, want %q", input, got, want)
		}
		if again := Slug(input); again != got {
			t.Fatalf("Slug(%q) is not stable: %q then %q", input, got, again)
		}
	}
	if strings.ContainsAny(Slug("/tmp/a b/c:d"), "/:\\") {
		t.Fatalf("slug keeps unsafe characters: %q", Slug("/tmp/a b/c:d"))
	}
}

func TestPageAlignsToTurnStart(t *testing.T) {
	store := newTestStore(t)
	opened, err := store.Create("/tmp/a", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for turn := int64(1); turn <= 3; turn++ {
		if _, err := opened.Append(EventTurnStart, map[string]any{"turn": turn}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if _, err := opened.AppendSurface(EventUserMessage, map[string]any{"role": "user", "turn": turn}); err != nil {
			t.Fatalf("AppendSurface: %v", err)
		}
		if _, err := opened.AppendSurface(EventAssistantMessage, map[string]any{"turn": turn, "stream": []any{}}); err != nil {
			t.Fatalf("AppendSurface: %v", err)
		}
		if _, err := opened.Append(EventTurnEnd, map[string]any{"turn": turn}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	events := opened.Events()
	if len(events) != 12 {
		t.Fatalf("seeded %d events", len(events))
	}
	page := PageEvents(events, PageRequest{ThroughSeq: 11, MaxMessages: 3})
	if len(page.Records) == 0 {
		t.Fatal("empty page")
	}
	first := page.Records[0]
	if first.Type != EventTurnStart || first.Seq != 4 {
		t.Fatalf("page starts at %s seq %d, want turn/start seq 4", first.Type, first.Seq)
	}
	if !page.HasMore {
		t.Fatal("expected an earlier page")
	}
}

func TestSummaryProjectionsCarryListRow(t *testing.T) {
	store := newTestStore(t)
	opened, err := store.Create("/tmp/a", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	entries, err := store.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("List: %v %d", err, len(entries))
	}
	projections := SummaryProjections(entries[0])
	if projections["kind"] != "cached" {
		t.Fatalf("kind = %#v", projections["kind"])
	}
	if projections["asOfSeq"] != int64(-1) {
		t.Fatalf("asOfSeq = %#v", projections["asOfSeq"])
	}
	t.Logf("session %s projections %s", opened.ID(), mustJSON(projections))
}

func TestLogFileUsesV4Layout(t *testing.T) {
	store := newTestStore(t)
	opened, err := store.Create("/tmp/layout", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := filepath.Join(store.Root(), Slug("/tmp/layout"), DirPrefix+opened.ID(), FileName)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected log at %s: %v", want, err)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var header Header
	if err := json.Unmarshal(raw[:firstLine(t, raw)], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Version != 4 || header.Type != "session" || header.ID != opened.ID() {
		t.Fatalf("header = %+v", header)
	}
}

func firstLine(t *testing.T, raw []byte) int {
	t.Helper()
	for i, b := range raw {
		if b == '\n' {
			return i
		}
	}
	t.Fatal("log has no newline")
	return 0
}

func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}

func TestCreateWithIDReusesBlankSessionOnRetry(t *testing.T) {
	store := newTestStore(t)
	first, err := store.CreateWithID("/tmp/retry", "std-no-subagent", "retry-id")
	if err != nil {
		t.Fatalf("first CreateWithID: %v", err)
	}
	second, err := store.CreateWithID("/tmp/retry", "std-no-subagent", "retry-id")
	if err != nil {
		t.Fatalf("retry CreateWithID: %v", err)
	}
	if second.ID() != first.ID() || second.Header().AgentPreset != "std-no-subagent" {
		t.Fatalf("retry opened %+v, want existing %q", second.Header(), first.ID())
	}
	header, events, err := readLog(logPath(first.Dir()))
	if err != nil {
		t.Fatalf("readLog: %v", err)
	}
	if header.ID != "retry-id" || len(events) != 0 {
		t.Fatalf("header=%+v events=%d, want one blank header", header, len(events))
	}
}

func TestCreateWithIDRejectsUsedSessionRetry(t *testing.T) {
	store := newTestStore(t)
	first, err := store.CreateWithID("/tmp/used", "", "used-id")
	if err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if _, err := first.Append(EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.CreateWithID("/tmp/used", "", "used-id"); err == nil {
		t.Fatal("duplicate create of a used session succeeded, want error")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want already exists", err)
	}
}
