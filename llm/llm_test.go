package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompleteStreamsTextUsageAndModel(t *testing.T) {
	var seen struct {
		path   string
		auth   string
		stream bool
		model  string
		effort string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		seen.auth = r.Header.Get("Authorization")
		body := decodeBody(t, r)
		seen.stream = body["stream"] == true
		seen.model, _ = body["model"].(string)
		seen.effort, _ = body["reasoning_effort"].(string)

		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"model":"fake-1","choices":[{"delta":{"reasoning_content":"thinking "}}]}`,
			`{"choices":[{"delta":{"content":"Hello "}}]}`,
			`{"choices":[{"delta":{"content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12,"prompt_cache_hit_tokens":3,"prompt_cache_miss_tokens":4}}`,
			`[DONE]`,
		}
		for _, chunk := range chunks {
			if strings.HasPrefix(chunk, "[DONE]") {
				if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
					return
				}
				continue
			}
			if _, err := w.Write([]byte("data: " + chunk + "\n\n")); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "default-model"})
	completion, err := client.Complete(context.Background(), Request{
		Model:           "fake-1",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completion.Text != "Hello world" {
		t.Fatalf("text = %q", completion.Text)
	}
	if completion.Reasoning != "thinking " {
		t.Fatalf("reasoning = %q", completion.Reasoning)
	}
	if completion.Model != "fake-1" || completion.FinishReason != "stop" {
		t.Fatalf("completion = %+v", completion)
	}
	want := Usage{InputTokens: 7, OutputTokens: 5, TotalTokens: 12, CacheReadTokens: 3, CacheWriteTokens: 4}
	if completion.Usage != want {
		t.Fatalf("usage = %+v, want %+v", completion.Usage, want)
	}
	if seen.path != "/v1/chat/completions" {
		t.Fatalf("path = %q", seen.path)
	}
	if seen.auth != "Bearer secret" {
		t.Fatalf("authorization = %q", seen.auth)
	}
	if !seen.stream || seen.model != "fake-1" || seen.effort != "high" {
		t.Fatalf("request body = %+v", seen)
	}
}

func TestCompleteUsesConfiguredModelWhenRequestIsSilent(t *testing.T) {
	var model string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		model, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"fake-1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "configured-model"})
	completion, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if model != "configured-model" {
		t.Fatalf("model = %q", model)
	}
	if completion.Text != "ok" || completion.Usage.TotalTokens != 3 {
		t.Fatalf("completion = %+v", completion)
	}
}

func TestCompleteReportsProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "m"})
	_, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *Failure", err)
	}
	if failure.Code != "HTTP_ERROR" || failure.StatusCode != http.StatusUnauthorized {
		t.Fatalf("failure = %+v", failure)
	}
	if failure.Message != "bad key" {
		t.Fatalf("message = %q", failure.Message)
	}
}

func TestCompleteRetriesServerErrorsOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}]}`))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "m"})
	completion, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completion.Text != "recovered" {
		t.Fatalf("text = %q", completion.Text)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestCompleteDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "m"})
	if _, err := client.Complete(context.Background(), Request{}); err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestCompleteRejectsEmptyAndMalformedAnswers(t *testing.T) {
	cases := map[string]struct {
		contentType string
		body        string
		code        string
	}{
		"empty stream":    {"text/event-stream", "data: {\"choices\":[{\"delta\":{}}]}\ndata: [DONE]\n", "EMPTY_RESPONSE"},
		"malformed chunk": {"text/event-stream", "data: not-json\n", "BAD_RESPONSE"},
		"streaming error": {"text/event-stream", "data: {\"error\":{\"message\":\"overloaded\"}}\n", "PROVIDER"},
		"empty json":      {"application/json", `{"choices":[]}`, "EMPTY_RESPONSE"},
		"malformed json":  {"application/json", `{`, "BAD_RESPONSE"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, Model: "m"})
			_, err := client.Complete(context.Background(), Request{})
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want *Failure", err)
			}
			if failure.Code != tc.code {
				t.Fatalf("code = %q, want %q", failure.Code, tc.code)
			}
		})
	}
}

func TestCompleteStopsWhenContextIsCancelled(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-blocked
	}))
	defer server.Close()
	defer close(blocked)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := New(Config{BaseURL: server.URL, Model: "m"})
	_, err := client.Complete(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func TestAnthropicCompleteStreamsTextAndUsage(t *testing.T) {
	var seen struct {
		path    string
		key     string
		version string
		body    map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		seen.key = r.Header.Get("x-api-key")
		seen.version = r.Header.Get("anthropic-version")
		seen.body = decodeBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"model":"deepseek/deepseek-v4.1-flash","usage":{"input_tokens":11,"output_tokens":0,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		}
		for _, event := range events {
			if _, err := w.Write([]byte("event: test\ndata: " + event + "\n\n")); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := New(Config{
		BaseURL:  server.URL + "/v1",
		APIKey:   "secret",
		Protocol: ProtocolAnthropicMessages,
		Model:    "default-model",
	})
	completion, err := client.Complete(context.Background(), Request{
		Model: "deepseek/deepseek-v4.1-flash",
		Messages: []Message{
			{Role: "system", Content: "be nice"},
			{Role: "user", Content: "hi"},
		},
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completion.Text != "Hello world" || completion.Reasoning != "hmm" {
		t.Fatalf("completion = %+v", completion)
	}
	if completion.Model != "deepseek/deepseek-v4.1-flash" || completion.FinishReason != "stop" {
		t.Fatalf("completion = %+v", completion)
	}
	want := Usage{InputTokens: 11, OutputTokens: 2, TotalTokens: 13, CacheReadTokens: 3, CacheWriteTokens: 4}
	if completion.Usage != want {
		t.Fatalf("usage = %+v, want %+v", completion.Usage, want)
	}
	if seen.path != "/v1/messages" {
		t.Fatalf("path = %q", seen.path)
	}
	if seen.key != "secret" || seen.version != "2023-06-01" {
		t.Fatalf("headers = key %q, version %q", seen.key, seen.version)
	}
	if seen.body["stream"] != true || seen.body["max_tokens"] != float64(128) {
		t.Fatalf("body = %+v", seen.body)
	}
	if seen.body["system"] != "be nice" {
		t.Fatalf("system = %v", seen.body["system"])
	}
	messages, _ := seen.body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %+v", seen.body["messages"])
	}
	message := messages[0].(map[string]any)
	if message["role"] != "user" {
		t.Fatalf("role = %v", message["role"])
	}
	content := message["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != "hi" {
		t.Fatalf("content = %+v", content)
	}
}

func TestAnthropicCompleteAcceptsJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m2","content":[{"type":"text","text":"json answer"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Protocol: ProtocolAnthropicMessages, Model: "m"})
	completion, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completion.Text != "json answer" || completion.FinishReason != "stop" {
		t.Fatalf("completion = %+v", completion)
	}
	if completion.Usage != (Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}) {
		t.Fatalf("usage = %+v", completion.Usage)
	}
}

func TestAnthropicCompleteReportsProviderErrorEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Protocol: ProtocolAnthropicMessages, Model: "m"})
	_, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "PROVIDER" || failure.Message != "busy" {
		t.Fatalf("error = %v, want PROVIDER busy", err)
	}
}

func TestAnthropicRejectsEmptyTranscript(t *testing.T) {
	client := New(Config{BaseURL: "http://127.0.0.1:9", Protocol: ProtocolAnthropicMessages, Model: "m"})
	_, err := client.Complete(context.Background(), Request{})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "INVALID_REQUEST" {
		t.Fatalf("error = %v, want INVALID_REQUEST", err)
	}
}

func TestCompleteRejectsUnknownProtocol(t *testing.T) {
	client := New(Config{BaseURL: "http://127.0.0.1:9", Protocol: "carrier-pigeon", Model: "m"})
	_, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "INVALID_REQUEST" {
		t.Fatalf("error = %v, want INVALID_REQUEST", err)
	}
}

func TestFinishChunkUsesContractShape(t *testing.T) {
	encoded, err := json.Marshal(Chunk{Type: "finish", Reason: "stop"})
	if err != nil {
		t.Fatalf("marshal finish: %v", err)
	}
	if got, want := string(encoded), `{"reason":{"kind":"stop"},"type":"finish"}`; got != want {
		t.Fatalf("finish = %s, want %s", got, want)
	}
}

func TestMaxTokensPrunesToolCallsFromBlocksAndReplay(t *testing.T) {
	replay := json.RawMessage(`{"response":{"kind":"deepseek-messages","version":1,"model":"m"},"blocks":[{"type":"text"},{"type":"reasoning","signature":"sig"},{"type":"tool-call"}]}`)
	var acc streamAccumulator
	for _, chunk := range []Chunk{
		{Type: "block-start", Index: 0, BlockType: "text"},
		{Type: "text-delta", Index: 0, Text: "partial"},
		{Type: "block-start", Index: 1, BlockType: "reasoning"},
		{Type: "block-end", Index: 1, Block: &ContentBlock{Type: "reasoning", Signature: "sig"}},
		{Type: "block-start", Index: 2, BlockType: "tool-call"},
		{Type: "tool-call-delta", Index: 2, ID: "call-1", HasName: true, Name: "read", ArgumentsDelta: `{"file_path":"x"}`},
		{Type: "block-end", Index: 2, Block: &ContentBlock{Type: "tool-call", ID: "call-1", Name: "read", Arguments: `{"file_path":"x"}`}},
		{Type: "finish", Reason: "max_tokens", ReplayState: replay},
	} {
		acc.accept(chunk)
	}
	completion := acc.completion()
	if completion.FinishReason != "max-tokens" {
		t.Fatalf("finish = %q", completion.FinishReason)
	}
	if len(completion.Blocks) != 2 || completion.Blocks[0].Type != "text" || completion.Blocks[1].Type != "reasoning" {
		t.Fatalf("blocks = %+v", completion.Blocks)
	}
	if completion.Blocks[1].Text != "" {
		t.Fatalf("empty reasoning block was dropped or changed: %+v", completion.Blocks[1])
	}
	if len(completion.ToolCalls) != 0 {
		t.Fatalf("tool calls = %+v", completion.ToolCalls)
	}
	var envelope struct {
		Response map[string]any   `json:"response"`
		Blocks   []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(completion.Replay, &envelope); err != nil {
		t.Fatalf("replay = %s: %v", completion.Replay, err)
	}
	if envelope.Response["model"] != "m" {
		t.Fatalf("replay response = %+v", envelope.Response)
	}
	if len(envelope.Blocks) != 2 || envelope.Blocks[0]["type"] != "text" || envelope.Blocks[1]["type"] != "reasoning" {
		t.Fatalf("replay blocks = %+v", envelope.Blocks)
	}
}

func TestReplayIsOmittedWhenEntriesDoNotAlign(t *testing.T) {
	replay := json.RawMessage(`{"response":{"kind":"deepseek-messages"},"blocks":[{"type":"text"},{"type":"reasoning"}]}`)
	var acc streamAccumulator
	for _, chunk := range []Chunk{
		{Type: "block-start", Index: 0, BlockType: "text"},
		{Type: "text-delta", Index: 0, Text: "hi"},
		{Type: "block-start", Index: 1, BlockType: "reasoning"},
		{Type: "block-start", Index: 2, BlockType: "tool-call"},
		{Type: "tool-call-delta", Index: 2, ID: "call-1", HasName: true, Name: "read", ArgumentsDelta: "{}"},
		{Type: "finish", Reason: "tool-calls", ReplayState: replay},
	} {
		acc.accept(chunk)
	}
	completion := acc.completion()
	if completion.Replay != nil {
		t.Fatalf("replay = %s, want nil", completion.Replay)
	}
	if len(completion.Blocks) != 3 || len(completion.ToolCalls) != 1 {
		t.Fatalf("completion = %+v", completion)
	}
}
