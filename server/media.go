package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// FilePath is the authenticated Host media route. It is not a Remote call: the
// page addresses it with a plain GET, and the version in the query makes the
// bytes immutable for that URL.
const FilePath = "/api/file"

// DefaultMaxFileBytes bounds one media response. It matches the Harness
// attachment image limit (50 MiB), so a picture the page could attach is a
// picture the page can also show.
const DefaultMaxFileBytes = 50 << 20

// Media route error codes. They are the same vocabulary the Harness filesystem
// provider reports, because the page maps a 403 body to a sentence and must not
// have to learn a second spelling.
const (
	fileCodeNotFound      = "FS_NOT_FOUND"
	fileCodeNotRegular    = "FS_NOT_REGULAR_FILE"
	fileCodePermission    = "FS_PERMISSION_DENIED"
	fileCodeTooLarge      = "FS_TOO_LARGE"
	fileCodeBadPath       = "FS_BAD_PATH"
	fileCodeAborted       = "FS_ABORTED"
	fileCodeInternalError = "FS_INTERNAL"
)

var (
	fileNoStoreHeaders = http.Header{
		"X-Content-Type-Options": {"nosniff"},
		// An HTML or SVG file may be opened directly on the API origin, so its
		// own scripts and fetches are fenced off.
		"Content-Security-Policy": {"sandbox; default-src 'none'"},
		"Cache-Control":           {"private, no-store"},
	}
	// A versioned URL names one file version, so its response never changes.
	fileImmutableHeaders = http.Header{
		"X-Content-Type-Options":  {"nosniff"},
		"Content-Security-Policy": {"sandbox; default-src 'none'"},
		"Cache-Control":           {"private, max-age=31536000, immutable"},
	}
)

// serveFile answers GET and HEAD /api/file.
//
// The page rewrites a local image or file path to this route, so the backend —
// not the hosting page — is what reads the bytes. The read reaches any absolute
// path the serving process can open: the Harness sandbox confines writes, not
// reads, and a session's screenshots routinely live outside its checkout.
//
// A request without the current `v` is redirected, uncached, to the versioned
// URL; that response is cacheable forever. The redirect is relative on purpose,
// so a deployment prefix or an outer proxy's path rewrite survives it.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		// The route is authenticated exactly like the mux: a rejected credential
		// is a transport-level 401, never a body the page would read as a file.
		fileFail(w, r, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	rawPath := r.URL.Query().Get("path")
	if rawPath == "" {
		fileFail(w, r, http.StatusBadRequest, "missing path", fileCodeBadPath)
		return
	}
	if strings.ContainsRune(rawPath, 0) || !filepath.IsAbs(rawPath) {
		fileFail(w, r, http.StatusBadRequest, "absolute path required", fileCodeBadPath)
		return
	}
	target := filepath.Clean(rawPath)
	info, err := os.Stat(target)
	if err != nil {
		fileFail(w, r, fileErrorStatus(err), fileErrorMessage(err), fileErrorCode(err))
		return
	}
	if !info.Mode().IsRegular() {
		fileFail(w, r, http.StatusForbidden, "not a regular file", fileCodeNotRegular)
		return
	}
	maxBytes := s.cfg.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	if info.Size() > maxBytes {
		fileFail(w, r, http.StatusRequestEntityTooLarge, "file exceeds byte limit", fileCodeTooLarge)
		return
	}
	version := fileVersionOf(info)
	if r.URL.Query().Get("v") != version {
		location := "?" + url.Values{"path": {rawPath}, "v": {version}}.Encode()
		header := w.Header()
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("Location", location)
		for name, values := range fileNoStoreHeaders {
			header[name] = values
		}
		w.WriteHeader(http.StatusFound)
		return
	}
	contentType := fileContentType(target)
	// Open before declaring a length. `stat` succeeds on a file the process may
	// not read, so a Content-Length taken from it would describe bytes that are
	// never written: the response says "403" while its own header still promises
	// the whole file, and the client reads a truncated body instead of the code.
	file, err := os.Open(target)
	if err != nil {
		fileFail(w, r, fileErrorStatus(err), fileErrorMessage(err), fileErrorCode(err))
		return
	}
	defer file.Close()
	// The handle, not the path, is what gets served: a file replaced between the
	// stat and this open is answered as the file that was actually opened.
	opened, err := file.Stat()
	if err != nil {
		fileFail(w, r, fileErrorStatus(err), fileErrorMessage(err), fileErrorCode(err))
		return
	}
	if !opened.Mode().IsRegular() {
		fileFail(w, r, http.StatusForbidden, "not a regular file", fileCodeNotRegular)
		return
	}
	if opened.Size() > maxBytes {
		fileFail(w, r, http.StatusRequestEntityTooLarge, "file exceeds byte limit", fileCodeTooLarge)
		return
	}
	header := w.Header()
	for name, values := range fileImmutableHeaders {
		header[name] = values
	}
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.FormatInt(opened.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		// A HEAD reaches the same verdict as the GET, including "not readable":
		// a client probing before it fetches must not be told a denied file is
		// there to take.
		return
	}
	// Exactly the length just declared: a file that grows mid-copy cannot push
	// the body past its own Content-Length.
	if _, err := io.CopyN(w, file, opened.Size()); err != nil && !errors.Is(err, io.EOF) {
		// The header is already out; there is no status left to correct, and the
		// client sees a short body rather than a wrong one.
		s.logf("serve file %s: %v", target, err)
	}
}

// fileVersionOf derives the opaque freshness token for one file.
//
// The Harness page treats this token as opaque; it compares two of them and
// never parses one. The fields match the Harness spelling so a value copied
// between the two hosts still names the same revision.
func fileVersionOf(info os.FileInfo) string {
	identity := fileIdentity(info)
	return fmt.Sprintf("%s:%d:%d", identity, info.Size(), info.ModTime().UnixNano())
}

// fileContentType names the media type of one file.
//
// The extension decides: a screenshot is served as image/png because that is
// what it is, and a file with no known extension falls back to the generic
// binary type rather than to a type guessed from the first bytes, which would
// mislabel a JavaScript or HTML file as text and let it load as a document.
func fileContentType(path string) string {
	if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

// fileFail answers one media request with a code the page can classify.
func fileFail(w http.ResponseWriter, r *http.Request, status int, message, code string) {
	header := w.Header()
	// This answer is not the file: any header a partially built success left
	// behind would describe bytes that are not coming. A Content-Length in
	// particular makes Go discard the body for being "too short", so the code the
	// page reads would never arrive.
	header.Del("Content-Length")
	header.Del("Content-Type")
	for name, values := range fileNoStoreHeaders {
		header[name] = values
	}
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("X-DSH-File-Error", code)
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	// The body is the code alone: the page reads it to name the cause, and a
	// longer sentence would describe the server's own bookkeeping.
	_, _ = io.WriteString(w, code)
}

// fileErrorStatus maps a read failure onto the status the page classifies.
func fileErrorStatus(err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// fileErrorCode maps a read failure onto the Harness filesystem vocabulary.
func fileErrorCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fileCodeNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return fileCodePermission
	default:
		return fileCodeInternalError
	}
}

// fileErrorMessage keeps the shape the Harness route answers with.
func fileErrorMessage(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return "permission denied"
	default:
		return "cannot read file"
	}
}
