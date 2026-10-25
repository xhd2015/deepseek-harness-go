package mux

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseClientFrame(t *testing.T) {
	frame, err := ParseClientFrame([]byte(`{"type":"open","streamId":"1","endpoint":"session/list","payload":{"args":{"_request":{}}}}`))
	if err != nil {
		t.Fatalf("valid open frame rejected: %v", err)
	}
	if frame.Type != FrameOpen || frame.StreamID != "1" || frame.Endpoint != "session/list" {
		t.Fatalf("unexpected frame: %+v", frame)
	}
	if string(frame.Payload) != `{"args":{"_request":{}}}` {
		t.Fatalf("payload not preserved: %s", frame.Payload)
	}

	frame, err = ParseClientFrame([]byte(`{"type":"item","streamId":"1"}`))
	if err != nil {
		t.Fatalf("item frame without value rejected: %v", err)
	}
	if frame.Value != nil {
		t.Fatalf("absent value should stay nil, got %s", frame.Value)
	}

	frame, err = ParseClientFrame([]byte(`{"type":"item","streamId":"1","value":null}`))
	if err != nil {
		t.Fatalf("item frame with null value rejected: %v", err)
	}
	if string(frame.Value) != "null" {
		t.Fatalf("explicit null should be preserved, got %q", frame.Value)
	}

	if _, err := ParseClientFrame([]byte(`{"type":"end","streamId":"1"}`)); err != nil {
		t.Fatalf("end frame rejected: %v", err)
	}
	if _, err := ParseClientFrame([]byte(`{"type":"cancel","streamId":"1"}`)); err != nil {
		t.Fatalf("cancel frame rejected: %v", err)
	}
}

func TestParseClientFrameRejects(t *testing.T) {
	cases := map[string]string{
		"extra open key":      `{"type":"open","streamId":"1","endpoint":"e","payload":{},"nope":1}`,
		"missing payload":     `{"type":"open","streamId":"1","endpoint":"e"}`,
		"empty endpoint":      `{"type":"open","streamId":"1","endpoint":"","payload":{}}`,
		"extra end key":       `{"type":"end","streamId":"1","value":1}`,
		"unknown type":        `{"type":"subscribe","streamId":"1"}`,
		"numeric stream id":   `{"type":"end","streamId":1}`,
		"empty stream id":     `{"type":"end","streamId":""}`,
		"not an object":       `[]`,
		"malformed json":      `{"type":"end",`,
		"missing stream id":   `{"type":"end"}`,
		"null payload absent": `{"type":"open","streamId":"1","endpoint":"e"}`,
	}
	for name, text := range cases {
		if _, err := ParseClientFrame([]byte(text)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestErrorFrameAlwaysCarriesObjectDetails(t *testing.T) {
	encoded, err := ErrorFrame("7", &Failure{Code: "gateway/unimplemented", Message: "nope"}).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errValue, ok := wire["error"].(map[string]any)
	if !ok {
		t.Fatalf("error must be an object: %s", encoded)
	}
	details, ok := errValue["details"].(map[string]any)
	if !ok {
		t.Fatalf("details must be an object: %s", encoded)
	}
	if len(details) != 0 {
		t.Fatalf("details should be empty: %s", encoded)
	}
}

func TestItemFrameEncoding(t *testing.T) {
	encoded, err := ItemFrame("3", nil).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(encoded) != `{"type":"item","streamId":"3","value":null}` {
		t.Fatalf("unexpected encoding: %s", encoded)
	}
}

func TestStreamLifecycle(t *testing.T) {
	srv, url := newTestServer(t, nil, func(_ context.Context, endpoint string, _ []byte, up Uplink) (Stream, error) {
		switch endpoint {
		case "echo":
			return &blockingStream{}, nil
		case "done":
			return &listStream{values: [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}}, nil
		case "fail":
			return nil, &Failure{Code: "gateway/unimplemented", Message: "no such endpoint"}
		case "uplink":
			return &uplinkEchoStream{up: up}, nil
		default:
			return nil, &Failure{Code: "gateway/unimplemented", Message: "no such endpoint"}
		}
	})
	defer srv.Close()

	conn := dial(t, url)
	defer conn.Close()

	// Normal stream: two items then end.
	write(t, conn, `{"type":"open","streamId":"a","endpoint":"done","payload":{"args":{}}}`)
	if got := readType(t, conn); got != FrameItem {
		t.Fatalf("first frame = %s, want item", got)
	}
	if got := readType(t, conn); got != FrameItem {
		t.Fatalf("second frame = %s, want item", got)
	}
	if got := readType(t, conn); got != FrameEnd {
		t.Fatalf("third frame = %s, want end", got)
	}

	// Unknown endpoint: error then end, socket still usable.
	write(t, conn, `{"type":"open","streamId":"b","endpoint":"missing","payload":{}}`)
	errorFrame := readFrame(t, conn)
	if errorFrame.Type != FrameError || errorFrame.Failure.Code != "gateway/unimplemented" {
		t.Fatalf("unexpected error frame: %+v", errorFrame)
	}
	if got := readType(t, conn); got != FrameEnd {
		t.Fatalf("after error = %s, want end", got)
	}

	// Client values reach the uplink.
	write(t, conn, `{"type":"open","streamId":"c","endpoint":"uplink","payload":{}}`)
	write(t, conn, `{"type":"item","streamId":"c","value":{"hello":"world"}}`)
	echoed := readFrame(t, conn)
	if echoed.Type != FrameItem || string(echoed.Value) != `{"hello":"world"}` {
		t.Fatalf("unexpected uplink echo: %+v (%s)", echoed, echoed.Value)
	}
	write(t, conn, `{"type":"end","streamId":"c"}`)
	if got := readType(t, conn); got != FrameEnd {
		t.Fatalf("after uplink end = %s, want end", got)
	}

	// Cancel silences the stream and keeps the socket open.
	write(t, conn, `{"type":"open","streamId":"d","endpoint":"echo","payload":{}}`)
	write(t, conn, `{"type":"cancel","streamId":"d"}`)
	write(t, conn, `{"type":"open","streamId":"e","endpoint":"done","payload":{}}`)
	if got := readType(t, conn); got != FrameItem {
		t.Fatalf("after cancel = %s, want item from the next stream", got)
	}
}

func TestDuplicateOpenClosesSocket(t *testing.T) {
	srv, url := newTestServer(t, nil, func(context.Context, string, []byte, Uplink) (Stream, error) {
		return &blockingStream{}, nil
	})
	defer srv.Close()

	conn := dial(t, url)
	defer conn.Close()
	write(t, conn, `{"type":"open","streamId":"dup","endpoint":"echo","payload":{}}`)
	write(t, conn, `{"type":"open","streamId":"dup","endpoint":"echo","payload":{}}`)
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("duplicate open should close the socket")
	}
}

func TestProtocolViolationClosesSocket(t *testing.T) {
	srv, url := newTestServer(t, nil, func(context.Context, string, []byte, Uplink) (Stream, error) {
		return &blockingStream{}, nil
	})
	defer srv.Close()

	conn := dial(t, url)
	defer conn.Close()
	write(t, conn, `{"type":"item","streamId":"x","value":1,"extra":true}`)
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("malformed frame should close the socket")
	}
}

func TestHeartbeatPingsAndStopsSilentClients(t *testing.T) {
	srv, url := newTestServer(t, &Server{HeartbeatInterval: 40 * time.Millisecond}, func(context.Context, string, []byte, Uplink) (Stream, error) {
		return &blockingStream{}, nil
	})
	defer srv.Close()
	conn := dial(t, url)
	defer conn.Close()
	pinged := make(chan struct{}, 8)
	conn.SetPingHandler(func(string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return nil
	})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case <-pinged:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a heartbeat ping")
	}
	// A client that never answers pongs is disconnected.
	conn2 := dial(t, url)
	defer conn2.Close()
	conn2.SetPongHandler(nil)
	conn2.SetPingHandler(func(string) error { return nil })
	if _, _, err := conn2.ReadMessage(); err == nil {
		t.Fatal("silent client should be disconnected")
	}
}

// --- test helpers ---

type testServer struct {
	*httptest.Server
	Config *Server
}

func newTestServer(t *testing.T, cfg *Server, opener Opener) (*testServer, string) {
	t.Helper()
	if cfg == nil {
		cfg = &Server{}
	}
	cfg.Opener = opener
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Hour
	}
	if cfg.MaxMissedHeartbeats == 0 {
		cfg.MaxMissedHeartbeats = 2
	}
	srv := httptest.NewServer(cfg)
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, Config: cfg}, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func write(t *testing.T, conn *websocket.Conn, text string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) ServerFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	frame, err := ParseServerFrame(data)
	if err != nil {
		t.Fatalf("parse %s: %v", data, err)
	}
	return frame
}

func readType(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	return readFrame(t, conn).Type
}

// listStream yields canned values and then ends.
type listStream struct {
	values [][]byte
	index  int
	closed bool
}

func (s *listStream) Next(ctx context.Context) ([]byte, error) {
	if s.index >= len(s.values) {
		return nil, io.EOF
	}
	value := s.values[s.index]
	s.index++
	return value, nil
}

func (s *listStream) Close() { s.closed = true }

// blockingStream stays open until cancelled.
type blockingStream struct{}

func (s *blockingStream) Next(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *blockingStream) Close() {}

// uplinkEchoStream reflects client values back as downlink items.
type uplinkEchoStream struct {
	up Uplink
}

func (s *uplinkEchoStream) Next(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case value, ok := <-s.up.Values():
		if !ok {
			return nil, io.EOF
		}
		return value, nil
	}
}

func (s *uplinkEchoStream) Close() {}
