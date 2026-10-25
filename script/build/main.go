// Command build compiles dsh-go with the captured frontend embedded and writes
// it to dist/dsh-go.
//
// Usage:
//
//	go run ./script/build [--version VERSION] [--output PATH] [--allow-missing-assets]
package main

import (
	"errors"
	"fmt"
	"os"

	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/build"
)

const help = `
Usage: go run ./script/build [options]

Build dsh-go into dist/dsh-go with the frontend captured in assets/dist
embedded.

Options:
  -o, --output PATH        write the binary to PATH
      --version VERSION    version stamped into the binary (default: git describe)
      --allow-missing-assets
                           build without a captured frontend
  -h, --help               show this help

Run ` + "`go run ./script/sync-assets`" + ` first to capture the frontend from a
running DeepSeek Harness web server.
`

func main() {
	var (
		output             string
		version            string
		allowMissingAssets bool
	)
	_, err := lessflags.String("-o,--output", &output).
		String("--version", &version).
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

	result, err := build.Build(build.Options{
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
	fmt.Printf("built %s (%s, %s)\n", result.Path, humanSize(result.Size), result.Version)
	if result.Assets {
		fmt.Printf("embedded %d frontend responses\n", result.Responses)
	} else {
		fmt.Println("warning: built without a captured frontend")
	}
}

func humanSize(size int64) string {
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
