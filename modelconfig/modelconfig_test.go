package modelconfig

import (
	"os"
	"path/filepath"
	"testing"
)

const patchFixture = `
- id: llm-pi-ai
  config:
    providers:
      llm-proxy-grok:
        displayName: Grok (llm-proxy)
        api: openai-responses
        baseURL: http://127.0.0.1:8890/v1
        reasoning: high
        models:
          - id: grok-4.7
            name: Grok 4.7
            contextWindow: 500000
            reasoningEfforts:
              low: Low
              medium: Medium
              high: High
              xhigh: Xhigh
      llm-proxy-commandcode:
        api: anthropic-messages
        baseURL: http://127.0.0.1:8890
        reasoning: xhigh
        models:
          - id: deepseek/deepseek-v4.1-flash-from-dsh
            name: DeepSeek V4.1 Flash
            reasoningEfforts:
              high: High
              max: Max
              xhigh: Xhigh
      broken:
        api: carrier-pigeon
        models:
          - id: bird
- id: agent-default-model
  config:
    provider: llm-proxy-grok
    model: grok-4.7
    reasoningEffort: high
`

func writePatch(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cordis.patch.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	return path
}

func TestLoadPatchBuildsCatalog(t *testing.T) {
	catalog, err := LoadPatch(writePatch(t, patchFixture))
	if err != nil {
		t.Fatalf("load patch: %v", err)
	}
	if catalog == nil {
		t.Fatal("catalog is nil")
	}
	if got, want := catalog.Default, (Selection{Provider: "llm-proxy-grok", Model: "grok-4.7", ReasoningEffort: "high"}); got != want {
		t.Fatalf("default = %#v, want %#v", got, want)
	}
	if catalog.RouteCount() != 2 {
		t.Fatalf("route count = %d, want 2", catalog.RouteCount())
	}
	if len(catalog.Failures) != 1 || catalog.Failures[0].ID != "broken" {
		t.Fatalf("failures = %#v, want the broken provider", catalog.Failures)
	}
	grok := catalog.Providers[0]
	if grok.Protocol != "openai-responses" || grok.BaseURL != "http://127.0.0.1:8890/v1" {
		t.Fatalf("grok provider = %#v", grok)
	}
	if got := modelIDs(grok.Models[0].Efforts); len(got) != 4 || got[0] != "low" || got[3] != "xhigh" {
		t.Fatalf("grok efforts = %v, want low/medium/high/xhigh", got)
	}
	if grok.Models[0].DefaultEffort != "high" {
		t.Fatalf("grok default effort = %q, want high", grok.Models[0].DefaultEffort)
	}
	commandcode := catalog.Providers[1]
	if commandcode.Protocol != "anthropic-messages" || commandcode.Models[0].DefaultEffort != "xhigh" {
		t.Fatalf("commandcode provider = %#v", commandcode)
	}
}

func TestResolveFillsDefaultEffortAndRejectsUnknown(t *testing.T) {
	catalog, err := LoadPatch(writePatch(t, patchFixture))
	if err != nil {
		t.Fatalf("load patch: %v", err)
	}
	route, err := catalog.Resolve(Selection{Provider: "llm-proxy-grok", Model: "grok-4.7"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.ReasoningEffort != "high" || route.Protocol != "openai-responses" {
		t.Fatalf("route = %#v", route)
	}
	if _, err := catalog.Resolve(Selection{Provider: "other", Model: "grok-4.7"}); err == nil {
		t.Fatal("unknown provider resolved")
	}
	if _, err := catalog.Resolve(Selection{Provider: "llm-proxy-grok", Model: "grok-4.7", ReasoningEffort: "max"}); err == nil {
		t.Fatal("unknown effort resolved")
	}
}

func TestSingleAcceptsUndelcaredEffort(t *testing.T) {
	catalog := Single("fake-provider", "fake-model", "")
	route, err := catalog.Resolve(Selection{Provider: "fake-provider", Model: "fake-model", ReasoningEffort: "high"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if route.ReasoningEffort != "high" {
		t.Fatalf("effort = %q, want high", route.ReasoningEffort)
	}
}

func TestMissingPatchFallsBack(t *testing.T) {
	catalog, err := LoadPatch(filepath.Join(t.TempDir(), "missing.yml"))
	if err != nil {
		t.Fatalf("load missing patch: %v", err)
	}
	if catalog != nil {
		t.Fatalf("catalog = %#v, want nil", catalog)
	}
}

func modelIDs(efforts []Effort) []string {
	ids := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		ids = append(ids, effort.ID)
	}
	return ids
}
