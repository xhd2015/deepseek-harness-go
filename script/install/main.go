// Command install builds dsh-go and installs it on PATH.
//
// The frontend is staged first (the same path `go run ./script/build` uses), so
// the installed binary serves the real DeepSeek Harness UI without a proxy.
// Then gotool/localbin/install writes it to an existing PATH entry or
// ~/.local/bin, refreshes other existing copies, and ensures ~/.local/bin is on
// PATH.
//
// Usage:
//
//	go run ./script/install [options]
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	install "github.com/xhd2015/dot-pkgs/go-pkgs/gotool/localbin/install"
	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/build"
	"github.com/xhd2015/deepseek-harness-go/frontend"
)

const help = `
Usage: go run ./script/install [options]

Stage the frontend, build dsh-go, and install the binary.

The primary destination is an existing dsh-go on PATH, otherwise
~/.local/bin/dsh-go. Other existing copies under ~/.local/bin, $GOBIN and
$GOPATH/bin are refreshed too.

INSTALL_TO_DIR overrides the install directory, and INSTALL_GOOS/INSTALL_GOARCH
cross-compile the binary (used by remote-agent install).

Frontend source lookup, first match wins:
  1. DSH_ROOT                     local checkout (must exist)
  2. .deepseek-harness.json local path, relative to the file
  3. .deepseek-harness.json repo + branch   cloned into ~/.cache/dsh-go
  4. official deepseek-ai/deepseek-harness main

Options:
      --dir DIR         install into DIR instead of ~/.local/bin
      --version VERSION version stamped into the binary (default: git describe)
      --source URL      capture from an already running Harness server
      --token TOKEN     auth token for --source
      --force-bundle    rebuild and recapture even when a capture exists
      --skip-frontend   install without touching the staged frontend
      --allow-missing-assets
                        install a backend-only binary
  -h, --help            show this help
`

func main() {
	var (
		dir                string
		version            string
		source             string
		token              string
		forceBundle        bool
		skipFrontend       bool
		allowMissingAssets bool
	)
	remain, err := lessflags.String("--dir", &dir).
		String("--version", &version).
		String("--source", &source).
		String("--token", &token).
		Bool("--force-bundle", &forceBundle).
		Bool("--skip-frontend", &skipFrontend).
		Bool("--allow-missing-assets", &allowMissingAssets).
		Help("-h,--help", help).
		Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, lessflags.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}
	if len(remain) > 0 {
		fmt.Fprintf(os.Stderr, "install: unexpected arguments %q\n", remain)
		os.Exit(2)
	}

	root, err := build.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}
	// An explicit destination wins. Otherwise the installer is left to its own
	// resolution: an existing dsh-go on PATH is overwritten in place, and only a
	// fresh machine gets ~/.local/bin.
	destDir := strings.TrimSpace(dir)
	if destDir == "" {
		destDir = strings.TrimSpace(os.Getenv(install.EnvInstallToDir))
	}
	if destDir != "" {
		absolute, err := filepath.Abs(destDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "install: resolve %s: %v\n", destDir, err)
			os.Exit(1)
		}
		destDir = absolute
	}

	if !skipFrontend {
		staged, err := frontend.Prepare(context.Background(), frontend.PrepareOptions{
			Root:   root,
			Source: source,
			Token:  token,
			Force:  forceBundle,
			Log:    os.Stdout,
		})
		if err != nil {
			if !allowMissingAssets {
				fmt.Fprintf(os.Stderr, "install: %v\n", err)
				fmt.Fprintln(os.Stderr, "install: pass --source URL to capture from a running server, --skip-frontend to reuse the staged capture, or --allow-missing-assets")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		} else if staged.Reused {
			fmt.Printf("frontend: reusing %d captured responses\n", staged.Assets)
		} else {
			fmt.Printf("frontend: captured %d responses (%s) from %s\n",
				staged.Assets, frontend.HumanSize(staged.Bytes), staged.Source)
		}
	}

	fmt.Println("==> Building")
	targetGOOS, targetGOARCH := install.TargetGOOS(), install.TargetGOARCH()
	cross := targetGOOS != runtime.GOOS || targetGOARCH != runtime.GOARCH
	output := buildOutput(root, targetGOOS, targetGOARCH, cross)
	buildOpts := build.Options{
		Root:               root,
		Version:            version,
		Output:             output,
		AllowMissingAssets: allowMissingAssets,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
	}
	if cross {
		buildOpts.GOOS = targetGOOS
		buildOpts.GOARCH = targetGOARCH
		fmt.Printf("==> Cross-compiling for %s/%s\n", targetGOOS, targetGOARCH)
	}
	result, err := build.Build(buildOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}

	installed, err := install.Install(install.Options{
		Dir:          root,
		Package:      build.MainPackage,
		BinName:      build.BinaryName,
		ModulePath:   build.ModulePath,
		InstallToDir: destDir,
		// The build already happened above, so installing copies that artifact
		// instead of compiling a second time.
		Build: func(_, _, out string) error {
			return build.CopyFile(result.Path, out)
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n%s is ready at %s\n", build.BinaryName, installed.Primary)
	for _, extra := range installed.Extras {
		fmt.Printf("also refreshed %s\n", extra)
	}
	if !onPath(installed.Primary) {
		fmt.Printf("note: %s is not on PATH; add its directory to run `%s` from any directory\n",
			installed.Primary, build.BinaryName)
	}
	fmt.Printf("run `%s version`, then `%s serve` and open http://127.0.0.1:3070/\n", build.BinaryName, build.BinaryName)
}

// buildOutput is where the intermediate binary is written.
func buildOutput(root, goos, goarch string, cross bool) string {
	name := build.BinaryName
	if cross {
		name += "-" + goos + "-" + goarch
	}
	return filepath.Join(root, "dist", name)
}

// onPath reports whether the binary's directory is already on PATH.
func onPath(binary string) bool {
	dir := filepath.Dir(binary)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		if absolute, err := filepath.Abs(entry); err == nil && absolute == dir {
			return true
		}
	}
	return false
}
