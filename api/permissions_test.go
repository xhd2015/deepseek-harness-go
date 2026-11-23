package api

import (
	"encoding/json"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestNewSessionStartsAtWorkspaceWrite is the acceptance criterion: the default is
// the safe one, and it is recorded rather than assumed.
func TestNewSessionStartsAtWorkspaceWrite(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "hello"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)

	state := session.FoldPermission(opened.Events())
	if state.Preset != session.PresetWorkspaceWrite {
		t.Fatalf("preset = %q, want workspace-write", state.Preset)
	}
	if state.Sandbox != session.SandboxWorkspaceWrite || state.Approval != session.ApprovalAsk {
		t.Fatalf("state = %+v", state)
	}
	// The three records are what the page and a later fold read.
	seen := map[string]bool{}
	for _, event := range opened.Events() {
		switch event.Type {
		case session.EventPermissionPreset, session.EventSandboxMode, session.EventApprovalPolicy:
			seen[event.Type] = true
		}
	}
	for _, typ := range []string{session.EventPermissionPreset, session.EventSandboxMode, session.EventApprovalPolicy} {
		if !seen[typ] {
			t.Fatalf("%s was not recorded", typ)
		}
	}
}

// TestPermissionCatalogOffersTheDSHTable covers the three presets the page offers.
func TestPermissionCatalogOffersTheDSHTable(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	value := f.invoke(t, "permissionPresets/catalog", map[string]any{
		"sessionId": opened.ID(),
	}).mustSucceed(t)
	var catalog struct {
		Options []struct {
			Value    string `json:"value"`
			Sandbox  string `json:"sandbox"`
			Approval string `json:"approval"`
			Desc     string `json:"description"`
		} `json:"options"`
		DefaultPreset string `json:"defaultPreset"`
		CurrentValue  string `json:"currentValue"`
	}
	if err := json.Unmarshal(value, &catalog); err != nil {
		t.Fatalf("decode %s: %v", value, err)
	}
	want := map[string]struct{ sandbox, approval string }{
		"read-only":          {session.SandboxReadOnly, session.ApprovalAsk},
		"workspace-write":    {session.SandboxWorkspaceWrite, session.ApprovalAsk},
		"danger-full-access": {session.SandboxDangerFull, session.ApprovalNever},
	}
	if len(catalog.Options) != len(want) {
		t.Fatalf("options = %+v", catalog.Options)
	}
	for _, option := range catalog.Options {
		expected, ok := want[option.Value]
		if !ok {
			t.Fatalf("unexpected preset %q", option.Value)
		}
		if option.Sandbox != expected.sandbox || option.Approval != expected.approval {
			t.Fatalf("%s = %+v, want %+v", option.Value, option, expected)
		}
		if option.Desc == "" {
			t.Fatalf("%s has no description for the picker", option.Value)
		}
	}
	if catalog.DefaultPreset != session.PresetWorkspaceWrite {
		t.Fatalf("defaultPreset = %q", catalog.DefaultPreset)
	}
	if catalog.CurrentValue != session.PresetWorkspaceWrite {
		t.Fatalf("currentValue = %q", catalog.CurrentValue)
	}
}

// TestPermissionSetWritesAllThreeKnobs is what makes a switch real.
func TestPermissionSetWritesAllThreeKnobs(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "permissionPresets/set", map[string]any{
		"sessionId": opened.ID(),
		"preset":    "read-only",
	}).mustSucceed(t)

	state := session.FoldPermission(opened.Events())
	if state.Sandbox != session.SandboxReadOnly {
		t.Fatalf("sandbox = %q", state.Sandbox)
	}
	if state.Approval != session.ApprovalAsk {
		t.Fatalf("approval = %q", state.Approval)
	}
	// The switch survives a reopen.
	reopened, err := f.store.Open(opened.ID())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := session.FoldPermission(reopened.Events()).Preset; got != "read-only" {
		t.Fatalf("after reopen preset = %q", got)
	}
}

// TestPermissionCommandSwitchesThePreset covers the `/permission` line the popup
// submits.
func TestPermissionCommandSwitchesThePreset(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	var execution struct {
		Result struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"result"`
	}
	decode(t, f.invoke(t, "commands/execute", map[string]any{
		"sessionId": opened.ID(), "line": "/permission danger-full-access",
	}).mustSucceed(t), &execution)
	if execution.Result.Kind != "success" {
		t.Fatalf("result = %+v", execution.Result)
	}
	if got := session.FoldPermission(opened.Events()).Approval; got != session.ApprovalNever {
		t.Fatalf("approval = %q after danger-full-access", got)
	}

	// An unknown preset is refused and names the real ones.
	decode(t, f.invoke(t, "commands/execute", map[string]any{
		"sessionId": opened.ID(), "line": "/permission nope",
	}).mustSucceed(t), &execution)
	if execution.Result.Kind != "error" || execution.Result.Text == "" {
		t.Fatalf("unknown preset result = %+v", execution.Result)
	}
}

// TestPermissionNeverRewritesAnOldSession keeps an existing log intact: it restores
// from its own last events rather than being migrated.
func TestPermissionNeverRewritesAnOldSession(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	// A session that recorded the old default.
	if _, err := opened.Append(session.EventPermissionPreset, map[string]any{"preset": "danger-full-access"}); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Append(session.EventSandboxMode, map[string]any{"mode": "danger-full-access"}); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Append(session.EventApprovalPolicy, map[string]any{"policy": "never"}); err != nil {
		t.Fatal(err)
	}
	before := len(opened.Events())

	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"sessionId": opened.ID(),
			"content":   []map[string]any{{"type": "text", "text": "hello"}},
		},
	}).mustSucceed(t)
	waitForTurnEnds(t, opened, 1, 10*1000*1000*1000)

	state := session.FoldPermission(opened.Events())
	if state.Sandbox != session.SandboxDangerFull || state.Approval != session.ApprovalNever {
		t.Fatalf("an old session was migrated: %+v", state)
	}
	// No second set of permission records was appended.
	records := 0
	for _, event := range opened.Events() {
		switch event.Type {
		case session.EventPermissionPreset, session.EventSandboxMode, session.EventApprovalPolicy:
			records++
		}
	}
	if records != 3 {
		t.Fatalf("permission records = %d, want the original 3 (before %d events)", records, before)
	}
}
