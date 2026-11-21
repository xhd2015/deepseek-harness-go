package dshsource

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checkout layout this package relies on. The frontend capture needs a Web
// server, and the checkout's own CLI is the only thing that serves the real
// page with its boot payload.
const (
	cliEntry      = "apps/cli/lib/bin.js"
	webDistIndex  = "apps/web/dist/index.html"
	webPackage    = "apps/web/package.json"
	cliLibDir     = "apps/cli/lib"
	listenFile    = "web-listen.json"
	startupBudget = 3 * time.Minute
)

// ServeOptions controls one throwaway Web server.
type ServeOptions struct {
	// Root is the checkout.
	Root string
	// Node is the node binary; empty uses PATH.
	Node string
	// TempDir is the scratch home; empty uses a fresh temp directory that is
	// removed on stop.
	TempDir string
	// Log receives the server's stderr; nil discards it.
	Log io.Writer
	// Startup is how long to wait for the listen record.
	Startup time.Duration
}

// Server is one running DeepSeek Harness Web process.
type Server struct {
	// Origin is the base URL, for example http://127.0.0.1:51234.
	Origin string

	cancel context.CancelFunc
	done   chan struct{}
	home   string
	clean  bool
}

// Serve starts the checkout's Web UI on an ephemeral port with a throwaway
// Harness home, so capturing the page never touches the operator's state and
// never needs a server that was already running.
func Serve(ctx context.Context, opts ServeOptions) (*Server, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		return nil, errors.New("dsh source: serve needs a checkout")
	}
	entry := filepath.Join(root, cliEntry)
	if !fileExists(entry) {
		return nil, fmt.Errorf("dsh source: %s is not built (%s is missing); build the checkout first", root, cliEntry)
	}
	if !fileExists(filepath.Join(root, webDistIndex)) {
		return nil, fmt.Errorf("dsh source: %s is not built (%s is missing); build the frontend first", root, webDistIndex)
	}
	node := opts.Node
	if node == "" {
		node = "node"
	}
	resolved, err := exec.LookPath(node)
	if err != nil {
		return nil, fmt.Errorf("dsh source: node is required to serve the frontend: %w", err)
	}
	home := opts.TempDir
	clean := false
	if home == "" {
		dir, err := os.MkdirTemp("", "dsh-go-frontend-")
		if err != nil {
			return nil, fmt.Errorf("dsh source: create scratch home: %w", err)
		}
		home = dir
		clean = true
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, resolved, entry, "web", "--no-open", "--no-auth", "--port", "0")
	cmd.Dir = root
	cmd.Env = append(scrubbedEnv(os.Environ()), "DSH_HOME="+home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		if clean {
			_ = os.RemoveAll(home)
		}
		return nil, fmt.Errorf("dsh source: serve stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		if clean {
			_ = os.RemoveAll(home)
		}
		return nil, fmt.Errorf("dsh source: serve stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		if clean {
			_ = os.RemoveAll(home)
		}
		return nil, fmt.Errorf("dsh source: start %s: %w", resolved, err)
	}

	server := &Server{cancel: cancel, done: make(chan struct{}), home: home, clean: clean}
	// Drain both pipes for the process's lifetime: a full stderr buffer would
	// stall the server, and the same reader surfaces why it failed to start.
	go func() {
		defer close(server.done)
		copyLines(stdout, opts.Log, nil)
		_ = cmd.Wait()
	}()
	go copyLines(stderr, opts.Log, nil)

	startup := opts.Startup
	if startup <= 0 {
		startup = startupBudget
	}
	record, err := awaitListen(ctx, home, startup, server.done)
	if err != nil {
		_ = server.Stop()
		return nil, err
	}
	if record.Origin == "" {
		_ = server.Stop()
		return nil, errors.New("dsh source: the checkout's Web server published no origin")
	}
	server.Origin = record.Origin
	return server, nil
}

// Stop terminates the server and removes a scratch home it created.
func (s *Server) Stop() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		return errors.New("dsh source: the Web server did not exit")
	}
	if s.clean && s.home != "" {
		if err := os.RemoveAll(s.home); err != nil {
			return fmt.Errorf("dsh source: remove scratch home: %w", err)
		}
		s.clean = false
	}
	return nil
}

// listenRecord is the subset of the published discovery file this package
// needs.
type listenRecord struct {
	Origin string `json:"origin"`
}

// awaitListen polls the scratch home for the record the server publishes.
func awaitListen(ctx context.Context, home string, budget time.Duration, done <-chan struct{}) (listenRecord, error) {
	deadline := time.Now().Add(budget)
	path := filepath.Join(home, listenFile)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			var record listenRecord
			if err := json.Unmarshal(raw, &record); err == nil && record.Origin != "" {
				return record, nil
			}
		}
		select {
		case <-ctx.Done():
			return listenRecord{}, ctx.Err()
		case <-done:
			return listenRecord{}, errors.New("dsh source: the checkout's Web server exited before publishing its listen record")
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return listenRecord{}, fmt.Errorf("dsh source: the checkout's Web server published no listen record under %s within %s", home, budget)
		}
	}
}

// scrubbedEnv drops ambient DSH_* variables so the child uses the scratch home
// and none of this process's runtime facts.
func scrubbedEnv(base []string) []string {
	env := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "DSH_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

// copyLines forwards a child's output line by line, optionally matching the
// startup line. A nil writer discards.
func copyLines(reader io.Reader, sink io.Writer, onLine func(string) bool) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if sink != nil {
			fmt.Fprintln(sink, line)
		}
		if onLine != nil && onLine(line) {
			return
		}
	}
}

// Ready reports whether a checkout can serve its frontend today, so a caller
// can say what is missing before spending minutes on a build.
func Ready(root string) (cli bool, dist bool) {
	return fileExists(filepath.Join(root, cliEntry)), fileExists(filepath.Join(root, webDistIndex))
}

// HasFrontendPackage reports whether the checkout still has the Web app this
// package expects to build.
func HasFrontendPackage(root string) bool {
	return fileExists(filepath.Join(root, webPackage))
}

// CLILibDir is the build output directory reported in "not built" errors.
func CLILibDir(root string) string { return filepath.Join(root, cliLibDir) }
