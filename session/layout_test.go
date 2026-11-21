package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectKeyMatchesHarness pins the project-directory rule against the
// DeepSeek Harness implementation: separators and drive colons collapse to one
// dash, unsafe code units escape as ~XXXX, leading dashes are stripped, and the
// slug is bounded.
func TestProjectKeyMatchesHarness(t *testing.T) {
	tests := map[string]string{
		"":                  NoCwdDir,
		"/Users/me/project": "--Users-me-project--",
		// DSH strips leading dashes only, so a trailing separator keeps one.
		"/Users/me/project/": "--Users-me-project---",
		"a//b":               "--a-b--",
		"C:\\Users\\me":      "--C-Users-me--",
		"/tmp/with space":    "--tmp-with~0020space--",
		"/tmp/tilde~here":    "--tmp-tilde~007Ehere--",
		"/tmp/中文":            "--tmp-~4E2D~6587--",
		"/---leading":        "--leading--",
		"/":                  "--root--",
		"//":                 "--root--",
		"/tmp/.hidden":       "--tmp-.hidden--",
		"/tmp/emoji-😀":       "--tmp-emoji-~D83D~DE00--",
	}
	for cwd, want := range tests {
		if got := ProjectKey(cwd); got != want {
			t.Errorf("ProjectKey(%q) = %q, want %q", cwd, got, want)
		}
	}
	long := "/" + strings.Repeat("a", 400)
	got := ProjectKey(long)
	if len(got) != MaxProjectSlug+4 {
		t.Errorf("long slug length = %d, want %d", len(got), MaxProjectSlug+4)
	}
}

// TestEncodeSegmentIsInjective proves a hostile id cannot escape its directory.
func TestEncodeSegmentIsInjective(t *testing.T) {
	tests := map[string]string{
		"session-abc": "session-abc",
		"..":          "~002E~002E",
		".":           "~002E",
		"a/b":         "a~002Fb",
		"a\\b":        "a~005Cb",
		"nul\x00":     "nul~0000",
		"tilde~":      "tilde~007E",
		"中文":          "~4E2D~6587",
	}
	for raw, want := range tests {
		got, err := encodeSegment(raw)
		if err != nil {
			t.Fatalf("encodeSegment(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("encodeSegment(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := encodeSegment(""); err == nil {
		t.Error("empty segment encoded without an error")
	}
}

func TestNormalizeSessionID(t *testing.T) {
	if got := NormalizeSessionID("abc"); got != "session-abc" {
		t.Errorf("bare id = %q", got)
	}
	if got := NormalizeSessionID("session-abc"); got != "session-abc" {
		t.Errorf("prefixed id = %q", got)
	}
	if got := NormalizeSessionID("  "); got != "" {
		t.Errorf("blank id = %q", got)
	}
}

func TestParseGenerationName(t *testing.T) {
	cases := []struct {
		name        string
		version     int
		compression Compression
		ok          bool
	}{
		{"session.v4.jsonl", 4, CompressionNone, true},
		{"session.v4.jsonl.zstd", 4, CompressionZstd, true},
		{"session.v3.jsonl.zstd", 3, CompressionZstd, true},
		{"session.jsonl", 0, CompressionNone, true},
		{"session.v04.jsonl", 0, "", false},
		{"session.v0.jsonl", 0, "", false},
		{"session.v4.jsonl.tmp", 0, "", false},
		{"session.v4.JSONL", 0, "", false},
		{"other.v4.jsonl", 0, "", false},
	}
	for _, test := range cases {
		version, compression, ok := ParseGenerationName(test.name)
		if ok != test.ok || version != test.version || compression != test.compression {
			t.Errorf("ParseGenerationName(%q) = %d, %q, %v; want %d, %q, %v",
				test.name, version, compression, ok, test.version, test.compression, test.ok)
		}
	}
}

func TestSessionDirUsesEncodedID(t *testing.T) {
	dir, err := SessionDir("/root", "/Users/me/project", "session-abc")
	if err != nil {
		t.Fatalf("SessionDir: %v", err)
	}
	want := filepath.Join("/root", "--Users-me-project--", "session-abc")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	// An id that tries to leave its own project directory stays inside it.
	escaped, err := SessionDir("/root", "/w", "../other")
	if err != nil {
		t.Fatalf("SessionDir: %v", err)
	}
	if rel, err := filepath.Rel(filepath.Join("/root", "--w--"), escaped); err != nil ||
		rel == ".." || strings.HasPrefix(rel, "../") {
		t.Errorf("escaped dir = %q", escaped)
	}
}

// TestCreateWritesHarnessLayout proves a new session lands on the exact path the
// DeepSeek Harness reader expects: project key, encoded id, generation file,
// lease file, and owner-only permissions.
func TestCreateWritesHarnessLayout(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	opened, err := store.Create("/Users/me/project", "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(opened.ID(), SessionPrefix) {
		t.Fatalf("id = %q, want a session- prefix", opened.ID())
	}
	wantDir := filepath.Join(root, "--Users-me-project--", opened.ID())
	if opened.Dir() != wantDir {
		t.Fatalf("dir = %q, want %q", opened.Dir(), wantDir)
	}
	if opened.Path() != filepath.Join(wantDir, "session.v4.jsonl.zstd") {
		t.Fatalf("path = %q", opened.Path())
	}
	if _, err := os.Stat(filepath.Join(wantDir, LeaseFileName)); err != nil {
		t.Fatalf("lease file: %v", err)
	}
	info, err := os.Stat(opened.Path())
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("artifact permissions = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(opened.Dir())
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory permissions = %o, want 700", perm)
	}
	if _, err := opened.AppendSurface(EventUserMessage, map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": "hi"}},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(opened.Path())
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	frames, torn, err := zstdFrames(raw)
	if err != nil {
		t.Fatalf("scan frames: %v", err)
	}
	if torn >= 0 {
		t.Fatalf("torn tail at %d", torn)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want one header frame and one append frame", len(frames))
	}
	decoder, err := newFrameDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer decoder.release()
	headerFrame, err := decoder.decompressFrame(raw[frames[0].start:frames[0].end])
	if err != nil {
		t.Fatalf("header frame: %v", err)
	}
	if strings.Count(string(headerFrame), "\n") != 1 || !strings.HasSuffix(string(headerFrame), "\n") {
		t.Fatalf("first frame is not exactly one header line: %q", headerFrame)
	}
	if !strings.Contains(string(headerFrame), `"id":"`+opened.ID()+`"`) {
		t.Fatalf("header id missing: %q", headerFrame)
	}
	if !strings.Contains(string(headerFrame), `"delegationDepth":0`) {
		t.Fatalf("header delegationDepth missing: %q", headerFrame)
	}
}

// TestLegacyBareIDIsReadableAndRepairedOnWrite proves a log written before ids
// carried the canonical prefix stays readable, and that opening it for writing
// repairs the header without touching a single event row.
func TestLegacyBareIDIsReadableAndRepairedOnWrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--tmp-legacy--", "session-legacy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "session.v4.jsonl")
	body := `{"type":"session","version":4,"id":"legacy","createdAt":1,"cwd":"/tmp/legacy","isSeeded":false,"delegationDepth":0}` + "\n" +
		`{"type":"turn/start","seq":0,"time":2,"data":{"turn":1}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	store, err := NewStoreWithConfig(Config{Root: root, Compression: CompressionNone})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	read, err := store.OpenRead("legacy")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	if read.ID() != "session-legacy" {
		t.Fatalf("id = %q", read.ID())
	}
	if len(read.Events()) != 1 {
		t.Fatalf("read %d events", len(read.Events()))
	}
	if err := read.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	store.Forget("legacy")

	write, err := store.Open("legacy")
	if err != nil {
		t.Fatalf("Open for write: %v", err)
	}
	if !write.Writable() {
		t.Fatal("write handle is not writable")
	}
	repaired, err := os.ReadFile(write.Path())
	if err != nil {
		t.Fatalf("read repaired log: %v", err)
	}
	header, events, _, err := decodeArtifact(repaired, CompressionNone)
	if err != nil {
		t.Fatalf("decode repaired log: %v", err)
	}
	if header.ID != "session-legacy" {
		t.Fatalf("repaired header id = %q", header.ID)
	}
	if len(events) != 1 || events[0].Type != EventTurnStart {
		t.Fatalf("repaired events = %+v", events)
	}
	// The repair is idempotent: the next open leaves the file as it is.
	if err := write.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	store.Forget("legacy")
	again, err := store.Open("legacy")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again.Header().ID != "session-legacy" {
		t.Fatalf("second repair changed the id to %q", again.Header().ID)
	}
}
