package frontend

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testIndex = `<!doctype html><html><head>
<link rel="stylesheet" href="/assets/app.css">
<script type="module" src="/assets/app.js"></script>
<script>globalThis["__DSH_BOOT__"] = {"rev":"abc123","release":{"version":"9.9.9"},
"entries":[{"url":"/plugins/a.js"}],"batches":[{"url":"/batches/b.js"}]};</script>
</head><body></body></html>`

// fakeHarness serves the page every capture expects.
func fakeHarness(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"/":               testIndex,
		"/assets/app.css": "body{}",
		"/assets/app.js":  "console.log('app')",
		"/plugins/a.js":   "console.log('plugin')",
		"/batches/b.js":   "console.log('batch')",
		// WorkerPath is a constant, so the map needs exactly one entry for it.
		WorkerPath: "self.onmessage=()=>{}",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch filepath.Ext(r.URL.Path) {
		case ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCaptureStoresEveryResponseThePageNeeds(t *testing.T) {
	server := fakeHarness(t)
	out := filepath.Join(t.TempDir(), "dist")
	result, err := Capture(CaptureOptions{Source: server.URL, Out: out})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if result.Rev != "abc123" || result.Version != "9.9.9" {
		t.Fatalf("rev/version = %q/%q", result.Rev, result.Version)
	}
	// Markup references, boot entries and batches, and the worker, which no
	// markup names.
	for _, uri := range []string{"/", "/assets/app.css", "/assets/app.js", "/plugins/a.js", "/batches/b.js", WorkerPath} {
		entry, ok := result.Manifest.Entries[uri]
		if !ok {
			t.Fatalf("missing %s in %v", uri, keys(result.Manifest.Entries))
		}
		if strings.HasPrefix(uri, "/assets/app.js") && !strings.HasPrefix(entry.ContentType, "text/javascript") {
			t.Fatalf("%s content type = %q", uri, entry.ContentType)
		}
	}
	// The directory is complete and self-describing.
	if !Complete(out) {
		t.Fatal("capture is not complete")
	}
	if count, _ := Count(out); count != len(result.Manifest.Entries) {
		t.Fatalf("Count = %d, want %d", count, len(result.Manifest.Entries))
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(raw), `"source"`) {
		t.Fatalf("manifest = %s", raw)
	}
	if _, err := os.Stat(filepath.Join(out, ".gitkeep")); err != nil {
		t.Fatalf(".gitkeep: %v", err)
	}
}

func TestCaptureRefusesAPageWithoutTheWorker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == WorkerPath {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(testIndex))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := Capture(CaptureOptions{Source: server.URL, Out: out}); err == nil {
		t.Fatal("a capture without the worker route was accepted")
	}
}

func TestCaptureRefusesANonHarnessPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not the harness</html>"))
	}))
	t.Cleanup(server.Close)
	if _, err := Capture(CaptureOptions{Source: server.URL, Out: t.TempDir()}); err == nil {
		t.Fatal("a page without __DSH_BOOT__ was accepted")
	}
}

func TestCaptureKeepsThePreviousCaptureOnFailure(t *testing.T) {
	server := fakeHarness(t)
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := Capture(CaptureOptions{Source: server.URL, Out: out}); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	// A source that fails halfway must not destroy what is already staged.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(testIndex))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	if _, err := Capture(CaptureOptions{Source: broken.URL, Out: out}); err == nil {
		t.Fatal("a failing capture reported success")
	}
	if !Complete(out) {
		t.Fatal("the previous capture was destroyed")
	}
}

func TestCompleteRequiresTheWorkerEntry(t *testing.T) {
	dir := t.TempDir()
	if Complete(dir) {
		t.Fatal("an empty directory is complete")
	}
	body := `{"entries":{"/":{}}}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if Complete(dir) {
		t.Fatal("a capture without the worker is complete")
	}
	body = `{"entries":{"/":{},"` + WorkerPath + `":{}}}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	if !Complete(dir) {
		t.Fatal("a full capture is not complete")
	}
}

func keys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
