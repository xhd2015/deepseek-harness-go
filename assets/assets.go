// Package assets serves the captured DeepSeek Harness web frontend.
//
// The frontend is a prebuilt React bundle: dsh-go never rebuilds it. It is
// captured byte-for-byte from a running DeepSeek Harness web server by
// `go run ./script/sync-assets` and embedded at build time, so one static
// capture serves every future build without a Node toolchain.
package assets

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Entry is one captured response.
type Entry struct {
	File        string `json:"file"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// Manifest indexes the capture by request URI.
type Manifest struct {
	Source     string           `json:"source"`
	CapturedAt string           `json:"capturedAt"`
	Index      string           `json:"index"`
	Entries    map[string]Entry `json:"entries"`
}

// Assets is the immutable frontend capture.
type Assets struct {
	fsys     fs.FS
	manifest Manifest
}

// Load reads the capture from one filesystem. An empty dir uses the copy
// embedded in the binary.
func Load(dir string) (*Assets, error) {
	fsys, err := captureFS(dir)
	if err != nil {
		return nil, err
	}
	assets := &Assets{fsys: fsys}
	raw, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return assets, nil
		}
		return nil, fmt.Errorf("assets: read manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &assets.manifest); err != nil {
		return nil, fmt.Errorf("assets: decode manifest: %w", err)
	}
	for uri, entry := range assets.manifest.Entries {
		if entry.File == "" || path.IsAbs(entry.File) || strings.Contains(entry.File, "..") {
			return nil, fmt.Errorf("assets: entry %q has an unsafe file path %q", uri, entry.File)
		}
	}
	return assets, nil
}

func captureFS(dir string) (fs.FS, error) {
	if dir != "" {
		absolute, err := absPath(dir)
		if err != nil {
			return nil, err
		}
		return os.DirFS(absolute), nil
	}
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, fmt.Errorf("assets: open embedded capture: %w", err)
	}
	return sub, nil
}

func absPath(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("assets: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("assets: %s is not a directory", dir)
	}
	return dir, nil
}

// Available reports whether a capture has been staged.
func (a *Assets) Available() bool {
	if a == nil {
		return false
	}
	return len(a.manifest.Entries) > 0 && a.manifest.Entries[a.indexURI()].File != ""
}

// Manifest returns the capture index.
func (a *Assets) Manifest() Manifest {
	if a == nil {
		return Manifest{}
	}
	return a.manifest
}

func (a *Assets) indexURI() string {
	if a.manifest.Index == "" {
		return "/"
	}
	return a.manifest.Index
}

// Lookup resolves one request URI to its captured body. The capture is keyed
// by the exact request URI because plugin bundles are selected by query
// parameters; a percent-decoded and, finally, a bare-path lookup keep
// hand-typed URLs working.
func (a *Assets) Lookup(uri string) ([]byte, string, bool) {
	if a == nil || len(a.manifest.Entries) == 0 {
		return nil, "", false
	}
	candidates := []string{uri}
	if decoded, err := url.QueryUnescape(uri); err == nil && decoded != uri {
		candidates = append(candidates, decoded)
	}
	if parsed, err := url.ParseRequestURI(uri); err == nil && parsed.Path != uri {
		candidates = append(candidates, parsed.Path)
	}
	if uri == "" {
		candidates = append(candidates, a.indexURI())
	}
	for _, key := range candidates {
		entry, ok := a.manifest.Entries[key]
		if !ok {
			continue
		}
		body, err := fs.ReadFile(a.fsys, entry.File)
		if err != nil {
			return nil, "", false
		}
		return body, entry.ContentType, true
	}
	return nil, "", false
}
