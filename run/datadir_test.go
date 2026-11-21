package run

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/server"
)

// TestDataDirIsDistinctFromHarnessHome proves the process never writes into the
// DeepSeek Harness home unless the operator asks for it by name.
func TestDataDirIsDistinctFromHarnessHome(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("DSH_HOME", filepath.Join(userHome, ".dsh"))
	t.Setenv(EnvHome, "")
	t.Setenv(EnvSessionsRoot, "")

	opts := Options{UserHome: userHome}
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	want := filepath.Join(userHome, ".dsh-go")
	if opts.DataDir != want {
		t.Fatalf("data dir = %q, want %q", opts.DataDir, want)
	}
	if opts.SessionsRoot != filepath.Join(want, "sessions") {
		t.Fatalf("sessions root = %q", opts.SessionsRoot)
	}
	if strings.Contains(opts.SessionsRoot, string(filepath.Separator)+".dsh"+string(filepath.Separator)) {
		t.Fatalf("sessions root %q points into the Harness home", opts.SessionsRoot)
	}

	// The model catalog still reads the existing Harness profile rather than
	// silently dropping it.
	if opts.Models != "" {
		t.Fatalf("explicit models path = %q", opts.Models)
	}
}

// TestDataDirPrecedence pins flag over environment over default.
func TestDataDirPrecedence(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv(EnvHome, filepath.Join(userHome, "from-env"))
	t.Setenv(EnvSessionsRoot, "")

	fromEnv := Options{UserHome: userHome}
	if err := fromEnv.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if fromEnv.DataDir != filepath.Join(userHome, "from-env") {
		t.Fatalf("env data dir = %q", fromEnv.DataDir)
	}

	explicit := Options{UserHome: userHome, DataDir: filepath.Join(userHome, "from-flag")}
	if err := explicit.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if explicit.DataDir != filepath.Join(userHome, "from-flag") {
		t.Fatalf("flag data dir = %q", explicit.DataDir)
	}
	if explicit.SessionsRoot != filepath.Join(userHome, "from-flag", "sessions") {
		t.Fatalf("derived sessions root = %q", explicit.SessionsRoot)
	}

	// An explicit sessions root overrides only that one directory.
	overridden := Options{
		UserHome:     userHome,
		DataDir:      filepath.Join(userHome, "data"),
		SessionsRoot: filepath.Join(userHome, "elsewhere"),
	}
	if err := overridden.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if overridden.SessionsRoot != filepath.Join(userHome, "elsewhere") {
		t.Fatalf("sessions root = %q", overridden.SessionsRoot)
	}
}

// TestSessionCompressionFlag proves only the two documented encodings are
// accepted, and that "auto" means detect.
func TestSessionCompressionFlag(t *testing.T) {
	userHome := t.TempDir()
	for _, value := range []string{"", "auto", "zstd", "none"} {
		opts := Options{UserHome: userHome, DataDir: filepath.Join(userHome, "d"), Compression: value}
		if err := opts.applyDefaults(); err != nil {
			t.Fatalf("applyDefaults(%q): %v", value, err)
		}
	}
	opts := Options{UserHome: userHome, DataDir: filepath.Join(userHome, "d"), Compression: "gzip"}
	if err := opts.applyDefaults(); err == nil {
		t.Fatal("an unknown compression was accepted")
	}
}

// TestNewServerCreatesDataDirLayout proves one start creates the whole tree
// under the data directory and nothing under the Harness home.
func TestNewServerCreatesDataDirLayout(t *testing.T) {
	userHome := t.TempDir()
	harnessHome := filepath.Join(userHome, ".dsh")
	if err := os.MkdirAll(filepath.Join(harnessHome, "profiles", "web"), 0o700); err != nil {
		t.Fatalf("seed harness home: %v", err)
	}
	patch := filepath.Join(harnessHome, "profiles", "web", "cordis.patch.yml")
	seed := "- id: agent-default-model\n  config:\n    provider: patched\n    model: patched-model\n"
	if err := os.WriteFile(patch, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed patch: %v", err)
	}
	t.Setenv("DSH_HOME", harnessHome)
	t.Setenv(EnvHome, "")
	t.Setenv(EnvSessionsRoot, "")
	t.Setenv(EnvModels, "")

	dataDir := filepath.Join(userHome, "dsh-go-data")
	opts := Options{
		UserHome: userHome,
		DataDir:  dataDir,
		Port:     0,
	}
	srv, cleanup, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer cleanup()
	if srv == nil {
		t.Fatal("no server")
	}
	for _, want := range []string{"sessions", "storages"} {
		if info, err := os.Stat(filepath.Join(dataDir, want)); err != nil || !info.IsDir() {
			t.Fatalf("%s under the data dir: %v", want, err)
		}
	}
	// Nothing may be written into the Harness home.
	entries, err := os.ReadDir(harnessHome)
	if err != nil {
		t.Fatalf("read harness home: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "profiles" {
			t.Fatalf("the start wrote %q into the Harness home", entry.Name())
		}
	}
}

// TestPublishListenToMirrorsRecord proves the opt-in mirror writes a second
// discovery record through the real server path and removes it on shutdown.
func TestPublishListenToMirrorsRecord(t *testing.T) {
	t.Setenv(EnvHome, "")
	t.Setenv(EnvSessionsRoot, "")
	t.Setenv(EnvModels, "")

	userHome := t.TempDir()
	shared := filepath.Join(userHome, ".dsh")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	dataDir := filepath.Join(userHome, "dsh-go")

	srv, cleanup, err := NewServer(Options{
		UserHome:        userHome,
		DataDir:         dataDir,
		PublishListenTo: shared,
		Port:            freePort(t),
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
	t.Cleanup(func() {
		cancel()
		<-done
	})

	paths := []string{
		filepath.Join(dataDir, "web-listen.json"),
		filepath.Join(shared, "web-listen.json"),
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		present := 0
		for _, path := range paths {
			if _, err := os.Stat(path); err == nil {
				present++
			}
		}
		if present == len(paths) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listen records missing: %v", paths)
		}
		time.Sleep(20 * time.Millisecond)
	}
	record, err := server.ReadListenRecord(shared)
	if err != nil {
		t.Fatalf("read mirrored record: %v", err)
	}
	if record == nil || record.Origin == "" {
		t.Fatalf("mirrored record = %+v", record)
	}

	// Shutting down removes both records so a stale address is never offered.
	cancel()
	<-done
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived shutdown", path)
		}
	}
}

// freePort reserves an ephemeral port and releases it for the server to bind.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
