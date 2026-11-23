package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestAgentPresetsListIsRealData is the acceptance criterion: the roster has rows,
// and each says the composition is built in rather than composed from plugins.
func TestAgentPresetsListIsRealData(t *testing.T) {
	f := newFixture(t)
	value := f.invoke(t, "agentPresets/list", map[string]any{}).mustSucceed(t)
	var roster struct {
		Presets []struct {
			ID          string            `json:"id"`
			Name        string            `json:"name"`
			Description string            `json:"description"`
			IsDefault   bool              `json:"isDefault"`
			Plugins     []json.RawMessage `json:"plugins"`
		} `json:"presets"`
	}
	if err := json.Unmarshal(value, &roster); err != nil {
		t.Fatalf("decode %s: %v", value, err)
	}
	if len(roster.Presets) < 2 {
		t.Fatalf("presets = %+v, want at least standard and minimal", roster.Presets)
	}
	ids := map[string]bool{}
	defaults := 0
	for _, preset := range roster.Presets {
		if preset.ID == "" || preset.Name == "" || preset.Description == "" {
			t.Fatalf("incomplete preset row: %+v", preset)
		}
		if ids[preset.ID] {
			t.Fatalf("duplicate preset id %q", preset.ID)
		}
		ids[preset.ID] = true
		if preset.IsDefault {
			defaults++
			if preset.ID != "standard" {
				t.Fatalf("default preset = %q", preset.ID)
			}
		}
		// `plugins` is present and empty: the field must not be null, or a client
		// rendering it would show "unknown" instead of "none".
		if preset.Plugins == nil {
			t.Fatalf("%s has no plugins field", preset.ID)
		}
	}
	if defaults != 1 {
		t.Fatalf("%d presets claim to be the default", defaults)
	}
	for _, want := range []string{"standard", "minimal"} {
		if !ids[want] {
			t.Fatalf("preset %q is missing", want)
		}
	}
}

// TestAgentPresetReadReportsTools lets the page show what a preset changes.
func TestAgentPresetReadReportsTools(t *testing.T) {
	f := newFixture(t)
	minimal := f.invoke(t, "agentPresets/read", map[string]any{
		"agentPreset": "minimal",
	}).mustSucceed(t)
	var document struct {
		ID     string   `json:"id"`
		Tools  []string `json:"tools"`
		Prompt string   `json:"prompt"`
	}
	decode(t, minimal, &document)
	if document.ID != "minimal" {
		t.Fatalf("id = %q", document.ID)
	}
	if len(document.Tools) == 0 || len(document.Tools) >= 20 {
		t.Fatalf("minimal tools = %v, want a shorter list than the full set", document.Tools)
	}
	if document.Prompt == "" {
		t.Fatal("minimal has no prompt")
	}
	f.invoke(t, "agentPresets/read", map[string]any{
		"agentPreset": "does-not-exist",
	}).mustFail(t, CodeNotFound)
}

// TestAgentPresetSelectShortensTheToolList is the behaviour behind the criterion:
// after selecting minimal, the request the model sees carries fewer tools.
func TestAgentPresetSelectShortensTheToolList(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	var selected struct {
		AgentPreset string `json:"agentPreset"`
	}
	decode(t, f.invoke(t, "agentPresets/select", map[string]any{
		"sessionId": opened.ID(), "agentPreset": "minimal",
	}).mustSucceed(t), &selected)
	if selected.AgentPreset != "minimal" {
		t.Fatalf("selected = %+v", selected)
	}
	// The choice is durable.
	var sawSwitch bool
	for _, event := range opened.Events() {
		if event.Type == session.EventAgentPreset && strings.Contains(string(event.Data), "minimal") {
			sawSwitch = true
		}
	}
	if !sawSwitch {
		t.Fatal("no preset switch was recorded")
	}

	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "hello"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)

	// The request the model received must carry the shorter list.
	requests := f.fakeRequests()
	if len(requests) == 0 {
		t.Fatal("the model was never called")
	}
	body := requests[len(requests)-1]
	for _, excluded := range []string{`"bash"`, `"web_search"`, `"job_output"`} {
		if strings.Contains(body, excluded) {
			t.Fatalf("minimal still offers %s: %s", excluded, body)
		}
	}
	if !strings.Contains(body, `"read"`) {
		t.Fatalf("minimal does not offer read: %s", body)
	}
}

// TestAgentPresetSelectRefusesWhileRunning keeps an in-flight step's tool list fixed.
func TestAgentPresetSelectRefusesWhileRunning(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	f := newBlockingFixture(t, block)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "go"}},
		},
	}).mustSucceed(t)
	deadline := nowPlus(5)
	for !f.runner.Busy(opened.ID()) && beforeDeadline(deadline) {
		sleepABit()
	}
	f.invoke(t, "agentPresets/select", map[string]any{
		"sessionId": opened.ID(), "agentPreset": "minimal",
	}).mustFail(t, CodeBusy)
}

// TestPluginManagerIsHonestlyEmpty is the other acceptance criterion: an empty page,
// not a crash and not fabricated bundles.
func TestPluginManagerIsHonestlyEmpty(t *testing.T) {
	f := newFixture(t)
	for _, method := range []string{"pluginManager/listBundles", "pluginManager/listPlugins"} {
		value := f.invoke(t, method, map[string]any{}).mustSucceed(t)
		var reply struct {
			Bundles []any  `json:"bundles"`
			Plugins []any  `json:"plugins"`
			Notice  string `json:"notice"`
		}
		if err := json.Unmarshal(value, &reply); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if reply.Notice == "" {
			t.Fatalf("%s has no notice explaining why it is empty", method)
		}
		if method == "pluginManager/listBundles" && len(reply.Bundles) != 0 {
			t.Fatalf("listBundles invented %d bundles", len(reply.Bundles))
		}
		if method == "pluginManager/listPlugins" && len(reply.Plugins) != 0 {
			t.Fatalf("listPlugins invented %d plugins", len(reply.Plugins))
		}
	}
	// A mutating call is refused rather than reported as done.
	f.invoke(t, "pluginManager/installBundle", map[string]any{"spec": "x"}).
		mustFail(t, CodeUnimplemented)
}
