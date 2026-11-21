package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/assets"
)

// newTestServerHandler builds a server and returns its HTTP handler.
func newTestServerHandler(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	if cfg.Service == nil {
		cfg.Service = testService(t)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

// rpcCall posts one client-request envelope to the bridge.
func rpcCall(t *testing.T, handler http.Handler, method string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, APIPrefix+method, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) ServerResponse {
	t.Helper()
	var response ServerResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return response
}

// TestRPCServesUnaryCall proves the HTTP bridge answers the same unary call the
// browser makes over the WebSocket.
func TestRPCServesUnaryCall(t *testing.T) {
	srv := newTestServerHandler(t, Config{})
	body := `{"type":"client-request","rpcId":"rpc-1","method":"session/list","payload":{"args":{"_request":{}}}}`
	recorder := rpcCall(t, srv, "session/list", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeResponse(t, recorder)
	if response.Type != "server-response" || response.RPCID != "rpc-1" {
		t.Fatalf("envelope = %+v", response)
	}
	if response.Result == nil || !response.Result.OK {
		t.Fatalf("result = %+v", response.Result)
	}
	if !strings.Contains(string(response.Result.Value), `"items"`) {
		t.Fatalf("value = %s", response.Result.Value)
	}
}

// TestRPCRejectsMalformedEnvelopes pins every refusal to the gateway's own
// contract: the same code and shape an in-page client would see.
func TestRPCRejectsMalformedEnvelopes(t *testing.T) {
	handler := newTestServerHandler(t, Config{})

	cases := []struct {
		name    string
		method  string
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		{
			name:   "method does not match path",
			method: "session/list",
			body:   `{"type":"client-request","rpcId":"r1","method":"session/page","payload":{"args":{}}}`,
			status: http.StatusOK,
			code:   "gateway/bad-request",
		},
		{
			name:   "payload without args",
			method: "session/list",
			body:   `{"type":"client-request","rpcId":"r2","method":"session/list","payload":{}}`,
			status: http.StatusOK,
			code:   "gateway/bad-request",
		},
		{
			name:   "payload args not an object",
			method: "session/list",
			body:   `{"type":"client-request","rpcId":"r3","method":"session/list","payload":{"args":"no"}}`,
			status: http.StatusOK,
			code:   "gateway/bad-request",
		},
		{
			name:   "wrong envelope type",
			method: "session/list",
			body:   `{"type":"notification","rpcId":"r4","method":"session/list","payload":{"args":{}}}`,
			status: http.StatusOK,
			code:   "gateway/bad-request",
		},
		{
			name:   "missing rpcId",
			method: "session/list",
			body:   `{"type":"client-request","method":"session/list","payload":{"args":{}}}`,
			status: http.StatusOK,
			code:   "gateway/bad-request",
		},
		{
			name:   "body is not json",
			method: "session/list",
			body:   `not json`,
			status: http.StatusOK,
			code:   "gateway/bad-request",
		},
		{
			name:    "wrong media type",
			method:  "session/list",
			body:    `{"type":"client-request","rpcId":"r5","method":"session/list","payload":{"args":{}}}`,
			headers: map[string]string{"Content-Type": "text/plain"},
			status:  http.StatusUnsupportedMediaType,
		},
		{
			name:   "path traversal",
			method: "../secret",
			body:   `{"type":"client-request","rpcId":"r6","method":"../secret","payload":{"args":{}}}`,
			status: http.StatusNotFound,
		},
		{
			name:   "unknown endpoint",
			method: "session/nope",
			body:   `{"type":"client-request","rpcId":"r7","method":"session/nope","payload":{"args":{"request":{}}}}`,
			status: http.StatusOK,
			code:   "unimplemented",
		},
		{
			name:   "stream endpoint over http",
			method: "session/follow",
			body:   `{"type":"client-request","rpcId":"r8","method":"session/follow","payload":{"args":{"request":{"address":{"kind":"session","sessionId":"x"}}}}}`,
			status: http.StatusOK,
			code:   "unimplemented",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := rpcCall(t, handler, test.method, test.body, test.headers)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, test.status, recorder.Body.String())
			}
			if test.status != http.StatusOK {
				return
			}
			response := decodeResponse(t, recorder)
			if response.Result == nil || response.Result.OK {
				t.Fatalf("result = %+v", response.Result)
			}
			if response.Result.Error == nil || response.Result.Error.Code != test.code {
				t.Fatalf("error = %+v, want code %q", response.Result.Error, test.code)
			}
			if response.RPCID == "" && !strings.Contains(test.body, `"rpcId"`) {
				return
			}
		})
	}
}

// TestRPCBearsTheLaunchToken proves the HTTP bridge accepts the same credential
// an out-of-process client reads from the listen record.
func TestRPCBearsTheLaunchToken(t *testing.T) {
	handler := newTestServerHandler(t, Config{Token: "launch-token"})
	body := `{"type":"client-request","rpcId":"auth","method":"session/list","payload":{"args":{"_request":{}}}}`

	if unauthorized := rpcCall(t, handler, "session/list", body, nil); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthorized.Code)
	}
	bad := rpcCall(t, handler, "session/list", body, map[string]string{"Authorization": "Bearer wrong"})
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", bad.Code)
	}
	good := rpcCall(t, handler, "session/list", body, map[string]string{"Authorization": "Bearer launch-token"})
	if decoded := decodeResponse(t, good); decoded.Result == nil || !decoded.Result.OK {
		t.Fatalf("bearer result = %+v", decoded.Result)
	}
}

// TestListenRecordRoundTrip proves discovery publishes a usable record and
// removes it again.
func TestListenRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	record := ListenRecord{PID: 4242, Origin: "http://127.0.0.1:8300", Token: "t"}
	path, err := writeListenRecord(dir, record)
	if err != nil {
		t.Fatalf("writeListenRecord: %v", err)
	}
	if path != dir+"/"+ListenFileName {
		t.Fatalf("path = %q", path)
	}
	read, err := ReadListenRecord(dir)
	if err != nil {
		t.Fatalf("ReadListenRecord: %v", err)
	}
	if read == nil || read.PID != 4242 || read.Origin != record.Origin || read.Token != "t" {
		t.Fatalf("record = %+v", read)
	}
	// A record written by another process must not be removed.
	clearListenRecord(dir, 9999)
	if read, _ := ReadListenRecord(dir); read == nil {
		t.Fatal("a foreign record was removed")
	}
	clearListenRecord(dir, 4242)
	if read, _ := ReadListenRecord(dir); read != nil {
		t.Fatalf("record survived its owner: %+v", read)
	}
}

// TestWorkerBundleIsNotAnRPC pins the regression that left the captured
// frontend stuck on "Reconnecting…": GET requests under /api are static routes,
// not Remote calls, so the SharedWorker bundle and the other Host-provided API
// GETs must be served as assets (or proxied), never answered with 405.
func TestWorkerBundleIsNotAnRPC(t *testing.T) {
	dir := t.TempDir()
	worker := []byte(`self.onmessage = (event) => self.postMessage(event.data);`)
	manifest := assets.Manifest{
		Source:     "test",
		CapturedAt: "2025-01-01T00:00:00Z",
		Index:      "/",
		Entries: map[string]assets.Entry{
			"/": {File: "index.html", ContentType: "text/html; charset=utf-8", Size: int64(len(testIndex))},
			WorkerPath: {
				File:        "worker.js",
				ContentType: "text/javascript; charset=utf-8",
				Size:        int64(len(worker)),
			},
		},
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mustJSON(t, manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(testIndex), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.js"), worker, 0o644); err != nil {
		t.Fatalf("write worker: %v", err)
	}
	captured, err := assets.Load(dir)
	if err != nil {
		t.Fatalf("assets.Load: %v", err)
	}
	ts := newTestServer(t, Config{Frontend: captured})

	resp := get(t, ts.URL+WorkerPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status = %d, want 200", WorkerPath, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("content type = %q", ct)
	}
	if body := readBody(t, resp); body != string(worker) {
		t.Fatalf("body = %q", body)
	}
}

// TestWorkerBundleFallsThroughToProxy proves a GET under /api reaches the
// running UI in development, which is where the capture and the page share one
// origin.
func TestWorkerBundleFallsThroughToProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte("proxied:" + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)
	ts := newTestServer(t, Config{ProxyTo: upstream.URL})

	for _, path := range []string{WorkerPath, "/api/session.export", "/api/changes.diff"} {
		resp := get(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.StatusCode)
		}
		if body := readBody(t, resp); body != "proxied:"+path {
			t.Fatalf("%s body = %q", path, body)
		}
	}
}

// TestRPCStillRequiresPost proves a plain POST still reaches the bridge and a
// GET does not, so widening the static route never turned into a free method.
func TestRPCStillRequiresPost(t *testing.T) {
	handler := newTestServerHandler(t, Config{})
	body := `{"type":"client-request","rpcId":"g1","method":"session/list","payload":{"args":{"_request":{}}}}`
	getRequest := httptest.NewRequest(http.MethodGet, APIPrefix+"session/list", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, getRequest)
	if recorder.Code == http.StatusOK && strings.Contains(recorder.Body.String(), "server-response") {
		t.Fatal("a GET was answered as an RPC")
	}
	post := rpcCall(t, handler, "session/list", body, nil)
	if decoded := decodeResponse(t, post); decoded.Result == nil || !decoded.Result.OK {
		t.Fatalf("POST result = %+v", decoded.Result)
	}
}
