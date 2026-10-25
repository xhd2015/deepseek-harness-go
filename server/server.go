// Package server serves the web client: the multiplexed Gateway endpoint,
// the captured frontend, and (in development) a reverse proxy to a running
// DeepSeek Harness UI.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/xhd2015/deepseek-harness-go/api"
	"github.com/xhd2015/deepseek-harness-go/assets"
	"github.com/xhd2015/deepseek-harness-go/mux"
)

// Wire paths shared with the web client.
const (
	// MuxPath is the single multiplexed Remote stream endpoint.
	MuxPath = "/api/remote.mux"
	// WorkerPath is the SharedWorker bundle the client can load.
	WorkerPath = "/api/remote.shared-mux-worker.js"
	// HealthPath answers readiness probes.
	HealthPath = "/healthz"
	// TokenQuery is the auth query parameter that mints a session cookie.
	TokenQuery = "token"
	// CookieName holds the minted auth cookie.
	CookieName = "dsh_go_token"
)

// Config configures the HTTP server.
type Config struct {
	// Addr is the listen address, for example 127.0.0.1:3070.
	Addr string
	// Service answers Remote streams; it is required.
	Service *api.Service
	// Assets is the captured frontend; nil or empty serves a setup page.
	Assets *assets.Assets
	// ProxyTo forwards every non-API request to a running UI, for development.
	ProxyTo string
	// Token enables cookie authentication; empty disables it.
	Token string
	// Logf receives diagnostics; nil discards them.
	Logf func(format string, args ...any)
}

// Server owns the HTTP listener and the stream mux.
type Server struct {
	cfg     Config
	handler http.Handler
	proxy   *httputil.ReverseProxy
	http    *http.Server

	addr string
}

// New builds a server.
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("server: a stream service is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:3070"
	}
	s := &Server{cfg: cfg}
	if cfg.ProxyTo != "" {
		target, err := url.Parse(cfg.ProxyTo)
		if err != nil {
			return nil, fmt.Errorf("server: parse proxy target: %w", err)
		}
		if target.Scheme == "" || target.Host == "" {
			return nil, fmt.Errorf("server: proxy target %q needs a scheme and host", cfg.ProxyTo)
		}
		s.proxy = httputil.NewSingleHostReverseProxy(target)
		s.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			s.logf("proxy %s: %v", r.URL.Path, err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
	}
	s.handler = http.HandlerFunc(s.serve)
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// Handler exposes the HTTP handler for tests and the dev proxy backend.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the bound listener address, once serving.
func (s *Server) Addr() string { return s.addr }

// Serve listens until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", s.cfg.Addr, err)
	}
	s.addr = listener.Addr().String()
	s.logf("listening on http://%s/", s.addr)
	if s.cfg.Token != "" {
		s.logf("authentication enabled: open http://%s/?%s=<token>", s.addr, TokenQuery)
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("serve: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
	case <-stopped:
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		_ = s.http.Close()
	}
	<-stopped
	return nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// The stream endpoint accepts the token directly, so a WebSocket client
	// without a cookie jar can connect; redirecting it would break the
	// handshake instead.
	if r.URL.Path != MuxPath && s.mintCookie(w, r) {
		return
	}
	switch {
	case r.URL.Path == HealthPath:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	case r.URL.Path == MuxPath:
		s.serveStreams(w, r)
	case s.proxy != nil:
		s.proxy.ServeHTTP(w, r)
	default:
		s.serveAsset(w, r)
	}
}

// serveStreams upgrades one multiplexed Remote connection.
func (s *Server) serveStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.logf("stream connection from %s", r.RemoteAddr)
	// The mux server owns the upgraded connection for its whole lifetime.
	muxServer := &mux.Server{Opener: s.cfg.Service.Open, Logf: s.cfg.Logf}
	muxServer.ServeHTTP(w, r)
}

// mintCookie exchanges ?token= for the auth cookie and redirects to the same
// URL without the secret, so it never reaches a Referer header or history.
func (s *Server) mintCookie(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Token == "" {
		return false
	}
	provided := r.URL.Query().Get(TokenQuery)
	if provided == "" || !s.matches(provided) {
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    cookieValue(s.cfg.Token),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 60 * 60,
	})
	target := *r.URL
	query := target.Query()
	query.Del(TokenQuery)
	target.RawQuery = query.Encode()
	if target.Path == "" {
		target.Path = "/"
	}
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
	return true
}

// authorized accepts the cookie minted above or a directly supplied token, so
// non-browser clients can connect without a cookie jar.
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	if provided := r.URL.Query().Get(TokenQuery); provided != "" && s.matches(provided) {
		return true
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(cookieValue(s.cfg.Token))) == 1
}

func (s *Server) matches(provided string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.Token)) == 1
}

// cookieValue derives the opaque cookie value from the token.
func cookieValue(token string) string {
	sum := sha256.Sum256([]byte("dsh-go auth\x00" + token))
	return hex.EncodeToString(sum[:])
}

// serveAsset answers one captured response.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	body, contentType, ok := s.cfg.Assets.Lookup(r.URL.RequestURI())
	if !ok && r.URL.Path == "/index.html" {
		body, contentType, ok = s.cfg.Assets.Lookup("/")
	}
	if !ok {
		if r.URL.Path == "/" {
			s.serveSetup(w)
			return
		}
		http.NotFound(w, r)
		return
	}
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	header.Set("Cache-Control", "no-cache")
	if strings.HasSuffix(r.URL.Path, ".js") {
		header.Set("X-Content-Type-Options", "nosniff")
	}
	_, _ = w.Write(body)
}

// serveSetup explains how to stage the frontend when no capture is embedded.
func (s *Server) serveSetup(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html>
<html><head><meta charset="utf-8"><title>dsh-go needs the web assets</title>
<style>body{font:16px/1.6 -apple-system,system-ui,sans-serif;max-width:44rem;margin:4rem auto;padding:0 1.5rem;color:#1f2328}code{background:#f2f4f7;padding:.15rem .35rem;border-radius:4px}</style>
</head><body>
<h1>dsh-go is running</h1>
<p>The backend started, but no web assets are embedded in this build.</p>
<p>With a DeepSeek Harness web server running on <code>http://127.0.0.1:3080</code>, capture its frontend once:</p>
<pre><code>go run ./script/sync-assets</code></pre>
<p>Then rebuild so the capture is embedded:</p>
<pre><code>go run ./script/build</code></pre>
<p>For UI work, <code>go run ./script/dev</code> proxies the frontend from that server instead of embedding it.</p>
</body></html>
`))
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}
