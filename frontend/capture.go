// Package frontend captures and stages the DeepSeek Harness web frontend that
// dsh-go embeds.
//
// There are two halves. Capture reads a running Harness Web server and stores
// every response the page needs, byte-for-byte, under one directory. Prepare
// does the whole production path: resolve a checkout, build it if needed, serve
// it once, capture it, and stop it.
package frontend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// WorkerPath is fetched alongside the HTML-referenced assets because the client
// opens it from code rather than from markup. A capture without it serves a
// page that never connects, so Prepare checks for it.
const WorkerPath = "/api/remote.shared-mux-worker.js"

// DefaultSource is the local Harness Web server used during development.
const DefaultSource = "http://127.0.0.1:3080"

// Manifest describes one capture.
type Manifest struct {
	Source     string           `json:"source"`
	CapturedAt string           `json:"capturedAt"`
	Index      string           `json:"index"`
	Entries    map[string]Entry `json:"entries"`
}

// Entry is one captured response.
type Entry struct {
	File        string `json:"file"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// CaptureOptions controls one capture.
type CaptureOptions struct {
	// Source is the running Web server, for example http://127.0.0.1:3080.
	Source string
	// Out is the destination directory; empty is an error.
	Out string
	// Token authenticates against a server that requires one.
	Token string
	// Log receives progress; nil discards it.
	Log io.Writer
	// Timeout bounds the whole capture; zero uses 2 minutes.
	Timeout time.Duration
}

// CaptureResult describes a finished capture.
type CaptureResult struct {
	Manifest   Manifest
	Out        string
	TotalBytes int64
	// Rev and Version identify the captured frontend build.
	Rev     string
	Version string
}

// Capture stores every response the frontend needs from a running server.
func Capture(opts CaptureOptions) (CaptureResult, error) {
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		return CaptureResult{}, errors.New("frontend: capture needs a source URL")
	}
	out := strings.TrimSpace(opts.Out)
	if out == "" {
		return CaptureResult{}, errors.New("frontend: capture needs an output directory")
	}
	base, err := url.Parse(source)
	if err != nil {
		return CaptureResult{}, fmt.Errorf("frontend: parse source %q: %w", source, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return CaptureResult{}, fmt.Errorf("frontend: source %q needs a scheme and host", source)
	}
	base.Path = "/"
	base.RawQuery = ""
	absolute, err := filepath.Abs(out)
	if err != nil {
		return CaptureResult{}, fmt.Errorf("frontend: resolve %s: %w", out, err)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	client, err := newClient(base, opts.Token, timeout)
	if err != nil {
		return CaptureResult{}, err
	}
	indexBody, boot, err := fetchIndex(client, base)
	if err != nil {
		return CaptureResult{}, err
	}
	uris := collectURIs(indexBody, boot, base)
	if len(uris) == 0 {
		return CaptureResult{}, fmt.Errorf("frontend: %s referenced no assets; is it a DeepSeek Harness web server?", base)
	}
	logf(opts.Log, "capturing %d assets from %s", len(uris), base)

	// Write into a sibling directory and publish by rename, so a failed capture
	// never leaves the previous, working capture half-deleted.
	partial := absolute + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return CaptureResult{}, fmt.Errorf("frontend: clear %s: %w", partial, err)
	}
	filesDir := filepath.Join(partial, "f")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return CaptureResult{}, fmt.Errorf("frontend: create %s: %w", filesDir, err)
	}
	if err := os.WriteFile(filepath.Join(partial, "index.html"), indexBody, 0o644); err != nil {
		return CaptureResult{}, err
	}

	captured := Manifest{
		Source:     base.String(),
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Index:      "/",
		Entries:    make(map[string]Entry, len(uris)+1),
	}
	// The document is captured once, as the index, and then referenced by the
	// root URI so serving "/" never depends on a page happening to link to it.
	captured.Entries[captured.Index] = Entry{
		File:        "index.html",
		ContentType: "text/html; charset=utf-8",
		Size:        int64(len(indexBody)),
	}
	total := int64(len(indexBody))
	for i, uri := range uris {
		body, contentType, err := fetch(client, base, uri)
		if err != nil {
			return CaptureResult{}, err
		}
		name := storedName(uri, i)
		if err := os.WriteFile(filepath.Join(filesDir, name), body, 0o644); err != nil {
			return CaptureResult{}, fmt.Errorf("frontend: write %s: %w", name, err)
		}
		captured.Entries[uri] = Entry{File: "f/" + name, ContentType: contentType, Size: int64(len(body))}
		total += int64(len(body))
	}
	if _, ok := captured.Entries[WorkerPath]; !ok {
		return CaptureResult{}, fmt.Errorf("frontend: %s did not serve %s; the page would never connect", base, WorkerPath)
	}

	raw, err := json.MarshalIndent(captured, "", "  ")
	if err != nil {
		return CaptureResult{}, err
	}
	if err := os.WriteFile(filepath.Join(partial, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		return CaptureResult{}, err
	}
	// Keep the directory itself committed so //go:embed always has a match.
	if err := os.WriteFile(filepath.Join(partial, ".gitkeep"), nil, 0o644); err != nil {
		return CaptureResult{}, err
	}
	if err := os.RemoveAll(absolute); err != nil {
		return CaptureResult{}, fmt.Errorf("frontend: replace %s: %w", absolute, err)
	}
	if err := os.Rename(partial, absolute); err != nil {
		_ = os.RemoveAll(partial)
		return CaptureResult{}, fmt.Errorf("frontend: publish %s: %w", absolute, err)
	}
	return CaptureResult{
		Manifest:   captured,
		Out:        absolute,
		TotalBytes: total,
		Rev:        boot.Rev,
		Version:    boot.Release.Version,
	}, nil
}

// bootInfo is the subset of __DSH_BOOT__ that names the frontend build and the
// plugin bundles it imports.
type bootInfo struct {
	Rev     string `json:"rev"`
	Release struct {
		Version string `json:"version"`
	} `json:"release"`
	// Entries and Batches list the plugin bundles the boot loader imports, so
	// they are fetched from the boot payload rather than from markup.
	Entries []bootURL `json:"entries"`
	Batches []bootURL `json:"batches"`
}

type bootURL struct {
	URL string `json:"url"`
}

func newClient(base *url.URL, token string, timeout time.Duration) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("frontend: create cookie jar: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: timeout}
	if token == "" {
		return client, nil
	}
	mint, err := base.Parse("/?token=" + url.QueryEscape(token))
	if err != nil {
		return nil, fmt.Errorf("frontend: build token URL: %w", err)
	}
	resp, err := client.Get(mint.String())
	if err != nil {
		return nil, fmt.Errorf("frontend: authenticate against %s: %w", base, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("frontend: authenticate against %s: %s", base, resp.Status)
	}
	return client, nil
}

func fetchIndex(client *http.Client, base *url.URL) ([]byte, bootInfo, error) {
	var boot bootInfo
	target, err := base.Parse("/")
	if err != nil {
		return nil, boot, fmt.Errorf("frontend: build index URL: %w", err)
	}
	resp, err := client.Get(target.String())
	if err != nil {
		return nil, boot, fmt.Errorf("frontend: fetch %s: %w (is the web server running?)", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, boot, fmt.Errorf("frontend: read %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, boot, fmt.Errorf("frontend: fetch %s: %s (pass a token when the server requires auth)", target, resp.Status)
	}
	if !strings.Contains(string(body), "__DSH_BOOT__") {
		return nil, boot, fmt.Errorf("frontend: %s does not look like the DeepSeek Harness web UI", target)
	}
	if raw, ok := extractBoot(body); ok {
		_ = json.Unmarshal(raw, &boot)
	}
	return body, boot, nil
}

// extractBoot returns the JSON object assigned to __DSH_BOOT__.
func extractBoot(body []byte) ([]byte, bool) {
	const marker = `globalThis["__DSH_BOOT__"]`
	at := strings.Index(string(body), marker)
	if at < 0 {
		return nil, false
	}
	start := strings.IndexByte(string(body[at:]), '{')
	if start < 0 {
		return nil, false
	}
	start += at
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(body); i++ {
		c := body[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return body[start : i+1], true
			}
		}
	}
	return nil, false
}

var attrPattern = regexp.MustCompile(`(?is)(?:src|href)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// collectURIs lists the request URIs the frontend needs, sorted for a stable
// manifest.
func collectURIs(indexBody []byte, boot bootInfo, base *url.URL) []string {
	seen := make(map[string]bool)
	var uris []string
	add := func(uri string) {
		if uri == "" || seen[uri] {
			return
		}
		seen[uri] = true
		uris = append(uris, uri)
	}
	for _, match := range attrPattern.FindAllSubmatch(indexBody, -1) {
		raw := string(match[1])
		if raw == "" {
			raw = string(match[2])
		}
		uri, ok := requestURI(html.UnescapeString(raw), base)
		if ok {
			add(uri)
		}
	}
	for _, group := range [][]bootURL{boot.Entries, boot.Batches} {
		for _, item := range group {
			if uri, ok := requestURI(item.URL, base); ok {
				add(uri)
			}
		}
	}
	add(WorkerPath)
	sort.Strings(uris)
	return uris
}

// requestURI resolves a markup reference to the request URI a browser would
// send to dsh-go.
func requestURI(raw string, base *url.URL) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", false
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch ref.Scheme {
	case "data", "blob", "mailto", "javascript", "about":
		return "", false
	}
	resolved := base.ResolveReference(ref)
	if resolved.Host != base.Host {
		return "", false
	}
	uri := resolved.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	if resolved.RawQuery != "" {
		uri += "?" + resolved.RawQuery
	}
	return uri, true
}

func fetch(client *http.Client, base *url.URL, uri string) ([]byte, string, error) {
	target, err := base.Parse(uri)
	if err != nil {
		return nil, "", fmt.Errorf("frontend: build URL for %s: %w", uri, err)
	}
	resp, err := client.Get(target.String())
	if err != nil {
		return nil, "", fmt.Errorf("frontend: fetch %s: %w", uri, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("frontend: read %s: %w", uri, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("frontend: fetch %s: %s", uri, resp.Status)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(target.Path))
	}
	return body, contentType, nil
}

func storedName(uri string, index int) string {
	sum := sha256.Sum256([]byte(uri))
	name := hex.EncodeToString(sum[:8])
	if ext := filepath.Ext(pathOf(uri)); ext != "" && len(ext) <= 8 && isPlain(ext) {
		name += ext
	}
	return fmt.Sprintf("%03d-%s", index, name)
}

func pathOf(uri string) string {
	if at := strings.IndexByte(uri, '?'); at >= 0 {
		uri = uri[:at]
	}
	return uri
}

// isPlain reports whether an extension can be appended to a file name as is.
func isPlain(ext string) bool {
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
		default:
			return false
		}
	}
	return true
}

// Complete reports whether dir holds a capture with the worker route, which is
// the cheapest proof that the page can connect.
func Complete(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return false
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false
	}
	if len(manifest.Entries) == 0 {
		return false
	}
	_, ok := manifest.Entries[WorkerPath]
	return ok
}

// Count reports how many responses dir holds and whether it is complete.
func Count(dir string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0, false
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, false
	}
	_, hasWorker := manifest.Entries[WorkerPath]
	return len(manifest.Entries), len(manifest.Entries) > 0 && hasWorker
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}
