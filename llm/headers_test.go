package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAttributionHeadersAreSent matches DSH: an upstream proxy bills and routes by
// these, and a request without them is billed as anonymous.
func TestAttributionHeadersAreSent(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data\n"))
	}))
	t.Cleanup(server.Close)

	client := New(Config{BaseURL: server.URL, Model: "m", APIKey: "k"})
	if _, err := client.Stream(context.Background(), Request{
		Model:     "m",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "session-abc",
	}, func(Chunk) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.Get("x-agent-runner") != "dsh" {
		t.Fatalf("x-agent-runner = %q", got.Get("x-agent-runner"))
	}
	if got.Get("x-dsh-session-id") != "session-abc" {
		t.Fatalf("x-dsh-session-id = %q", got.Get("x-dsh-session-id"))
	}
}

// TestConfiguredAttributionWins keeps a deployment's own value intact.
func TestConfiguredAttributionWins(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data\n"))
	}))
	t.Cleanup(server.Close)

	client := New(Config{
		BaseURL: server.URL, Model: "m", APIKey: "k",
		Headers: map[string]string{"x-agent-runner": "my-fork"},
	})
	if _, err := client.Stream(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, SessionID: "s",
	}, func(Chunk) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.Get("x-agent-runner") != "my-fork" {
		t.Fatalf("the configured value was replaced: %q", got.Get("x-agent-runner"))
	}
}

// TestNoSessionIDMeansNoHeader avoids claiming an empty conversation.
func TestNoSessionIDMeansNoHeader(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data\n"))
	}))
	t.Cleanup(server.Close)

	client := New(Config{BaseURL: server.URL, Model: "m", APIKey: "k"})
	if _, err := client.Stream(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(Chunk) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.Get("x-dsh-session-id") != "" {
		t.Fatalf("x-dsh-session-id = %q, want absent", got.Get("x-dsh-session-id"))
	}
	if got.Get("x-agent-runner") != "dsh" {
		t.Fatalf("x-agent-runner = %q", got.Get("x-agent-runner"))
	}
}
