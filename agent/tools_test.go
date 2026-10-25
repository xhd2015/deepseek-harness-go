package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newToolTestExecutor(t *testing.T) (*ToolExecutor, string) {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = resolved
	executor := NewToolExecutor(root)
	if !executor.Enabled() {
		t.Fatalf("tool executor is not enabled: %v", executor.InitError())
	}
	return executor, root
}

func callTool(t *testing.T, executor *ToolExecutor, inv ToolInvocation, name string, args map[string]any) (string, bool) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	inv.Name = name
	inv.Arguments = string(encoded)
	return executor.Execute(context.Background(), inv)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestToolDefinitions(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	definitions := executor.Definitions()
	want := []string{"bash", "read", "write", "edit", "glob", "grep"}
	if len(definitions) != len(want) {
		t.Fatalf("got %d tool definitions, want %d", len(definitions), len(want))
	}
	for index, name := range want {
		if definitions[index].Name != name {
			t.Errorf("tool %d = %q, want %q", index, definitions[index].Name, name)
		}
	}
}

func TestReadEnvelopeAndPagination(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	writeTestFile(t, filepath.Join(root, "notes.txt"), "one\ntwo\nthree\nfour\n")
	inv := ToolInvocation{SessionID: "read"}

	output, isError := callTool(t, executor, inv, "read", map[string]any{"file_path": "notes.txt", "offset": 2, "limit": 2})
	if isError {
		t.Fatalf("read failed: %s", output)
	}
	want := fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n2: two\n3: three\n\n(Showing lines 2-3 of 4. Use offset=4 to continue.)\n</content>", filepath.ToSlash(filepath.Join(root, "notes.txt")))
	if output != want {
		t.Fatalf("read output:\n%s\nwant:\n%s", output, want)
	}

	output, isError = callTool(t, executor, inv, "read", map[string]any{"file_path": "notes.txt", "offset": 5})
	wantOutOfRange := fmt.Sprintf("offset 5 is out of range for %q (4 lines)", filepath.ToSlash(filepath.Join(root, "notes.txt")))
	if !isError || output != wantOutOfRange {
		t.Fatalf("out-of-range read = %q, %v; want %q", output, isError, wantOutOfRange)
	}
}

func TestReadTruncatesLongLinesByUTF16Units(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	long := strings.Repeat("a", readMaxLineLength) + "tail"
	writeTestFile(t, filepath.Join(root, "long.txt"), long+"\n")
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "long"}, "read", map[string]any{"file_path": "long.txt"})
	if isError {
		t.Fatalf("read failed: %s", output)
	}
	if !strings.Contains(output, strings.Repeat("a", readMaxLineLength)+"... (line truncated to 2000 chars)") {
		t.Fatalf("line was not truncated as expected: %q", output)
	}
	if strings.Contains(output, "tail") {
		t.Fatal("truncated line retained its tail")
	}
}

func TestReadByteCapAndInvalidUTF8(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	var lines strings.Builder
	for index := 0; index < 80; index++ {
		lines.WriteString(strings.Repeat("x", 1000))
		lines.WriteByte('\n')
	}
	writeTestFile(t, filepath.Join(root, "wide.txt"), lines.String())
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "wide"}, "read", map[string]any{"file_path": "wide.txt"})
	if isError {
		t.Fatalf("read failed: %s", output)
	}
	if !strings.Contains(output, "(Output capped. Showing lines 1-51. Use offset=52 to continue.)") {
		t.Fatalf("missing byte-cap footer: %s", output)
	}

	writeTestFile(t, filepath.Join(root, "bad.txt"), string([]byte{'a', 0xff, 'b'}))
	output, isError = callTool(t, executor, ToolInvocation{SessionID: "bad"}, "read", map[string]any{"file_path": "bad.txt"})
	wantInvalid := fmt.Sprintf("cannot read %q: invalid UTF-8 text", filepath.ToSlash(filepath.Join(root, "bad.txt")))
	if !isError || output != wantInvalid {
		t.Fatalf("invalid UTF-8 read = %q, %v; want %q", output, isError, wantInvalid)
	}
}

func TestWriteRequiresObservationAndReportsOperation(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	inv := ToolInvocation{SessionID: "write"}
	target := filepath.Join(root, "dir", "file.txt")

	output, isError := callTool(t, executor, inv, "write", map[string]any{"file_path": "dir/file.txt", "content": "first"})
	if isError {
		t.Fatalf("create failed: %s", output)
	}
	if !strings.Contains(output, "Created file") {
		t.Fatalf("create output = %q", output)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "first" {
		t.Fatalf("created content = %q, %v", content, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("created mode = %v, %v; want 0600", info.Mode(), err)
	}

	if output, isError = callTool(t, executor, inv, "write", map[string]any{"file_path": "dir/file.txt", "content": "second"}); isError || !strings.Contains(output, "Updated file") {
		t.Fatalf("same-session overwrite after create = %q, %v", output, isError)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("overwritten mode = %v, %v; want 0600", info.Mode(), err)
	}

	unobserved := filepath.Join(root, "existing.txt")
	writeTestFile(t, unobserved, "old")
	output, isError = callTool(t, executor, inv, "write", map[string]any{"file_path": "existing.txt", "content": "new"})
	wantUnobserved := fmt.Sprintf("cannot modify %q: file has not been read — read the file, then retry", filepath.ToSlash(unobserved))
	if !isError || output != wantUnobserved {
		t.Fatalf("unobserved overwrite = %q, %v; want %q", output, isError, wantUnobserved)
	}

	if output, isError = callTool(t, executor, inv, "read", map[string]any{"file_path": "existing.txt"}); isError {
		t.Fatalf("read failed: %s", output)
	}
	output, isError = callTool(t, executor, inv, "write", map[string]any{"file_path": "existing.txt", "content": "new"})
	if isError || !strings.Contains(output, "Updated file") {
		t.Fatalf("observed overwrite = %q, %v", output, isError)
	}
	if info, err := os.Stat(unobserved); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("existing mode = %v, %v; want 0644 preserved", info.Mode(), err)
	}
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	output, isError = callTool(t, executor, inv, "write", map[string]any{"file_path": "folder", "content": "x"})
	wantFolder := fmt.Sprintf("cannot write %q: not a regular file", filepath.ToSlash(filepath.Join(root, "folder")))
	if !isError || output != wantFolder {
		t.Fatalf("directory write = %q, %v; want %q", output, isError, wantFolder)
	}
}

func TestReadErrorWordingAndDisplayPath(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	inv := ToolInvocation{SessionID: "read-errors"}

	missing := filepath.Join(root, "missing.txt")
	output, isError := callTool(t, executor, inv, "read", map[string]any{"file_path": "missing.txt"})
	wantMissing := fmt.Sprintf("cannot read %q: not found", filepath.ToSlash(missing))
	if !isError || output != wantMissing {
		t.Fatalf("missing read = %q, %v; want %q", output, isError, wantMissing)
	}

	dir := filepath.Join(root, "folder")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	output, isError = callTool(t, executor, inv, "read", map[string]any{"file_path": "folder"})
	wantDir := fmt.Sprintf("cannot read %q: not a regular file", filepath.ToSlash(dir))
	if !isError || output != wantDir {
		t.Fatalf("directory read = %q, %v; want %q", output, isError, wantDir)
	}
}

func TestEditLiteralReplacementAndLineEndings(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	inv := ToolInvocation{SessionID: "edit"}
	path := filepath.Join(root, "crlf.txt")
	writeTestFile(t, path, "alpha\r\nbeta\r\nalpha\r\n")

	output, isError := callTool(t, executor, inv, "edit", map[string]any{"file_path": "crlf.txt", "old_string": "alpha", "new_string": "gamma"})
	wantUnobserved := fmt.Sprintf("cannot modify %q: file has not been read — read the file, then retry", filepath.ToSlash(path))
	if !isError || output != wantUnobserved {
		t.Fatalf("unobserved edit = %q, %v; want %q", output, isError, wantUnobserved)
	}
	if output, isError = callTool(t, executor, inv, "read", map[string]any{"file_path": "crlf.txt"}); isError {
		t.Fatalf("read failed: %s", output)
	}
	output, isError = callTool(t, executor, inv, "edit", map[string]any{"file_path": "crlf.txt", "old_string": "alpha", "new_string": "gamma"})
	if !isError || !strings.Contains(output, "matched 2 times") {
		t.Fatalf("ambiguous edit = %q, %v", output, isError)
	}
	output, isError = callTool(t, executor, inv, "edit", map[string]any{"file_path": "crlf.txt", "old_string": "alpha", "new_string": "gamma", "replace_all": true})
	if isError || !strings.Contains(output, "All occurrences were successfully replaced.") {
		t.Fatalf("replace-all edit = %q, %v", output, isError)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "gamma\r\nbeta\r\ngamma\r\n" {
		t.Fatalf("CRLF content = %q", content)
	}
}

func TestWorkspaceWritePolicy(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("workspace-write", "workspace-write")
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inv := ToolInvocation{SessionID: "policy", Cwd: workspace}
	if output, isError := callTool(t, executor, inv, "write", map[string]any{"file_path": "inside.txt", "content": "ok"}); isError {
		t.Fatalf("inside write failed: %s", output)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	output, isError := callTool(t, executor, inv, "write", map[string]any{"file_path": outside, "content": "no"})
	if !isError || !strings.Contains(output, "path is outside the session workspace") {
		t.Fatalf("outside write = %q, %v", output, isError)
	}
}

func TestBashExecutionOutputAndTimeout(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	inv := ToolInvocation{SessionID: "bash"}

	output, isError := callTool(t, executor, inv, "bash", map[string]any{"description": "Print greeting", "command": "printf 'hello\\n'"})
	if isError || output != "hello\n" {
		t.Fatalf("bash output = %q, %v", output, isError)
	}
	output, isError = callTool(t, executor, inv, "bash", map[string]any{"description": "Print streams", "command": "printf out; printf err >&2; exit 3"})
	if isError {
		t.Fatalf("non-zero exit was treated as an error: %s", output)
	}
	want := "out\n[stderr]\nerr\n[exit code: 3]"
	if output != want {
		t.Fatalf("bash streams = %q, want %q", output, want)
	}
	output, isError = callTool(t, executor, inv, "bash", map[string]any{"description": "Show workdir", "command": "pwd", "workdir": "."})
	if isError || strings.TrimSpace(output) != root {
		t.Fatalf("bash workdir = %q, %v", output, isError)
	}
	start := time.Now()
	output, isError = callTool(t, executor, inv, "bash", map[string]any{"description": "Sleep briefly", "command": "sleep 2", "timeoutMs": 100})
	if isError || !strings.Contains(output, "[timed out after 100ms]") {
		t.Fatalf("timeout output = %q, %v", output, isError)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestBashManagedEnvironment(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	home := filepath.Join(root, ".dsh-go")
	executor.SetDSHHome(home)
	inv := ToolInvocation{SessionID: "env-session"}

	t.Setenv("DEEPSEEK_API_KEY", "harness-key")
	t.Setenv("SERVICE_TOKEN", "service-token")
	t.Setenv("DB_PASSWORD", "password")
	t.Setenv("MY_SECRET", "secret")
	t.Setenv("api_key", "lowercase-key")
	t.Setenv("DSH_HOME", "/ambient/home")
	t.Setenv("DSH_GO_TOOL_ROOT", "/ambient/root")

	command := `printf '%s\n' "$DSH_HOME" "$DSH_SHELL" "$DSH_SESSION_ID" "${DEEPSEEK_API_KEY-unset}" "${SERVICE_TOKEN-unset}" "${DB_PASSWORD-unset}" "${MY_SECRET-unset}" "${api_key-unset}" "${DSH_GO_TOOL_ROOT-unset}" "${PATH:+set}"`
	output, isError := callTool(t, executor, inv, "bash", map[string]any{"description": "Inspect managed environment", "command": command})
	if isError {
		t.Fatalf("environment command failed: %s", output)
	}
	want := strings.Join([]string{
		home,
		"1",
		"env-session",
		"unset",
		"unset",
		"unset",
		"unset",
		"unset",
		"unset",
		"set",
		"",
	}, "\n")
	if output != want {
		t.Fatalf("managed environment:\n%s\nwant:\n%s", output, want)
	}
}

func TestBashValidation(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	inv := ToolInvocation{SessionID: "bash"}
	output, isError := callTool(t, executor, inv, "bash", map[string]any{"command": "true"})
	if !isError || output != "invalid description: expected a non-empty string" {
		t.Fatalf("missing description = %q, %v", output, isError)
	}
	output, isError = callTool(t, executor, inv, "bash", map[string]any{"description": "No command", "command": "  "})
	if !isError || output != "invalid command: expected a non-empty string" {
		t.Fatalf("missing command = %q, %v", output, isError)
	}
	output, isError = callTool(t, executor, inv, "bash", map[string]any{"description": "Bad timeout", "command": "true", "timeoutMs": "soon"})
	if !isError || output != "invalid timeoutMs: expected a positive number, got soon" {
		t.Fatalf("bad timeout = %q, %v", output, isError)
	}
}

func TestGlobIncludesHiddenAndExcludesVCS(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	writeTestFile(t, filepath.Join(root, "a.txt"), "a")
	writeTestFile(t, filepath.Join(root, "sub", "b.txt"), "b")
	writeTestFile(t, filepath.Join(root, ".hidden.txt"), "h")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "g")
	writeTestFile(t, filepath.Join(root, ".git", "objects", "x"), "x")

	output, isError := callTool(t, executor, ToolInvocation{SessionID: "glob"}, "glob", map[string]any{"pattern": "*.txt"})
	if isError {
		t.Fatalf("glob failed: %s", output)
	}
	for _, want := range []string{"a.txt", "sub/b.txt", ".hidden.txt"} {
		if !strings.Contains(output, want) {
			t.Errorf("glob output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, ".git") {
		t.Errorf("glob output included VCS metadata:\n%s", output)
	}
	output, isError = callTool(t, executor, ToolInvocation{SessionID: "glob"}, "glob", map[string]any{"pattern": "*.nope"})
	if isError || output != "No files found" {
		t.Fatalf("empty glob = %q, %v", output, isError)
	}
}

func TestGlobCapAndSpill(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	for index := 0; index < globResultLimit+3; index++ {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("file-%03d.txt", index)), "x")
	}
	sessionDir := t.TempDir()
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "glob-cap", SessionDir: sessionDir}, "glob", map[string]any{"pattern": "*.txt"})
	if isError {
		t.Fatalf("glob failed: %s", output)
	}
	if !strings.Contains(output, "(Showing 100 of 103 paths. Full sorted result stored at:") {
		t.Fatalf("missing glob cap footer:\n%s", output)
	}
	entries, err := os.ReadDir(filepath.Join(sessionDir, "spill"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("spill files = %v, %v", entries, err)
	}
}

func TestGlobPathSpellingAndExplicitTargets(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	writeTestFile(t, filepath.Join(root, "sub", "a.txt"), "a")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "g")
	inv := ToolInvocation{SessionID: "glob-paths"}

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"default", map[string]any{"pattern": "*.txt"}, "sub/a.txt"},
		{"dot", map[string]any{"pattern": "*.txt", "path": "."}, "./sub/a.txt"},
		{"dot-slash-sub", map[string]any{"pattern": "*.txt", "path": "./sub"}, "./sub/a.txt"},
		{"relative-sub", map[string]any{"pattern": "*.txt", "path": "sub"}, "sub/a.txt"},
		{"absolute-sub", map[string]any{"pattern": "*.txt", "path": filepath.Join(root, "sub")}, "sub/a.txt"},
	}
	for _, tc := range cases {
		output, isError := callTool(t, executor, inv, "glob", tc.args)
		if isError {
			t.Fatalf("%s: glob failed: %s", tc.name, output)
		}
		if output != tc.want {
			t.Errorf("%s: glob = %q, want %q", tc.name, output, tc.want)
		}
	}

	// An explicit file is returned even when the pattern does not match; only
	// directory searches apply the pattern and VCS filters.
	output, isError := callTool(t, executor, inv, "glob", map[string]any{"pattern": "*.txt", "path": ".git/config"})
	if isError || output != ".git/config" {
		t.Fatalf("explicit file target = %q, %v", output, isError)
	}
	output, isError = callTool(t, executor, inv, "glob", map[string]any{"pattern": "*", "path": ".git"})
	if isError || output != "No files found" {
		t.Fatalf("VCS search root = %q, %v", output, isError)
	}
}

func TestGlobOrdersByAscendingModificationTime(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	newer := filepath.Join(root, "newer.txt")
	older := filepath.Join(root, "older.txt")
	writeTestFile(t, newer, "n")
	writeTestFile(t, older, "o")
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(newer, base.Add(2*time.Hour), base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(older, base, base); err != nil {
		t.Fatal(err)
	}
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "glob-mtime"}, "glob", map[string]any{"pattern": "*.txt"})
	if isError {
		t.Fatalf("glob failed: %s", output)
	}
	if output != "older.txt\nnewer.txt" {
		t.Fatalf("glob order = %q", output)
	}
}

func TestGlobSamplesAcrossTopLevelEntries(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	stamp := 0
	writeGroup := func(dir string, count int) {
		for index := 0; index < count; index++ {
			file := filepath.Join(root, dir, fmt.Sprintf("file-%03d.txt", index))
			writeTestFile(t, file, "x")
			when := base.Add(time.Duration(stamp) * time.Second)
			stamp++
			if err := os.Chtimes(file, when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeGroup("dir-a", 50)
	writeGroup("dir-b", 50)
	writeGroup("dir-c", 3)

	sessionDir := t.TempDir()
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "glob-sample", SessionDir: sessionDir}, "glob", map[string]any{"pattern": "*.txt"})
	if isError {
		t.Fatalf("glob failed: %s", output)
	}
	if !strings.Contains(output, "dir-c/file-000.txt") {
		t.Fatalf("over-cap page did not sample the third top-level entry:\n%s", output)
	}
	wantBasis := "(Showing 100 of 103 paths, sampled across 3 of the 3 top-level entries this pattern matched instead of taken in modification-time order. Full sorted result stored at:"
	if !strings.Contains(output, wantBasis) {
		t.Fatalf("missing sampled footer %q:\n%s", wantBasis, output)
	}
	if strings.Contains(output, "Narrow path to inspect a specific subtree") {
		t.Fatalf("unexpected narrowing hint when every top-level entry was sampled:\n%s", output)
	}
}

func TestGlobSamplesAbsoluteTargetAsOneGroup(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	sub := filepath.Join(root, "sub")
	for index := 0; index < 103; index++ {
		writeTestFile(t, filepath.Join(sub, fmt.Sprintf("file-%03d.txt", index)), "x")
	}
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "glob-absolute", SessionDir: t.TempDir()}, "glob", map[string]any{
		"pattern": "*.txt",
		"path":    sub,
	})
	if isError {
		t.Fatalf("glob failed: %s", output)
	}
	wantBasis := "(Showing 100 of 103 paths, sampled across 1 of the 1 top-level entries this pattern matched"
	if !strings.Contains(output, wantBasis) {
		t.Fatalf("absolute target did not sample as one group:\n%s", output)
	}
}

func TestGrepDisplaysCallerPathSpelling(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	writeTestFile(t, filepath.Join(root, "b.txt"), "alpha\n")
	writeTestFile(t, filepath.Join(root, "sub", "a.txt"), "alpha\n")
	inv := ToolInvocation{SessionID: "grep-paths"}

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"omitted", map[string]any{"pattern": "alpha"}, "\nb.txt\nLine 1: alpha"},
		{"dot", map[string]any{"pattern": "alpha", "path": "."}, "\n./b.txt\nLine 1: alpha"},
		{"relative-sub", map[string]any{"pattern": "alpha", "path": "sub"}, "\nsub/a.txt\nLine 1: alpha"},
		{"dot-slash-sub", map[string]any{"pattern": "alpha", "path": "./sub"}, "\n./sub/a.txt\nLine 1: alpha"},
		{"absolute-sub", map[string]any{"pattern": "alpha", "path": filepath.Join(root, "sub")}, "\nsub/a.txt\nLine 1: alpha"},
	}
	for _, tc := range cases {
		output, isError := callTool(t, executor, inv, "grep", tc.args)
		if isError {
			t.Fatalf("%s: grep failed: %s", tc.name, output)
		}
		if !strings.Contains(output, tc.want) {
			t.Errorf("%s: grep output %q does not contain %q", tc.name, output, tc.want)
		}
	}
}

func TestGrepGroupsIncludesAndCaps(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	writeTestFile(t, filepath.Join(root, "a.go"), "alpha\nbeta alpha\n")
	writeTestFile(t, filepath.Join(root, "b.txt"), "alpha\n")
	writeTestFile(t, filepath.Join(root, "sub", "c.go"), "alpha\n")
	inv := ToolInvocation{SessionID: "grep"}

	output, isError := callTool(t, executor, inv, "grep", map[string]any{"pattern": "alpha", "include": "*.go"})
	if isError {
		t.Fatalf("grep failed: %s", output)
	}
	// ripgrep does not promise file order when it scans in parallel; group
	// content and numbering are the contract, not group order.
	if !strings.HasPrefix(output, "Found 3 matches\n\n") {
		t.Fatalf("grep header:\n%s", output)
	}
	for _, group := range []string{"a.go\nLine 1: alpha\nLine 2: beta alpha", "sub/c.go\nLine 1: alpha"} {
		if !strings.Contains(output, group) {
			t.Fatalf("grep output missing group %q:\n%s", group, output)
		}
	}
	output, isError = callTool(t, executor, inv, "grep", map[string]any{"pattern": "missing"})
	if isError || output != "No matches found" {
		t.Fatalf("empty grep = %q, %v", output, isError)
	}
	output, isError = callTool(t, executor, inv, "grep", map[string]any{"pattern": "alpha", "include": "!*.go"})
	if !isError || !strings.Contains(output, "include must be a positive glob filter") {
		t.Fatalf("negated include = %q, %v", output, isError)
	}
}

func TestGrepCapAndSpill(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	var content strings.Builder
	for index := 0; index < grepMatchLimit+2; index++ {
		content.WriteString("hit\n")
	}
	writeTestFile(t, filepath.Join(root, "many.txt"), content.String())
	sessionDir := t.TempDir()
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "grep-cap", SessionDir: sessionDir}, "grep", map[string]any{"pattern": "hit"})
	if isError {
		t.Fatalf("grep failed: %s", output)
	}
	if !strings.Contains(output, "Found 250 of 252 matches") || !strings.Contains(output, "Full grep result stored at:") {
		t.Fatalf("missing grep cap footer:\n%s", output)
	}
}

func TestUnknownToolAndInvalidArguments(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	inv := ToolInvocation{SessionID: "unknown", Name: "nope", Arguments: "{}"}
	if output, isError := executor.Execute(context.Background(), inv); !isError || output != "unknown tool: nope" {
		t.Fatalf("unknown tool = %q, %v", output, isError)
	}
	inv = ToolInvocation{SessionID: "unknown", Name: "read", Arguments: "{"}
	if output, isError := executor.Execute(context.Background(), inv); !isError || !strings.HasPrefix(output, "invalid tool arguments:") {
		t.Fatalf("invalid args = %q, %v", output, isError)
	}
}
