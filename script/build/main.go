// Command build compiles dsh-go with the DeepSeek Harness frontend embedded and
// writes it to dist/dsh-go.
//
// The frontend comes first: this command resolves a Harness checkout, builds it
// if necessary, serves it once, captures every response into assets/dist, and
// only then runs `go build`, so the embedded bytes are always the ones just
// produced.
//
// Usage:
//
//	go run ./script/build [options]
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/build"
	"github.com/xhd2015/deepseek-harness-go/frontend"
)

const help = `
Usage: go run ./script/build [options]

Resolve the DeepSeek Harness frontend, capture it into assets/dist, then build
dsh-go with it embedded into dist/dsh-go.

Frontend source lookup, first match wins:
  1. DSH_ROOT                     local checkout (must exist)
  2. .deepseek-harness.json local path, relative to the file
  3. .deepseek-harness.json repo + branch   cloned into ~/.cache/dsh-go
  4. official deepseek-ai/deepseek-harness main

A linked worktree without its own config uses the main checkout's copy. The
chosen source is printed before the build.

Options:
  -o, --output PATH        write the binary to PATH
      --version VERSION    version stamped into the binary (default: git describe)
      --source URL         capture from an already running Harness server
                           instead of building a checkout
      --token TOKEN        auth token for --source
      --force-frontend     rebuild and recapture even when a capture exists
      --skip-frontend      reuse the existing capture without touching it
      --allow-missing-assets
                           build without a captured frontend
  -h, --help               show this help
`

func main() {
	var (
		output             string
		version            string
		source             string
		token              string
		forceFrontend      bool
		skipFrontend       bool
		allowMissingAssets bool
	)
	_, err := lessflags.String("-o,--output", &output).
		String("--version", &version).
		String("--source", &source).
		String("--token", &token).
		Bool("--force-frontend", &forceFrontend).
		Bool("--skip-frontend", &skipFrontend).
		Bool("--allow-missing-assets", &allowMissingAssets).
		Help("-h,--help", help).
		Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, lessflags.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}

	root, err := build.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}

	if !skipFrontend {
		staged, err := frontend.Prepare(context.Background(), frontend.PrepareOptions{
			Root:   root,
			Source: source,
			Token:  token,
			Force:  forceFrontend,
			Log:    os.Stdout,
		})
		if err != nil {
			if !allowMissingAssets {
				fmt.Fprintf(os.Stderr, "build: %v\n", err)
				fmt.Fprintln(os.Stderr, "build: pass --source URL to capture from a running server, or --allow-missing-assets for a backend-only build")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		} else if staged.Reused {
			fmt.Printf("frontend: reusing %s (%d assets)\n", staged.Out, staged.Assets)
		} else {
			if staged.Rev != "" {
				fmt.Printf("frontend rev %s, version %s\n", staged.Rev, staged.Version)
			}
			fmt.Printf("frontend: captured %d assets (%s) from %s\n",
				staged.Assets, frontend.HumanSize(staged.Bytes), staged.Source)
		}
	}

	result, err := build.Build(build.Options{
		Root:               root,
		Output:             output,
		Version:            version,
		AllowMissingAssets: allowMissingAssets,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("built %s (%s, %s)\n", result.Path, frontend.HumanSize(result.Size), result.Version)
	if result.Assets {
		fmt.Printf("embedded %d frontend responses\n", result.Responses)
	} else {
		fmt.Println("warning: built without a captured frontend")
		fmt.Printf("note: with no frontend, serve needs --proxy-to; open http://127.0.0.1:8300/ after `%s serve`\n", build.BinaryName)
	}
}
