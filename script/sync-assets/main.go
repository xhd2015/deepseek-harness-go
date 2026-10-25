// Command sync-assets captures the DeepSeek Harness web frontend from a running
// server into assets/dist, so the Go implementation can serve the real UI
// byte-for-byte without rebuilding it.
//
// Usage:
//
//	go run ./script/sync-assets [--source URL] [--out DIR] [--token TOKEN]
package main

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

	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/build"
)

// DefaultSource is the local DSH web server used during development.
const DefaultSource = "http://127.0.0.1:3080"

// WorkerPath is fetched alongside the HTML-referenced assets because the client
// opens it from code rather than from markup.
const WorkerPath = "/api/remote.shared-mux-worker.js"

const help = `
Usage: go run ./script/sync-assets [options]

Capture the frontend of a running DeepSeek Harness web server into assets/dist
so dsh-go can embed and serve it.

Options:
      --source URL   server to capture from (default http://127.0.0.1:3080)
      --out DIR      capture directory (default assets/dist)
      --token TOKEN  auth token, when the source requires one
  -h, --help         show this help
`

type manifest struct {
	Source     string           `json:"source"`
	CapturedAt string           `json:"capturedAt"`
	Index      string           `json:"index"`
	Entries    map[string]entry `json:"entries"`
}

type entry struct {
	File        string `json:"file"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

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

func main() {
	var source, out, token string
	remain, err := lessflags.String("--source", &source).
		String("--out", &out).
		String("--token", &token).
		Help("-h,--help", help).
		Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, lessflags.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "sync-assets: %v\n", err)
		os.Exit(1)
	}
	if len(remain) > 0 {
		fmt.Fprintf(os.Stderr, "sync-assets: unexpected arguments %q\n", remain)
		os.Exit(2)
	}
	root, err := build.FindRoot()
	if err != nil {
		fail(err)
	}
	if source == "" {
		source = DefaultSource
	}
	base, err := url.Parse(source)
	if err != nil {
		fail(fmt.Errorf("parse --source %q: %w", source, err))
	}
	if base.Scheme == "" || base.Host == "" {
		fail(fmt.Errorf("--source %q needs a scheme and host", source))
	}
	base.Path = "/"
	base.RawQuery = ""
	if out == "" {
		out = filepath.Join(root, "assets", "dist")
	}
	out, err = filepath.Abs(out)
	if err != nil {
		fail(fmt.Errorf("resolve --out: %w", err))
	}

	client, err := newClient(base, token)
	if err != nil {
		fail(err)
	}
	indexBody, boot, err := fetchIndex(client, base)
	if err != nil {
		fail(err)
	}

	uris := collectURIs(indexBody, boot, base)
	if len(uris) == 0 {
		fail(fmt.Errorf("%s/index.html referenced no assets; is %s a DeepSeek Harness web server?", base, base))
	}
	fmt.Printf("capturing %d assets from %s\n", len(uris), base)
	if boot.Rev != "" {
		fmt.Printf("frontend rev %s, version %s\n", boot.Rev, boot.Release.Version)
	}

	if err := os.RemoveAll(out); err != nil {
		fail(fmt.Errorf("clear %s: %w", out, err))
	}
	filesDir := filepath.Join(out, "f")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		fail(fmt.Errorf("create %s: %w", out, err))
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), indexBody, 0o644); err != nil {
		fail(err)
	}

	captured := manifest{
		Source:     base.String(),
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Index:      "/",
		Entries:    make(map[string]entry, len(uris)),
	}
	var total int64
	for i, uri := range uris {
		body, contentType, err := fetch(client, base, uri)
		if err != nil {
			fail(err)
		}
		name := storedName(uri, i)
		if err := os.WriteFile(filepath.Join(filesDir, name), body, 0o644); err != nil {
			fail(fmt.Errorf("write %s: %w", name, err))
		}
		captured.Entries[uri] = entry{File: "f/" + name, ContentType: contentType, Size: int64(len(body))}
		total += int64(len(body))
	}

	raw, err := json.MarshalIndent(captured, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		fail(err)
	}
	// Keep the directory itself committed so //go:embed always has a match.
	if err := os.WriteFile(filepath.Join(out, ".gitkeep"), nil, 0o644); err != nil {
		fail(err)
	}

	fmt.Printf("captured %d assets (%s) into %s\n", len(captured.Entries), humanSize(total), out)
	fmt.Println("next: go run ./script/build")
}

func newClient(base *url.URL, token string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: 120 * time.Second}
	if token == "" {
		return client, nil
	}
	mint, err := base.Parse("/?token=" + url.QueryEscape(token))
	if err != nil {
		return nil, fmt.Errorf("build token URL: %w", err)
	}
	resp, err := client.Get(mint.String())
	if err != nil {
		return nil, fmt.Errorf("authenticate against %s: %w", base, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("authenticate against %s: %s", base, resp.Status)
	}
	return client, nil
}

func fetchIndex(client *http.Client, base *url.URL) ([]byte, bootInfo, error) {
	var boot bootInfo
	target, err := base.Parse("/")
	if err != nil {
		return nil, boot, fmt.Errorf("build index URL: %w", err)
	}
	resp, err := client.Get(target.String())
	if err != nil {
		return nil, boot, fmt.Errorf("fetch %s: %w (is the web server running?)", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, boot, fmt.Errorf("read %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, boot, fmt.Errorf("fetch %s: %s (pass --token when the server requires auth)", target, resp.Status)
	}
	if !strings.Contains(string(body), "__DSH_BOOT__") {
		return nil, boot, fmt.Errorf("%s does not look like the DeepSeek Harness web UI", target)
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
		fmt.Fprintf(os.Stderr, "sync-assets: skipping foreign URL %s\n", raw)
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
		return nil, "", fmt.Errorf("build URL for %s: %w", uri, err)
	}
	resp, err := client.Get(target.String())
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", uri, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", uri, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch %s: %s", uri, resp.Status)
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

func fail(err error) {
	fmt.Fprintf(os.Stderr, "sync-assets: %v\n", err)
	os.Exit(1)
}
