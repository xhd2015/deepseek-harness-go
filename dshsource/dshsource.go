// Package dshsource locates the DeepSeek Harness checkout whose frontend this
// project embeds.
//
// The frontend is not an npm dependency: it is built from a git checkout. The
// lookup order matches the rest of the toolchain so one machine configures it
// once:
//
//  1. DSH_ROOT — a local checkout that must exist.
//  2. .deepseek-harness.json "local" — relative to the file that was found.
//  3. .deepseek-harness.json "repo" + "branch" — cloned into the cache.
//  4. The official repository's main branch, in the same cache.
//
// A linked worktree without its own config falls back to the main checkout's
// copy, because the file is gitignored and would otherwise have to be
// duplicated in every worktree.
package dshsource

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ConfigName is the per-checkout configuration file.
const ConfigName = ".deepseek-harness.json"

// ExampleName is the tracked template a user copies.
const ExampleName = ".deepseek-harness.example.json"

// EnvRoot names a checkout directly and wins over the configuration file.
const EnvRoot = "DSH_ROOT"

// OfficialRepo and OfficialBranch are the fallback source.
const (
	OfficialRepo   = "https://github.com/deepseek-ai/deepseek-harness"
	OfficialBranch = "main"
)

// Kind reports which lookup step chose the checkout.
type Kind string

const (
	KindEnv      Kind = "env"
	KindLocal    Kind = "local"
	KindCustom   Kind = "custom"
	KindOfficial Kind = "official"
)

// Config is the on-disk configuration file.
type Config struct {
	// Local is a checkout path, relative to the file's directory.
	Local string `json:"local,omitempty"`
	// Repo and Branch select a fork to clone and keep refreshed.
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// Plan is where the frontend source will come from.
type Plan struct {
	Kind Kind
	// Root is the absolute checkout path.
	Root string
	// Repo and Branch are set when the checkout is cloned or fetched.
	Repo   string
	Branch string
	// Notice explains the choice, for the build log.
	Notice string
	// ConfigFile is the file that decided the plan, when one was read.
	ConfigFile string
	// FromMain records that the config came from the main checkout because
	// this tree is a linked worktree without its own copy.
	FromMain bool
}

// Options overrides the process environment. The zero value is production.
type Options struct {
	// Root is this repository; empty discovers it from the working directory.
	Root string
	// Getenv reads DSH_ROOT; nil uses os.Getenv.
	Getenv func(string) string
	// UserHome locates the cache; nil uses os.UserHomeDir.
	UserHome func() (string, error)
	// Git runs git with args in dir and returns stdout; nil runs the git
	// binary. Tests inject a fake.
	Git func(dir string, args ...string) (string, error)
	// Log receives the notice; nil is silent.
	Log io.Writer
}

// ErrCheckoutMissing reports a configured checkout that is not on disk.
var ErrCheckoutMissing = errors.New("dsh source: checkout not found")

// Resolve plans the source without touching the network or the filesystem
// beyond reading the config file.
func Resolve(opts Options) (Plan, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		discovered, err := FindRoot()
		if err != nil {
			return Plan{}, err
		}
		root = discovered
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Plan{}, fmt.Errorf("dsh source: resolve %s: %w", root, err)
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	if fromEnv := strings.TrimSpace(getenv(EnvRoot)); fromEnv != "" {
		absolute, err := filepath.Abs(fromEnv)
		if err != nil {
			return Plan{}, fmt.Errorf("dsh source: resolve %s: %w", EnvRoot, err)
		}
		if !dirExists(absolute) {
			return Plan{}, fmt.Errorf("%w: %s=%s", ErrCheckoutMissing, EnvRoot, absolute)
		}
		return Plan{
			Kind:   KindEnv,
			Root:   absolute,
			Notice: fmt.Sprintf("frontend source %s: %s", EnvRoot, absolute),
		}, nil
	}

	located, err := locateConfig(root, opts)
	if err != nil {
		return Plan{}, err
	}
	config := Config{}
	if located != nil {
		config, err = readConfig(located.file)
		if err != nil {
			return Plan{}, err
		}
	}
	origin := ConfigName
	if located != nil && located.fromMain {
		origin = fmt.Sprintf("%s from main checkout %s", ConfigName, located.dir)
	}
	base := root
	if located != nil {
		base = located.dir
	}

	if config.Local != "" {
		absolute := config.Local
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(base, config.Local)
		}
		absolute, err := filepath.Abs(absolute)
		if err != nil {
			return Plan{}, fmt.Errorf("dsh source: resolve local: %w", err)
		}
		if !dirExists(absolute) {
			return Plan{}, fmt.Errorf("%w: %s local = %s", ErrCheckoutMissing, origin, absolute)
		}
		return Plan{
			Kind:       KindLocal,
			Root:       absolute,
			Notice:     fmt.Sprintf("frontend source %s: %s", origin, absolute),
			ConfigFile: located.file,
			FromMain:   located.fromMain,
		}, nil
	}

	home, err := userHome(opts)
	if err != nil {
		return Plan{}, err
	}
	repo, branch := config.Repo, config.Branch
	if repo != "" && branch != "" {
		return Plan{
			Kind:       KindCustom,
			Root:       CacheDir(repo, branch, home),
			Repo:       repo,
			Branch:     branch,
			Notice:     fmt.Sprintf("frontend source %s repo %s (branch %s)", origin, repo, branch),
			ConfigFile: located.configFileOr(),
			FromMain:   located.fromMain,
		}, nil
	}
	return Plan{
		Kind:   KindOfficial,
		Root:   CacheDir(OfficialRepo, OfficialBranch, home),
		Repo:   OfficialRepo,
		Branch: OfficialBranch,
		Notice: fmt.Sprintf("frontend source official repo %s (branch %s)", OfficialRepo, OfficialBranch),
	}, nil
}

// Ensure resolves the source, materializes a cloned checkout when needed, and
// reports the plan it used.
func Ensure(opts Options) (Plan, error) {
	plan, err := Resolve(opts)
	if err != nil {
		return Plan{}, err
	}
	if plan.Kind == KindEnv || plan.Kind == KindLocal {
		// Both were asserted to exist during resolution.
		logNotice(opts, plan.Notice)
		return plan, nil
	}
	if err := syncCheckout(plan, opts); err != nil {
		return Plan{}, err
	}
	if !dirExists(plan.Root) {
		return Plan{}, fmt.Errorf("%w: %s", ErrCheckoutMissing, plan.Root)
	}
	logNotice(opts, plan.Notice)
	return plan, nil
}

// CacheDir is where a cloned checkout lives for one repo and branch.
func CacheDir(repo, branch, home string) string {
	return filepath.Join(home, ".cache", "dsh-go", "deepseek-harness", repoKey(repo), branchKey(branch))
}

// repoKey turns a clone URL into one path element.
func repoKey(repo string) string {
	value := strings.TrimSpace(repo)
	value = strings.TrimSuffix(value, ".git")
	if rest, ok := strings.CutPrefix(value, "git@"); ok {
		value = strings.Replace(rest, ":", "/", 1)
	}
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		value = strings.TrimPrefix(value, prefix)
	}
	return strings.Trim(value, "/")
}

// branchKey turns a branch name into one path element.
func branchKey(branch string) string {
	return strings.ReplaceAll(branch, "/", "--")
}

// FindRoot walks up from the working directory to the module root.
func FindRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("dsh source: working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("dsh source: no go.mod above the working directory")
		}
		dir = parent
	}
}

type located struct {
	file     string
	dir      string
	fromMain bool
}

// configFileOr returns the config path, empty when no file was involved.
func (l *located) configFileOr() string {
	if l == nil {
		return ""
	}
	return l.file
}

// locateConfig finds the config in this checkout, else in the main worktree.
func locateConfig(root string, opts Options) (*located, error) {
	here := filepath.Join(root, ConfigName)
	if fileExists(here) {
		return &located{file: here, dir: root}, nil
	}
	main, err := mainWorktree(root, opts)
	if err != nil {
		return nil, err
	}
	if main == "" || filepath.Clean(main) == filepath.Clean(root) {
		return nil, nil
	}
	there := filepath.Join(main, ConfigName)
	if !fileExists(there) {
		return nil, nil
	}
	return &located{file: there, dir: main, fromMain: true}, nil
}

// mainWorktree returns the first worktree of this repository, or "" when this
// tree is not a linked worktree.
func mainWorktree(root string, opts Options) (string, error) {
	git := gitCommand(opts)
	gitDir, err := git(root, "rev-parse", "--git-dir")
	if err != nil {
		return "", nil
	}
	commonDir, err := git(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", nil
	}
	if samePath(root, gitDir) == samePath(root, commonDir) {
		return "", nil
	}
	porcelain, err := git(root, "worktree", "list", "--porcelain")
	if err != nil {
		return "", nil
	}
	for _, line := range strings.Split(porcelain, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", nil
}

// samePath compares two git-reported paths after resolving them against root.
func samePath(root, reported string) string {
	if !filepath.IsAbs(reported) {
		reported = filepath.Join(root, reported)
	}
	absolute, err := filepath.Abs(reported)
	if err != nil {
		return filepath.Clean(reported)
	}
	return filepath.Clean(absolute)
}

func readConfig(file string) (Config, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Config{}, fmt.Errorf("dsh source: read %s: %w", file, err)
	}
	var parsed Config
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Config{}, fmt.Errorf("dsh source: %s is not valid JSON: %w", file, err)
	}
	parsed.Local = strings.TrimSpace(parsed.Local)
	parsed.Repo = strings.TrimSpace(parsed.Repo)
	parsed.Branch = strings.TrimSpace(parsed.Branch)
	if err := parsed.validate(); err != nil {
		return Config{}, fmt.Errorf("dsh source: %s: %w", file, err)
	}
	return parsed, nil
}

// validate rejects configurations whose meaning would be ambiguous.
func (c Config) validate() error {
	if c.Local != "" && (c.Repo != "" || c.Branch != "") {
		return errors.New("cannot set both local and repo/branch")
	}
	if (c.Repo == "") != (c.Branch == "") {
		return errors.New("repo and branch must be set together")
	}
	return nil
}

// syncCheckout clones the source, or refreshes an existing clone of the branch.
func syncCheckout(plan Plan, opts Options) error {
	if plan.Repo == "" || plan.Branch == "" {
		return fmt.Errorf("dsh source: %s needs a repo and branch", plan.Kind)
	}
	git := gitCommand(opts)
	// .git is a directory in a normal clone and a file in a linked worktree, so
	// either one means "already cloned".
	if pathExists(filepath.Join(plan.Root, ".git")) {
		if _, err := git(plan.Root, "fetch", "--depth", "1", "origin", plan.Branch); err != nil {
			return fmt.Errorf("dsh source: fetch %s: %w", plan.Branch, err)
		}
		if _, err := git(plan.Root, "checkout", "-B", plan.Branch, "FETCH_HEAD"); err != nil {
			return fmt.Errorf("dsh source: checkout %s: %w", plan.Branch, err)
		}
		return nil
	}
	if dirExists(plan.Root) {
		return fmt.Errorf("dsh source: cache path exists but is not a git checkout: %s", plan.Root)
	}
	if err := os.MkdirAll(filepath.Dir(plan.Root), 0o755); err != nil {
		return fmt.Errorf("dsh source: create cache: %w", err)
	}
	partial := plan.Root + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return fmt.Errorf("dsh source: clear %s: %w", partial, err)
	}
	_, err := git(filepath.Dir(plan.Root), "clone", "--depth", "1", "--branch", plan.Branch,
		"--single-branch", plan.Repo, partial)
	if err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("dsh source: clone %s: %w", plan.Repo, err)
	}
	if err := os.Rename(partial, plan.Root); err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("dsh source: publish clone: %w", err)
	}
	return nil
}

func gitCommand(opts Options) func(dir string, args ...string) (string, error) {
	if opts.Git != nil {
		return opts.Git
	}
	return func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

func userHome(opts Options) (string, error) {
	home := opts.UserHome
	if home == nil {
		home = os.UserHomeDir
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("dsh source: resolve home: %w", err)
	}
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("dsh source: HOME is unset")
	}
	return dir, nil
}

func logNotice(opts Options, notice string) {
	if opts.Log == nil || notice == "" {
		return
	}
	fmt.Fprintln(opts.Log, notice)
}

// pathExists reports whether anything, file or directory, is at path.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
