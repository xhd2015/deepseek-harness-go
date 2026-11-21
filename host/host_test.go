package host

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDefaultPortIsTheDocumentedOne pins the one number three surfaces share:
// the flag help, the server fallback, and this package's constant.
func TestDefaultPortIsTheDocumentedOne(t *testing.T) {
	if DefaultPort != 8300 {
		t.Fatalf("DefaultPort = %d, want 8300", DefaultPort)
	}
	if DefaultAddr != DefaultHost+":8300" {
		t.Fatalf("DefaultAddr = %q", DefaultAddr)
	}
	var opts Options
	if err := opts.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if opts.Port != 8300 || opts.Host != "127.0.0.1" {
		t.Fatalf("defaults = %s:%d", opts.Host, opts.Port)
	}
}

// TestMountServesTheAPIWithoutAFrontend proves a host gets a working backend
// and no page: this is the shape eluc mounts.
func TestMountServesTheAPIWithoutAFrontend(t *testing.T) {
	dir := t.TempDir()
	handler, cleanup, err := Mount(Options{
		UserHome: dir,
		DataDir:  filepath.Join(dir, "data"),
		Port:     0,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer cleanup()

	// The page is absent, and says so instead of pretending to be the UI.
	page := serve(t, handler, http.MethodGet, "/", "")
	if page.status != http.StatusServiceUnavailable {
		t.Fatalf("GET / = %d, want the setup page", page.status)
	}
	if !strings.Contains(page.body, "needs the web assets") {
		t.Fatalf("setup page = %q", page.body)
	}

	// The remote surface is complete.
	body := `{"type":"client-request","rpcId":"r1","method":"session/list","payload":{"args":{"_request":{}}}}`
	list := serve(t, handler, http.MethodPost, "/api/session/list", body)
	if list.status != http.StatusOK {
		t.Fatalf("POST /api/session/list = %d", list.status)
	}
	if !strings.Contains(list.body, `"ok":true`) {
		t.Fatalf("list body = %s", list.body)
	}
	// The SharedWorker the client loads by hand is not an asset here either.
	worker := serve(t, handler, http.MethodGet, "/api/remote.shared-mux-worker.js", "")
	if worker.status == http.StatusOK {
		t.Fatalf("a host with no frontend served a worker bundle")
	}
}

// TestApplyDefaultsKeepsTheDataDirSeparate repeats the one rule a host must not
// break: sessions never land in the Harness home unless asked for by name.
func TestApplyDefaultsKeepsTheDataDirSeparate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DSH_HOME", filepath.Join(home, ".dsh"))
	t.Setenv(EnvHome, "")
	opts := Options{UserHome: home}
	if err := opts.ApplyDefaults(); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	if opts.DataDir != filepath.Join(home, ".dsh-go") {
		t.Fatalf("data dir = %q", opts.DataDir)
	}
	if strings.Contains(opts.SessionsRoot, string(filepath.Separator)+".dsh"+string(filepath.Separator)) {
		t.Fatalf("sessions root = %q", opts.SessionsRoot)
	}
}

// TestListenRecordIsPublishedForAHost proves a mounted backend still writes
// discovery, so an external client can find it.
func TestListenRecordIsPublishedForAHost(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	srv, cleanup, err := NewServer(Options{
		UserHome: dir,
		DataDir:  dataDir,
		Addr:     "127.0.0.1:0",
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx)
	}()
	waitFor(t, filepath.Join(dataDir, "web-listen.json"))
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(dataDir, "web-listen.json")); !os.IsNotExist(err) {
		t.Fatal("the listen record survived shutdown")
	}
}

// waitFor blocks until path exists, so a test never races the listener.
func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

type response struct {
	status int
	body   string
}

// serve drives the handler directly, so no port is bound per assertion.
func serve(t *testing.T, handler http.Handler, method, path, body string) response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return response{status: recorder.Code, body: recorder.Body.String()}
}
