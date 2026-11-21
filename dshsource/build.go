package dshsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BuildOptions controls building one checkout's frontend artifacts.
type BuildOptions struct {
	// Root is the checkout.
	Root string
	// Force rebuilds even when both artifacts are already present.
	Force bool
	// PackageManager is the pnpm executable; empty uses PATH.
	PackageManager string
	// Log receives build output; nil discards it.
	Log io.Writer
	// Timeout bounds the build; zero uses DefaultBuildTimeout.
	Timeout time.Duration
}

// DefaultBuildTimeout is generous: a cold DSH build compiles the host
// libraries, the client face, and the Web app.
const DefaultBuildTimeout = 45 * time.Minute

// BuildResult reports whether a build ran.
type BuildResult struct {
	// Built is false when existing artifacts were reused.
	Built bool
	// Duration is how long a build took.
	Duration time.Duration
}

// Build prepares the checkout's Web server and page. It builds only when an
// artifact is missing, so a checkout someone already built is reused as is.
func Build(ctx context.Context, opts BuildOptions) (BuildResult, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		return BuildResult{}, errors.New("dsh source: build needs a checkout")
	}
	if !HasFrontendPackage(root) {
		return BuildResult{}, fmt.Errorf("dsh source: %s has no %s; is it a DeepSeek Harness checkout?", root, webPackage)
	}
	cli, dist := Ready(root)
	if cli && dist && !opts.Force {
		return BuildResult{}, nil
	}

	manager := opts.PackageManager
	if manager == "" {
		manager = "pnpm"
	}
	resolved, err := exec.LookPath(manager)
	if err != nil {
		return BuildResult{}, fmt.Errorf("dsh source: %s is required to build the frontend: %w", manager, err)
	}
	if !fileExists(filepath.Join(root, "node_modules", ".bin", "tsx")) {
		return BuildResult{}, fmt.Errorf("dsh source: %s has no installed dependencies; run `%s install` there first", root, manager)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultBuildTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if opts.Log != nil {
		fmt.Fprintf(opts.Log, "building the frontend in %s (this takes a while)\n", root)
	}
	started := time.Now()
	cmd := exec.CommandContext(runCtx, resolved, "run", "build")
	cmd.Dir = root
	cmd.Env = append(scrubbedEnv(os.Environ()), "CI=1")
	cmd.Stdout = opts.Log
	cmd.Stderr = opts.Log
	if err := cmd.Run(); err != nil {
		return BuildResult{}, fmt.Errorf("dsh source: %s run build in %s: %w", manager, root, err)
	}
	if cli, dist = Ready(root); !cli || !dist {
		switch {
		case !dist:
			return BuildResult{}, fmt.Errorf("dsh source: the build produced no %s", webDistIndex)
		default:
			return BuildResult{}, fmt.Errorf("dsh source: the build produced no %s", cliEntry)
		}
	}
	return BuildResult{Built: true, Duration: time.Since(started)}, nil
}
