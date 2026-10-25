package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/xhd2015/deepseek-harness-go/api"
	"github.com/xhd2015/deepseek-harness-go/assets"
	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/session"
)

const testIndex = `<!doctype html><html><body>
<script>globalThis["__DSH_BOOT__"] = {"rev":"test"};</script>
</body></html>`

const testPlugin = `window.__ModuleLoader__.load({id:"plugin", factory(){}});`

func testCapture(t *testing.T) *assets.Assets {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"manifest.json": mustJSON(t, assets.Manifest{
			Source:     "test",
			CapturedAt: "2025-01-01T00:00:00Z",
			Index:      "/",
			Entries: map[string]assets.Entry{
				"/": {File: "index.html", ContentType: "text/html; charset=utf-8", Size: int64(len(testIndex))},
				"/plugins/??@scope/plugin/client.js&rev=abc": {File: "plugin.js", ContentType: "text/javascript; charset=utf-8", Size: int64(len(testPlugin))},
			},
		}),
		"index.html": []byte(testIndex),
		"plugin.js":  []byte(testPlugin),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	captured, err := assets.Load(dir)
	if err != nil {
		t.Fatalf("assets.Load: %v", err)
	}
	if !captured.Available() {
		t.Fatal("test capture is not available")
	}
	return captured
}

func testService(t *testing.T) *api.Service {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return api.New(api.Config{Store: store, HomeDir: root})
}

func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Service == nil {
		cfg.Service = testService(t)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthAndCapturedFrontend(t *testing.T) {
	capture := testCapture(t)
	ts := newTestServer(t, Config{Assets: capture})

	health := get(t, ts.URL+HealthPath)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", health.StatusCode)
	}
	if body := readBody(t, health); !strings.Contains(body, "ok") {
		t.Fatalf("health body = %q", body)
	}

	index := get(t, ts.URL+"/")
	if index.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", index.StatusCode)
	}
	if got := index.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	if got := index.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache control = %q", got)
	}
	if body := readBody(t, index); body != testIndex {
		t.Fatalf("index body = %q", body)
	}

	// /index.html is answered from the capture entry for "/".
	explicit := get(t, ts.URL+"/index.html")
	if body := readBody(t, explicit); explicit.StatusCode != http.StatusOK || body != testIndex {
		t.Fatalf("/index.html = %d %q", explicit.StatusCode, body)
	}

	// Anything the capture does not know about is a 404, never the setup page.
	missing := get(t, ts.URL+"/missing.js")
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset status = %d", missing.StatusCode)
	}
}

func TestAssetLookupKeepsPluginQueryURIs(t *testing.T) {
	ts := newTestServer(t, Config{Assets: testCapture(t)})
	uri := "/plugins/??@scope/plugin/client.js&rev=abc"
	resp := get(t, ts.URL+uri)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	if body := readBody(t, resp); body != testPlugin {
		t.Fatalf("body = %q", body)
	}
}

func TestSetupPageWithoutCapture(t *testing.T) {
	empty, err := assets.Load(t.TempDir())
	if err != nil {
		t.Fatalf("assets.Load: %v", err)
	}
	if empty.Available() {
		t.Fatal("empty capture reports itself available")
	}
	ts := newTestServer(t, Config{Assets: empty})
	resp := get(t, ts.URL+"/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "sync-assets") {
		t.Fatalf("body = %q", body)
	}

	// Without a capture every non-root path is a plain 404.
	if resp := get(t, ts.URL+"/plugin.js"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestTokenAuthGuardsTheMux(t *testing.T) {
	ts := newTestServer(t, Config{Assets: testCapture(t), Token: "s3cret"})

	if resp := get(t, ts.URL+"/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("frontend must stay readable: %d", resp.StatusCode)
	}

	// A wrong token neither mints a cookie nor gets a redirect.
	wrong := get(t, ts.URL+"/?token=nope")
	if wrong.StatusCode != http.StatusOK || len(wrong.Cookies()) != 0 {
		t.Fatalf("wrong token: %d cookies=%v", wrong.StatusCode, wrong.Cookies())
	}

	minted := get(t, ts.URL+"/?token=s3cret")
	if minted.StatusCode != http.StatusFound {
		t.Fatalf("mint status = %d", minted.StatusCode)
	}
	if location := minted.Header.Get("Location"); location != "/" {
		t.Fatalf("location = %q", location)
	}
	cookies := minted.Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName || cookies[0].Value == "" {
		t.Fatalf("cookies = %v", cookies)
	}
	cookie := cookies[0]

	// The socket is the guarded surface; an unauthorized dial never upgrades.
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL+"/api/remote.mux"), nil); err == nil {
		t.Fatal("unauthorized dial succeeded")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized dial = %v (resp %v)", err, resp)
	}
	token, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL+"/api/remote.mux?token=s3cret"), nil)
	if err != nil {
		t.Fatalf("token dial: %v (%v)", err, resp)
	}
	token.Close()

	authorized, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL+"/api/remote.mux"), http.Header{
		"Cookie": []string{cookie.String()},
	})
	if err != nil {
		t.Fatalf("cookie dial: %v (%v)", err, resp)
	}
	defer authorized.Close()

	if err := authorized.WriteMessage(websocket.TextMessage, mustJSON(t, map[string]any{
		"type":     "open",
		"streamId": "1",
		"endpoint": api.EndpointInvoke,
		"payload": map[string]any{
			"args": map[string]any{
				"endpoint": "session/list",
				"payload":  map[string]any{"args": map[string]any{"_request": map[string]any{}}},
			},
		},
	})); err != nil {
		t.Fatalf("write open: %v", err)
	}
	item := readFrame(t, authorized)
	if item.Type != mux.FrameItem || item.StreamID != "1" {
		t.Fatalf("frame = %+v", item)
	}
	var value struct {
		OK    bool `json:"ok"`
		Value struct {
			Items []any `json:"items"`
		} `json:"value"`
	}
	decode(t, item.Value, &value)
	if !value.OK || value.Value.Items == nil {
		t.Fatalf("value = %s", item.Value)
	}
	if end := readFrame(t, authorized); end.Type != mux.FrameEnd {
		t.Fatalf("frame = %+v", end)
	}
}

func TestProxyForwardsFrontendAndKeepsAPILocal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte("upstream:" + r.URL.RequestURI()))
	}))
	t.Cleanup(upstream.Close)

	ts := newTestServer(t, Config{ProxyTo: upstream.URL, Token: "s3cret"})
	resp := get(t, ts.URL+"/session/abc?x=1")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("proxy response = %d %q", resp.StatusCode, resp.Header.Get("X-Upstream"))
	}
	if body := readBody(t, resp); body != "upstream:/session/abc?x=1" {
		t.Fatalf("proxy body = %q", body)
	}

	// /healthz and the mux stay local even while proxying.
	if resp := get(t, ts.URL+HealthPath); resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upstream") != "" {
		t.Fatalf("/healthz was proxied: %d", resp.StatusCode)
	}
	plain := get(t, ts.URL+"/api/remote.mux")
	if plain.Header.Get("X-Upstream") != "" {
		t.Fatal("/api/remote.mux was proxied")
	}
	if plain.StatusCode != http.StatusUnauthorized {
		t.Fatalf("plain mux status = %d", plain.StatusCode)
	}
}

type frame struct {
	Type     string          `json:"type"`
	StreamID string          `json:"streamId"`
	Value    json.RawMessage `json:"value"`
	Error    *mux.Failure    `json:"error"`
}

func readFrame(t *testing.T, conn *websocket.Conn) frame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var decoded frame
	decode(t, data, &decoded)
	return decoded
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func decode(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}
