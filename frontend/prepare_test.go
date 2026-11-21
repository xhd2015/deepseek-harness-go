package frontend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/dshsource"
)

// TestPrepareReusesACompleteCapture proves the production path costs nothing
// when the staged capture is already usable, and still reports which checkout a
// fresh capture would come from.
func TestPrepareReusesACompleteCapture(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "dsh")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	out := filepath.Join(root, "assets", "dist")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}
	body := `{"entries":{"/":{},"` + WorkerPath + `":{}}}`
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	var log strings.Builder
	result, err := Prepare(context.Background(), PrepareOptions{
		Root: root,
		Log:  &log,
		DSH: dshsource.Options{
			Root:     root,
			Getenv:   func(string) string { return checkout },
			UserHome: func() (string, error) { return t.TempDir(), nil },
		},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !result.Reused || result.Out != out {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(log.String(), checkout) {
		t.Fatalf("log = %q, want it to name the checkout a fresh capture would use", log.String())
	}
}

// TestPrepareFailsWithoutACompleteCaptureAndNoCheckout proves a backend-only
// build is opt-in, not a silent fallback.
func TestPrepareFailsWithoutACompleteCaptureAndNoCheckout(t *testing.T) {
	root := t.TempDir()
	_, err := Prepare(context.Background(), PrepareOptions{
		Root: root,
		DSH: dshsource.Options{
			Root:     root,
			Getenv:   func(string) string { return filepath.Join(root, "missing") },
			UserHome: func() (string, error) { return root, nil },
		},
	})
	if err == nil {
		t.Fatal("Prepare succeeded with neither a capture nor a checkout")
	}
	if !errors.Is(err, dshsource.ErrCheckoutMissing) {
		t.Fatalf("err = %v, want ErrCheckoutMissing", err)
	}
}

// TestPrepareSourceOverrideSkipsTheCheckout proves --source is the development
// escape hatch.
func TestPrepareSourceOverrideSkipsTheCheckout(t *testing.T) {
	server := fakeHarness(t)
	root := t.TempDir()
	result, err := Prepare(context.Background(), PrepareOptions{
		Root:   root,
		Source: server.URL,
		DSH: dshsource.Options{
			Root:     root,
			Getenv:   func(string) string { return filepath.Join(root, "missing") },
			UserHome: func() (string, error) { return root, nil },
		},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if result.DSH.Root != "" {
		t.Fatalf("source override resolved a checkout: %+v", result.DSH)
	}
	if result.Assets == 0 || !Complete(result.Out) {
		t.Fatalf("result = %+v", result)
	}
}

func TestRequiresBundle(t *testing.T) {
	root := t.TempDir()
	if !RequiresBundle(root) {
		t.Fatal("an empty module needs a bundle")
	}
	out := filepath.Join(root, "assets", "dist")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"entries":{"/":{},"` + WorkerPath + `":{}}}`
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if RequiresBundle(root) {
		t.Fatal("a complete capture still requires a bundle")
	}
}
