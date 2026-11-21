package frontend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/xhd2015/deepseek-harness-go/dshsource"
)

// PrepareOptions controls the production frontend path.
type PrepareOptions struct {
	// Root is this module; empty discovers it.
	Root string
	// Source captures from an already running server instead of building one.
	// It is the development override; production leaves it empty.
	Source string
	// Token authenticates --source.
	Token string
	// Out is the capture directory; empty uses <root>/assets/dist.
	Out string
	// Force rebuilds and recaptures even when the staged capture is complete.
	Force bool
	// Log receives progress; nil discards it.
	Log io.Writer
	// DSH overrides the checkout lookup (tests, alternate environments).
	DSH dshsource.Options
	// BuildTimeout bounds a checkout build; zero uses the package default.
	BuildTimeout time.Duration
}

// PrepareResult describes what was staged and where it came from.
type PrepareResult struct {
	// Out is the capture directory.
	Out string
	// Source is the URL the capture read.
	Source string
	// DSH is the checkout that produced it, empty when --source was used.
	DSH dshsource.Plan
	// Assets is the number of captured responses.
	Assets int
	// Bytes is their total size.
	Bytes int64
	// Rev and Version identify the frontend build.
	Rev     string
	Version string
	// Reused is true when an existing complete capture was kept.
	Reused bool
}

// Prepare stages a complete, current capture under the module's assets
// directory. It builds the configured checkout when needed, serves it once on
// an ephemeral port, captures it, and stops it, so a production build needs
// neither a Node process left running nor a Harness server on 3080.
func Prepare(ctx context.Context, opts PrepareOptions) (PrepareResult, error) {
	root := opts.Root
	if root == "" {
		discovered, err := dshsource.FindRoot()
		if err != nil {
			return PrepareResult{}, err
		}
		root = discovered
	}
	out := opts.Out
	if out == "" {
		out = filepath.Join(root, "assets", "dist")
	}
	dshOpts := opts.DSH
	if dshOpts.Root == "" {
		dshOpts.Root = root
	}
	if dshOpts.Log == nil {
		dshOpts.Log = opts.Log
	}

	// A complete capture is reused, but the source is still resolved and
	// reported (cheaply, without cloning), so the log always says where an
	// embed came from and where a fresh one would come from.
	if !opts.Force && opts.Source == "" && Complete(out) {
		count, _ := Count(out)
		plan, err := dshsource.Resolve(dshOpts)
		if err != nil {
			logf(opts.Log, "warning: reuse the staged frontend without a checkout: %v", err)
			return PrepareResult{Out: out, Assets: count, Reused: true}, nil
		}
		logf(opts.Log, "%s (staged in %s)", plan.Notice, out)
		logf(opts.Log, "reusing the staged capture; pass --force-frontend to recapture")
		return PrepareResult{Out: out, Assets: count, Reused: true, DSH: plan}, nil
	}

	source := opts.Source
	var plan dshsource.Plan
	var server *dshsource.Server
	if source == "" {
		resolved, err := dshsource.Ensure(dshOpts)
		if err != nil {
			return PrepareResult{}, err
		}
		plan = resolved
		if _, err := dshsource.Build(ctx, dshsource.BuildOptions{
			Root:    plan.Root,
			Log:     opts.Log,
			Timeout: opts.BuildTimeout,
		}); err != nil {
			return PrepareResult{}, err
		}
		started, err := dshsource.Serve(ctx, dshsource.ServeOptions{Root: plan.Root, Log: opts.Log})
		if err != nil {
			return PrepareResult{}, err
		}
		server = started
		source = started.Origin
		logf(opts.Log, "serving the checkout frontend at %s", source)
	}
	defer func() {
		if server != nil {
			if err := server.Stop(); err != nil {
				logf(opts.Log, "warning: stop the temporary Web server: %v", err)
			}
		}
	}()

	captured, err := Capture(CaptureOptions{
		Source: source,
		Out:    out,
		Token:  opts.Token,
		Log:    opts.Log,
	})
	if err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{
		Out:     captured.Out,
		Source:  source,
		DSH:     plan,
		Assets:  len(captured.Manifest.Entries),
		Bytes:   captured.TotalBytes,
		Rev:     captured.Rev,
		Version: captured.Version,
	}, nil
}

// RequiresBundle reports whether a build would embed an incomplete frontend.
func RequiresBundle(root string) bool {
	_, ok := Count(filepath.Join(root, "assets", "dist"))
	return !ok
}

// ErrNoAssets reports a build that would embed no frontend.
var ErrNoAssets = errors.New("frontend: no captured frontend to embed")

// HumanSize formats a byte count for build output.
func HumanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}
