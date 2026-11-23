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

// TestOpenCodeRoutesSendTheirSessionHeader is the routing requirement: those
// upstreams reject a request without it, so a missing header makes the whole route
// unusable rather than merely unattributed.
func TestOpenCodeRoutesSendTheirSessionHeader(t *testing.T) {
	for _, provider := range []string{"opencode", "opencode-go", "llm-proxy-opencode", "llm-proxy-opencode-go"} {
		var got http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
			_, _ = w.Write([]byte("data\n"))
		}))
		client := New(Config{BaseURL: server.URL, Model: "m", APIKey: "k", Provider: provider})
		if _, err := client.Stream(context.Background(), Request{
			Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, SessionID: "s-1",
		}, func(Chunk) error { return nil }); err != nil {
			server.Close()
			t.Fatalf("%s: stream: %v", provider, err)
		}
		server.Close()
		if got.Get("x-opencode-session") != "s-1" {
			t.Fatalf("%s: x-opencode-session = %q", provider, got.Get("x-opencode-session"))
		}
		// The generic headers still travel: a proxy may route by either.
		if got.Get("x-dsh-session-id") != "s-1" || got.Get("x-agent-runner") != "dsh" {
			t.Fatalf("%s: generic headers missing: %v", provider, got)
		}
	}
}

// TestOtherProvidersDoNotSendTheOpenCodeHeader keeps the header meaningful.
func TestOtherProvidersDoNotSendTheOpenCodeHeader(t *testing.T) {
	for _, provider := range []string{"deepseek", "grok", "llm-proxy-deepseek", ""} {
		var got http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
			_, _ = w.Write([]byte("data\n"))
		}))
		client := New(Config{BaseURL: server.URL, Model: "m", APIKey: "k", Provider: provider})
		if _, err := client.Stream(context.Background(), Request{
			Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, SessionID: "s-1",
		}, func(Chunk) error { return nil }); err != nil {
			server.Close()
			t.Fatalf("%s: stream: %v", provider, err)
		}
		server.Close()
		if got.Get("x-opencode-session") != "" {
			t.Fatalf("%s: sent x-opencode-session = %q", provider, got.Get("x-opencode-session"))
		}
	}
}
