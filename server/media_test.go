package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// noRedirectClient answers with the redirect itself, so a test can assert on the
// version a request names instead of on the bytes behind it.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// mediaFile writes one file into a fresh temp dir and returns its path.
func mediaFile(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// mediaURL builds the versioned route URL for one path.
func mediaURL(base, path, version string) string {
	query := url.Values{"path": {path}}
	if version != "" {
		query.Set("v", version)
	}
	return base + FilePath + "?" + query.Encode()
}

// versionOfFileAnswer follows the redirect one unversioned request answers and
// returns the version it names.
func versionOfFileAnswer(t *testing.T, tsURL, path string) string {
	t.Helper()
	resp := get(t, mediaURL(tsURL, path, ""))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unversioned status = %d, body = %q", resp.StatusCode, readBody(t, resp))
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse location %q: %v", resp.Header.Get("Location"), err)
	}
	version := location.Query().Get("v")
	if version == "" {
		t.Fatalf("location %q has no v", resp.Header.Get("Location"))
	}
	return version
}

// TestFileRouteServesTheBytesItIsAskedFor is the whole point of the route: the
// page rewrites a screenshot path to /api/file, and this backend — not the
// hosting page — has to read it.
func TestFileRouteServesTheBytesItIsAskedFor(t *testing.T) {
	path := mediaFile(t, "result.png", "png-bytes")
	ts := newTestServer(t, Config{})

	version := versionOfFileAnswer(t, ts.URL, path)
	resp := get(t, mediaURL(ts.URL, path, version))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("content type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("cache control = %q", got)
	}
	if body := readBody(t, resp); body != "png-bytes" {
		t.Fatalf("body = %q", body)
	}
}

// TestFileRouteHeadCarriesTheSizeButNoBody keeps a probe cheap: the page asks
// HEAD to learn the status without downloading the picture twice.
func TestFileRouteHeadCarriesTheSizeButNoBody(t *testing.T) {
	path := mediaFile(t, "shot.png", "0123456789")
	ts := newTestServer(t, Config{})
	version := versionOfFileAnswer(t, ts.URL, path)

	req, err := http.NewRequest(http.MethodHead, mediaURL(ts.URL, path, version), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != "10" {
		t.Fatalf("content length = %q", got)
	}
	if body := readBody(t, resp); body != "" {
		t.Fatalf("HEAD body = %q", body)
	}
}

// TestFileRouteNamesWhyItCannotRead keeps the page able to say "deleted"
// instead of "unavailable": the body is the code it classifies on.
func TestFileRouteNamesWhyItCannotRead(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, Config{})

	missing := get(t, mediaURL(ts.URL, filepath.Join(dir, "gone.png"), ""))
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d", missing.StatusCode)
	}
	if body := readBody(t, missing); body != fileCodeNotFound {
		t.Fatalf("missing body = %q", body)
	}

	notFile := get(t, mediaURL(ts.URL, dir, ""))
	if notFile.StatusCode != http.StatusForbidden {
		t.Fatalf("directory status = %d", notFile.StatusCode)
	}
	if body := readBody(t, notFile); body != fileCodeNotRegular {
		t.Fatalf("directory body = %q", body)
	}
}

// TestFileRouteSaysWhenTheFileIsUnreadable is the case a stat can see but a read
// cannot: `chmod 000` lets the version be computed and the redirect be issued,
// then refuses the bytes. The answer must still be the code — a Content-Length
// left over from the size stat makes Go drop the body as "too short", and the
// page then reads an empty 403 it cannot classify.
func TestFileRouteSaysWhenTheFileIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0o000 file, so the denial cannot be provoked")
	}
	path := mediaFile(t, "locked.png", "private")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	ts := newTestServer(t, Config{})
	// The version is derived from stat alone, so the redirect still happens.
	version := versionOfFileAnswer(t, ts.URL, path)

	resp := get(t, mediaURL(ts.URL, path, version))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Go fills Content-Length from the buffered body, so the only correct value
	// here is the code's own length. Anything else is a leftover from the size
	// stat, and Go then drops the body for being shorter than it is promised.
	if got := resp.Header.Get("Content-Length"); got != "" && got != strconv.Itoa(len(fileCodePermission)) {
		t.Fatalf("failure declared length %q for a %d-byte body", got, len(fileCodePermission))
	}
	if body := readBody(t, resp); body != fileCodePermission {
		t.Fatalf("body = %q, want %q", body, fileCodePermission)
	}
	if got := resp.Header.Get("X-DSH-File-Error"); got != fileCodePermission {
		t.Fatalf("error header = %q", got)
	}

	// HEAD reaches the same verdict: a probe must not call a denied file ready.
	req, err := http.NewRequest(http.MethodHead, mediaURL(ts.URL, path, version), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	head, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer head.Body.Close()
	if head.StatusCode != http.StatusForbidden {
		t.Fatalf("HEAD status = %d", head.StatusCode)
	}
	if got := head.Header.Get("Content-Length"); got != "" && got != strconv.Itoa(len(fileCodePermission)) {
		t.Fatalf("HEAD failure declared length %q", got)
	}
}

// TestFileRouteFailureCarriesOnlyItsCode keeps every failure shape identical:
// the body is the code, no length is promised, and no content type claims the
// answer is the file.
func TestFileRouteFailureCarriesOnlyItsCode(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, Config{MaxFileBytes: 4})
	big := mediaFile(t, "big.png", "0123456789")

	for _, tc := range []struct {
		path   string
		status int
		code   string
	}{
		{filepath.Join(dir, "missing.png"), http.StatusNotFound, fileCodeNotFound},
		{dir, http.StatusForbidden, fileCodeNotRegular},
		{big, http.StatusRequestEntityTooLarge, fileCodeTooLarge},
	} {
		resp := get(t, mediaURL(ts.URL, tc.path, ""))
		if resp.StatusCode != tc.status {
			t.Fatalf("%s status = %d", tc.path, resp.StatusCode)
		}
		if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(tc.code)); got != "" && got != want {
			t.Fatalf("%s declared length %q, want %q", tc.path, got, want)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("%s content type = %q", tc.path, got)
		}
		if body := readBody(t, resp); body != tc.code {
			t.Fatalf("%s body = %q, want %q", tc.path, body, tc.code)
		}
	}
}

// TestFileRouteRejectsWhatIsNotAnAbsolutePath never lets the route be used to
// read a page-relative path as if it were a file.
func TestFileRouteRejectsWhatIsNotAnAbsolutePath(t *testing.T) {
	ts := newTestServer(t, Config{})
	for _, raw := range []string{"", "relative.png", "../up.png", "/tmp/zero\x00.png"} {
		query := url.Values{}
		if raw != "" {
			query.Set("path", raw)
		}
		resp := get(t, ts.URL+FilePath+"?"+query.Encode())
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("path %q status = %d", raw, resp.StatusCode)
		}
		if body := readBody(t, resp); body != fileCodeBadPath {
			t.Fatalf("path %q body = %q", raw, body)
		}
	}
}

// TestFileRouteRefusesAFilePastTheLimit answers 413 rather than streaming a file
// the page cannot render.
func TestFileRouteRefusesAFilePastTheLimit(t *testing.T) {
	path := mediaFile(t, "huge.png", strings.Repeat("x", 64))
	ts := newTestServer(t, Config{MaxFileBytes: 8})
	resp := get(t, mediaURL(ts.URL, path, ""))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, readBody(t, resp))
	}
	if body := readBody(t, resp); body != fileCodeTooLarge {
		t.Fatalf("body = %q", body)
	}
}

// TestFileRouteVersionChangesWhenTheBytesDo keeps a rewritten file from being
// served as the version the page already cached.
func TestFileRouteVersionChangesWhenTheBytesDo(t *testing.T) {
	path := mediaFile(t, "live.png", "first")
	ts := newTestServer(t, Config{})
	before := versionOfFileAnswer(t, ts.URL, path)

	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after := versionOfFileAnswer(t, ts.URL, path)
	if before == after {
		t.Fatalf("version did not change: %q", before)
	}

	stale := get(t, mediaURL(ts.URL, path, before))
	if stale.StatusCode != http.StatusFound {
		t.Fatalf("stale version status = %d", stale.StatusCode)
	}
	fresh := get(t, mediaURL(ts.URL, path, after))
	if body := readBody(t, fresh); fresh.StatusCode != http.StatusOK || body != "second" {
		t.Fatalf("fresh = %d %q", fresh.StatusCode, body)
	}
}

// TestFileRouteIsAuthenticatedLikeTheMux means a token deployment cannot be
// used as an unauthenticated file reader.
func TestFileRouteIsAuthenticatedLikeTheMux(t *testing.T) {
	path := mediaFile(t, "secret.png", "secret")
	ts := newTestServer(t, Config{Token: "s3cret"})

	resp := get(t, mediaURL(ts.URL, path, ""))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, mediaURL(ts.URL, path, ""), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	// The shared test client never follows a redirect, so the answer here is the
	// unversioned redirect rather than the bytes behind it.
	authorized, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("authorized GET: %v", err)
	}
	defer authorized.Body.Close()
	if authorized.StatusCode != http.StatusFound {
		t.Fatalf("authorized status = %d", authorized.StatusCode)
	}
}

// TestUnroutedAPIRouteIsNamed is the honest half: a GET route this backend does
// not implement must not read as a missing deployment or a deleted file.
func TestUnroutedAPIRouteIsNamed(t *testing.T) {
	ts := newTestServer(t, Config{Frontend: testCapture(t)})

	// A capture still wins for the routes it holds.
	if resp := get(t, ts.URL+"/plugins/??@scope/plugin/client.js&rev=abc"); resp.StatusCode != http.StatusOK {
		t.Fatalf("captured asset status = %d", resp.StatusCode)
	}

	resp := get(t, ts.URL+"/api/present.host")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, UnroutedAPICode) || !strings.Contains(body, "/api/present.host") {
		t.Fatalf("body = %q", body)
	}
}
