// Package build compiles dsh-go with its embedded frontend and reports what
// went into the binary.
package build

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ModulePath is this module.
const ModulePath = "github.com/xhd2015/deepseek-harness-go"

// BinaryName is the installed command name.
const BinaryName = "dsh-go"

// MainPackage is the command built by the scripts.
const MainPackage = "./cmd/dsh-go"

// Options controls one build.
type Options struct {
	// Root is the module root; empty discovers it from the working directory.
	Root string
	// Package is the main package; empty builds MainPackage.
	Package string
	// Output is the binary path; empty writes dist/dsh-go.
	Output string
	// Version overrides the version stamped into the binary.
	Version string
	// GoBinary overrides the go command; empty uses $GO or PATH.
	GoBinary string
	// GOOS and GOARCH cross-compile for another platform. Empty inherits the
	// process environment.
	GOOS   string
	GOARCH string
	// Ldflags replaces the generated linker flags.
	Ldflags string
	// AllowMissingAssets permits building before the frontend is captured.
	AllowMissingAssets bool
	// Stdout and Stderr receive command output; nil discards it.
	Stdout, Stderr io.Writer
}

// Result describes a completed build.
type Result struct {
	Path    string
	Version string
	Size    int64
	// Responses is the number of captured frontend responses embedded.
	Responses int
	// Assets is false when the frontend was not captured.
	Assets bool
}

// Build compiles the command.
func Build(opts Options) (Result, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		discovered, err := FindRoot()
		if err != nil {
			return Result{}, err
		}
		root = discovered
	}
	pkg := opts.Package
	if pkg == "" {
		pkg = MainPackage
	}
	output := opts.Output
	if output == "" {
		output = filepath.Join(root, "dist", BinaryName)
		if runtime.GOOS == "windows" {
			output += ".exe"
		}
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = DetectVersion(root)
	}
	responses, captured := AssetCount(root)
	if !captured && !opts.AllowMissingAssets {
		return Result{}, fmt.Errorf("no frontend captured in %s: run `go run ./script/sync-assets` first, "+
			"or pass --allow-missing-assets for a backend-only build", filepath.Join(root, "assets", "dist"))
	}
	goBinary, err := goCommand(opts.GoBinary)
	if err != nil {
		return Result{}, err
	}
	ldflags := opts.Ldflags
	if ldflags == "" {
		ldflags = fmt.Sprintf("-s -w -X %s/version.Version=%s", ModulePath, version)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return Result{}, fmt.Errorf("build: create output directory: %w", err)
	}

	args := []string{"build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", output, pkg}
	cmd := exec.Command(goBinary, args...)
	cmd.Dir = root
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if opts.GOOS != "" || opts.GOARCH != "" {
		cmd.Env = targetEnv(os.Environ(), opts.GOOS, opts.GOARCH)
	}
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("build: %s %s: %w", goBinary, strings.Join(args, " "), err)
	}
	info, err := os.Stat(output)
	if err != nil {
		return Result{}, fmt.Errorf("build: stat %s: %w", output, err)
	}
	return Result{
		Path:      output,
		Version:   version,
		Size:      info.Size(),
		Responses: responses,
		Assets:    captured,
	}, nil
}

// targetEnv returns base with the requested target platform, forcing
// CGO_ENABLED=0 when cross-compiling so no cross C toolchain is needed.
func targetEnv(base []string, goos, goarch string) []string {
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	cross := goos != runtime.GOOS || goarch != runtime.GOARCH
	drop := map[string]bool{"GOOS": true, "GOARCH": true, "CGO_ENABLED": true}
	if cross {
		drop["GOFLAGS"] = true
	}
	env := make([]string, 0, len(base)+3)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && drop[key] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOOS="+goos, "GOARCH="+goarch)
	if cross {
		env = append(env, "CGO_ENABLED=0")
	}
	return env
}

// FindRoot walks upwards from the working directory to the module root.
func FindRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("build: working directory: %w", err)
	}
	for {
		if moduleRoot(dir) {
			return dir, nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			return "", fmt.Errorf("build: run from inside the %s module", ModulePath)
		}
		dir = next
	}
}

func moduleRoot(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")) == ModulePath
		}
	}
	return false
}

// DetectVersion prefers the git description and falls back to a static label.
func DetectVersion(root string) string {
	cmd := exec.Command("git", "-C", root, "describe", "--tags", "--always", "--dirty")
	cmd.Stderr = io.Discard
	if out, err := cmd.Output(); err == nil {
		if version := strings.TrimSpace(string(out)); version != "" {
			return version
		}
	}
	return "0.1.0-dev"
}

type manifestEntries struct {
	Entries map[string]json.RawMessage `json:"entries"`
}

// AssetCount reports how many captured responses are staged.
func AssetCount(root string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(root, "assets", "dist", "manifest.json"))
	if err != nil {
		return 0, false
	}
	var manifest manifestEntries
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, false
	}
	if len(manifest.Entries) == 0 {
		return 0, false
	}
	return len(manifest.Entries), true
}

// GoCommand exposes the resolved go binary for scripts that shell out.
func GoCommand(override string) (string, error) { return goCommand(override) }

func goCommand(override string) (string, error) {
	if candidate := strings.TrimSpace(override); candidate != "" {
		return candidate, nil
	}
	if candidate := strings.TrimSpace(os.Getenv("GO")); candidate != "" {
		return candidate, nil
	}
	found, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("build: no go command in PATH: %w", err)
	}
	return found, nil
}

// CopyFile copies src over dst.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
