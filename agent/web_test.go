package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestWebSearchDeepSeekProviderAndFormatting(t *testing.T) {
	var body map[string]any
	var bodyMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Errorf("body = %s: %v", raw, err)
		}
		bodyMu.Lock()
		body = parsed
		bodyMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
			map[string]any{"url": "https://example.com/a", "title": "Example A", "snippet": "First snippet", "date": "2025-01-02"},
			map[string]any{"url": "https://example.com/b", "title": "", "snippet": ""},
		}})
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", server.URL)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")

	executor, _ := newToolTestExecutor(t)
	out := executor.webSearch(context.Background(), map[string]any{"queries": []any{"golang", "go testing"}})
	if out.IsError {
		t.Fatalf("web_search failed: %s", out.Text)
	}
	if body["query"] != "golang" && body["query"] != "go testing" {
		t.Fatalf("provider query = %v", body["query"])
	}
	if body["count"] != float64(webSearchMaxResults) {
		t.Fatalf("provider count = %v", body["count"])
	}
	for _, want := range []string{
		webTrustNotice,
		"- [Example A](https://example.com/a) — First snippet (2025-01-02)",
		"- [example.com](https://example.com/b)",
		"Cite the relevant URLs above as markdown links in your answer.",
	} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("output missing %q:\n%s", want, out.Text)
		}
	}
	if !strings.Contains(out.Text, "Sources:") {
		t.Fatalf("sources section missing: %s", out.Text)
	}
}

func TestWebSearchInputValidation(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{}, "queries must be an array of strings"},
		{map[string]any{"queries": []any{}}, "queries must contain at least one query"},
		{map[string]any{"queries": []any{"a", "b", "c", "d", "e"}}, "queries must contain at most 4 queries"},
		{map[string]any{"queries": []any{"ok", "  "}}, "each query must be a non-empty string"},
	}
	for _, test := range cases {
		out := executor.webSearch(context.Background(), test.args)
		if !out.IsError || !strings.Contains(out.Text, test.want) {
			t.Errorf("web_search(%v) = %q, %v; want %q", test.args, out.Text, out.IsError, test.want)
		}
	}
}

func TestMergeWebSearchResultsDeduplicatesAndCaps(t *testing.T) {
	first := webSearchResult{Sources: []webSource{
		{URL: "https://a"}, {URL: "https://b"}, {URL: "https://c"},
	}}
	second := webSearchResult{Sources: []webSource{
		{URL: "https://b"}, {URL: "https://d"}, {URL: "https://e"},
	}}
	merged := mergeWebSearchResults([]string{"one", "two"}, []webSearchResult{first, second})
	var urls []string
	for _, source := range merged.Sources {
		urls = append(urls, source.URL)
	}
	want := "https://a,https://b,https://d,https://c,https://e"
	if strings.Join(urls, ",") != want {
		t.Fatalf("merged = %v, want %s", urls, want)
	}
}

func TestHTMLToMarkdownRemovesHiddenContentAndConvertsStructure(t *testing.T) {
	source := `<html><head><title>skip</title><style>x</style></head><body>
<h1>Title</h1><p>Hello <strong>world</strong>.</p>
<script>alert(1)</script><div hidden>secret</div><div style="display: none">hidden</div>
<ul><li>one</li><li>two</li></ul>
<pre><code>fmt.Println("hi")</code></pre>
<table><tr><th>Name</th><th>Value</th></tr><tr><td>a</td><td>1</td></tr></table>
</body></html>`
	got := htmlToMarkdown(source)
	for _, want := range []string{"# Title", "Hello **world**.", "- one", "- two", "```\nfmt.Println(\"hi\")\n```", "| Name | Value |\n| --- | --- |\n| a | 1 |"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"alert(1)", "secret", "style", "hidden"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("markdown retained %q:\n%s", unwanted, got)
		}
	}
}

func TestReadWebResponseCapsBody(t *testing.T) {
	header := http.Header{}
	header.Set("content-type", "text/plain; charset=utf-8")
	response := &http.Response{
		StatusCode: 200,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", webFetchMaxBody+20))),
	}
	result, err := readWebResponse(response, mustParseWebURL(t, "https://example.com/"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len([]rune(result.Body)) != webFetchMaxBody {
		t.Fatalf("truncated=%v length=%d", result.Truncated, len([]rune(result.Body)))
	}
}

func TestWebFetchRejectsNonPublicAndUnsupportedContent(t *testing.T) {
	if _, err := fetchWeb(context.Background(), "http://127.0.0.1:1/"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("loopback fetch error = %v", err)
	}
	unsupported := http.Header{}
	unsupported.Set("content-type", "application/octet-stream")
	response := &http.Response{
		StatusCode: 200,
		Header:     unsupported,
		Body:       io.NopCloser(strings.NewReader("binary")),
	}
	if _, err := readWebResponse(response, mustParseWebURL(t, "https://example.com/file")); err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("unsupported content type error = %v", err)
	}
}

func mustParseWebURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
