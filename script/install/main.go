// Command install builds dsh-go and installs it into ~/.local/bin.
//
// Usage:
//
//	go run ./script/install [--dir DIR] [--version VERSION]
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	install "github.com/xhd2015/dot-pkgs/go-pkgs/gotool/localbin/install"
	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/build"
)

const help = `
Usage: go run ./script/install [options]

Build dsh-go and install the binary into ~/.local/bin.

INSTALL_TO_DIR overrides the install directory, and INSTALL_GOOS/INSTALL_GOARCH
cross-compile the binary (used by remote-agent install).

Options:
      --dir DIR         install into DIR instead of ~/.local/bin
      --version VERSION version stamped into the binary (default: git describe)
  -h, --help            show this help
`

func main() {
	var dir, version string
	remain, err := lessflags.String("--dir", &dir).
		String("--version", &version).
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
	destDir := strings.TrimSpace(dir)
	if destDir == "" {
		destDir = strings.TrimSpace(os.Getenv(install.EnvInstallToDir))
	}
	if destDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "install: locate home directory: %v\n", err)
			os.Exit(1)
		}
		destDir = filepath.Join(home, ".local", "bin")
	}
	absolute, err := filepath.Abs(destDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: resolve %s: %v\n", destDir, err)
		os.Exit(1)
	}
	destDir = absolute

	fmt.Println("==> Building")
	targetGOOS, targetGOARCH := install.TargetGOOS(), install.TargetGOARCH()
	cross := targetGOOS != runtime.GOOS || targetGOARCH != runtime.GOARCH
	output := filepath.Join(root, "dist", build.BinaryName)
	buildOpts := build.Options{
		Root:    root,
		Version: version,
		Output:  output,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}
	if cross {
		output = filepath.Join(root, "dist", build.BinaryName+"-"+targetGOOS+"-"+targetGOARCH)
		buildOpts.Output = output
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
		// The build already happened above, so installing is a copy of the
		// artifact, never a second compile.
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
	if !onPath(destDir) {
		fmt.Printf("note: add %s to PATH to run `%s` from any directory\n", destDir, build.BinaryName)
	}
	fmt.Printf("run `%s version`, then `%s serve` and open http://127.0.0.1:3070/\n", build.BinaryName, build.BinaryName)
}

func onPath(dir string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		absolute, err := filepath.Abs(entry)
		if err != nil {
			continue
		}
		if absolute == dir {
			return true
		}
	}
	return false
}
