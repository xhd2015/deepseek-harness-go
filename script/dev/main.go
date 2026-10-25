// Command dev runs the development server: the Go backend on port 3070 with
// every non-API request proxied to a running DeepSeek Harness web UI, so the
// Go implementation can be developed against the real frontend.
//
// Usage:
//
//	go run ./script/dev
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	devserver "github.com/xhd2015/dot-pkgs/go-pkgs/dev/server"

	"github.com/xhd2015/deepseek-harness-go/build"
	"github.com/xhd2015/deepseek-harness-go/run"
)

// EnvFrontend overrides the frontend upstream.
const EnvFrontend = "DSH_GO_DEV_FRONTEND"

func main() {
	root, err := build.FindRoot()
	if err != nil {
		fatal(err)
	}
	frontend := strings.TrimSpace(os.Getenv(EnvFrontend))
	if frontend == "" {
		frontend = "http://127.0.0.1:3080"
	}

	err = devserver.Main(os.Args[1:], devserver.Config{
		Name:         "dsh-go",
		Root:         root,
		BuildPackage: "./script/dev",
		BackendPort:  run.DefaultPort,
		Frontend:     nil,
		WatchDirs: []string{
			filepath.Join(root, "cmd"),
			filepath.Join(root, "run"),
			filepath.Join(root, "server"),
			filepath.Join(root, "mux"),
			filepath.Join(root, "api"),
			filepath.Join(root, "agent"),
			filepath.Join(root, "llm"),
			filepath.Join(root, "modelconfig"),
			filepath.Join(root, "session"),
			filepath.Join(root, "assets"),
			filepath.Join(root, "version"),
			filepath.Join(root, "script", "dev"),
		},
		BackendHandler: func(ctx context.Context, backend devserver.Backend) (http.Handler, error) {
			handler, cleanup, err := run.NewHandler(run.Options{
				Port:    backend.Port,
				ProxyTo: frontend,
			})
			if err != nil {
				return nil, err
			}
			go func() {
				<-ctx.Done()
				cleanup()
			}()
			return handler, nil
		},
		OnListen: func(backend devserver.Backend) (func(), error) {
			fmt.Printf("dsh-go dev: %s (frontend proxied from %s)\n", backend.URL, frontend)
			return nil, nil
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "dsh-go dev: %v\n", err)
	os.Exit(1)
}
