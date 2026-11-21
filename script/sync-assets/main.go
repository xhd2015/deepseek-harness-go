// Command sync-assets captures the DeepSeek Harness web frontend from a running
// server into assets/dist, so the Go implementation can serve the real UI
// byte-for-byte without rebuilding it.
//
// `go run ./script/build` does this by itself, including building a checkout
// and serving it once. This command is the manual form, for capturing from a
// server that is already running.
//
// Usage:
//
//	go run ./script/sync-assets [--source URL] [--out DIR] [--token TOKEN]
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/build"
	"github.com/xhd2015/deepseek-harness-go/frontend"
)

const help = `
Usage: go run ./script/sync-assets [options]

Capture the frontend of a running DeepSeek Harness web server into assets/dist
so dsh-go can embed and serve it.

Options:
      --source URL   server to capture from (default http://127.0.0.1:3080)
      --out DIR      capture directory (default assets/dist)
      --token TOKEN  auth token, when the source requires one
  -h, --help         show this help

` + "`go run ./script/build`" + ` already does this from a checkout, so this
command is only needed to capture from a server you are already running.
`

func main() {
	var source, out, token string
	remain, err := lessflags.String("--source", &source).
		String("--out", &out).
		String("--token", &token).
		Help("-h,--help", help).
		Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, lessflags.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "sync-assets: %v\n", err)
		os.Exit(1)
	}
	if len(remain) > 0 {
		fmt.Fprintf(os.Stderr, "sync-assets: unexpected arguments %q\n", remain)
		os.Exit(2)
	}
	root, err := build.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync-assets: %v\n", err)
		os.Exit(1)
	}
	if source == "" {
		source = frontend.DefaultSource
	}
	if out == "" {
		out = filepath.Join(root, "assets", "dist")
	}

	result, err := frontend.Capture(frontend.CaptureOptions{
		Source: source,
		Out:    out,
		Token:  token,
		Log:    os.Stdout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync-assets: %v\n", err)
		os.Exit(1)
	}
	if result.Rev != "" {
		fmt.Printf("frontend rev %s, version %s\n", result.Rev, result.Version)
	}
	fmt.Printf("captured %d assets (%s) into %s\n", len(result.Manifest.Entries), frontend.HumanSize(result.TotalBytes), result.Out)
	fmt.Println("next: go run ./script/build")
}
