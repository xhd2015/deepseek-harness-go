package dshsource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit serves the handful of git answers the worktree probe needs.
type fakeGit struct {
	answers map[string]string
	fail    map[string]bool
}

func (f *fakeGit) run(_ string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	if f.fail[key] {
		return "", errors.New("git: " + key)
	}
	if value, ok := f.answers[key]; ok {
		return value, nil
	}
	return "", errors.New("git: unexpected " + key)
}

func baseOptions(t *testing.T, root string) Options {
	t.Helper()
	home := t.TempDir()
	return Options{
		Root:     root,
		Getenv:   func(string) string { return "" },
		UserHome: func() (string, error) { return home, nil },
		Git:      (&fakeGit{fail: map[string]bool{}}).run,
	}
}

func TestResolvePrefersEnvRoot(t *testing.T) {
	checkout := t.TempDir()
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Getenv = func(key string) string {
		if key == EnvRoot {
			return checkout
		}
		return ""
	}
	plan, err := Resolve(opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Kind != KindEnv || plan.Root != checkout {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(plan.Notice, checkout) {
		t.Fatalf("notice = %q, want the checkout path", plan.Notice)
	}
}

func TestResolveRejectsMissingEnvRoot(t *testing.T) {
	opts := baseOptions(t, t.TempDir())
	opts.Getenv = func(key string) string {
		if key == EnvRoot {
			return filepath.Join(t.TempDir(), "gone")
		}
		return ""
	}
	if _, err := Resolve(opts); !errors.Is(err, ErrCheckoutMissing) {
		t.Fatalf("err = %v, want ErrCheckoutMissing", err)
	}
}

func TestResolveLocalIsRelativeToTheFile(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "dsh")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeConfig(t, root, `{"local":"`+checkout+`"}`)
	plan, err := Resolve(baseOptions(t, root))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Kind != KindLocal || plan.Root != checkout {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.FromMain {
		t.Fatal("a local config must not be reported as coming from the main checkout")
	}
}

func TestResolveLocalRelativePath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "dsh-go")
	checkout := filepath.Join(parent, "deepseek-harness")
	for _, dir := range []string{root, checkout} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeConfig(t, root, `{"local":"../deepseek-harness"}`)
	plan, err := Resolve(baseOptions(t, root))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Root != checkout {
		t.Fatalf("root = %q, want %q", plan.Root, checkout)
	}
}

func TestResolveFallsBackToMainWorktreeConfig(t *testing.T) {
	parent := t.TempDir()
	main := filepath.Join(parent, "main")
	worktree := filepath.Join(parent, "feature")
	checkout := filepath.Join(parent, "dsh")
	for _, dir := range []string{main, worktree, checkout} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// The config lives only in the main checkout, and its local path is
	// resolved against that directory, not against the worktree.
	writeConfig(t, main, `{"local":"../dsh"}`)
	opts := baseOptions(t, worktree)
	opts.Git = (&fakeGit{answers: map[string]string{
		"rev-parse --git-dir":        "../main/.git/worktrees/feature",
		"rev-parse --git-common-dir": "../main/.git",
		"worktree list --porcelain":  "worktree " + main + "\nHEAD abc\n",
	}}).run

	plan, err := Resolve(opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Kind != KindLocal || plan.Root != checkout {
		t.Fatalf("plan = %+v", plan)
	}
	if !plan.FromMain {
		t.Fatal("the plan must record that the config came from the main checkout")
	}
	if !strings.Contains(plan.Notice, "main checkout") {
		t.Fatalf("notice = %q, want it to name the main checkout", plan.Notice)
	}
}

func TestResolveCustomRepoUsesCache(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"repo":"https://github.com/example/deepseek-harness","branch":"feat/web"}`)
	opts := baseOptions(t, root)
	plan, err := Resolve(opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Kind != KindCustom || plan.Repo != "https://github.com/example/deepseek-harness" || plan.Branch != "feat/web" {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(plan.Root, filepath.Join("deepseek-harness", "github.com", "example", "deepseek-harness", "feat--web")) {
		t.Fatalf("cache path = %q", plan.Root)
	}
}

func TestResolveOfficialIsTheFallback(t *testing.T) {
	plan, err := Resolve(baseOptions(t, t.TempDir()))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Kind != KindOfficial || plan.Repo != OfficialRepo || plan.Branch != OfficialBranch {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(plan.Root, filepath.Join(".cache", "dsh-go")) {
		t.Fatalf("cache path = %q", plan.Root)
	}
}

func TestResolveRejectsAmbiguousConfig(t *testing.T) {
	for name, body := range map[string]string{
		"both local and repo": `{"local":"../x","repo":"https://example.com/x","branch":"main"}`,
		"repo without branch": `{"repo":"https://example.com/x"}`,
		"branch without repo": `{"branch":"main"}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfig(t, root, body)
			if _, err := Resolve(baseOptions(t, root)); err == nil {
				t.Fatal("an ambiguous config was accepted")
			}
		})
	}
}

func TestResolveRejectsInvalidJSON(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{`)
	if _, err := Resolve(baseOptions(t, root)); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestEnsureClonesThenFetches(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeConfig(t, root, `{"repo":"https://example.com/dsh","branch":"main"}`)

	var calls []string
	opts := Options{
		Root:     root,
		Getenv:   func(string) string { return "" },
		UserHome: func() (string, error) { return home, nil },
		Git: func(dir string, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			if args[0] == "clone" {
				// Simulate the clone: the destination is the last argument.
				target := args[len(args)-1]
				if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
					t.Fatalf("mkdir clone: %v", err)
				}
			}
			return "", nil
		},
	}
	plan, err := Ensure(opts)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "clone --depth 1") {
		t.Fatalf("git calls = %v, want one clone", calls)
	}
	if _, err := os.Stat(filepath.Join(plan.Root, ".git")); err != nil {
		t.Fatalf("clone was not published: %v", err)
	}

	// A second run refreshes the branch instead of cloning again.
	resolved, err := Ensure(opts)
	if err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if resolved.Root != plan.Root {
		t.Fatalf("second root = %q", resolved.Root)
	}
	if len(calls) != 3 || !strings.HasPrefix(calls[1], "fetch") || !strings.HasPrefix(calls[2], "checkout -B") {
		t.Fatalf("git calls = %v, want fetch plus checkout", calls)
	}
}

func TestEnsureReportsMissingLocalCheckout(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"local":"../nowhere"}`)
	if _, err := Ensure(baseOptions(t, root)); !errors.Is(err, ErrCheckoutMissing) {
		t.Fatalf("err = %v, want ErrCheckoutMissing", err)
	}
}

func TestCacheDirKeys(t *testing.T) {
	home := "/home/user"
	got := CacheDir("git@github.com:x/deepseek-harness.git", "feat/a", home)
	want := filepath.Join(home, ".cache", "dsh-go", "deepseek-harness", "github.com", "x", "deepseek-harness", "feat--a")
	if got != want {
		t.Fatalf("CacheDir = %q, want %q", got, want)
	}
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
