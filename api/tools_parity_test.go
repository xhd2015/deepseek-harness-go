package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/agent"
)

// coreToolNames are the tools a session must have to feel like DSH: the file,
// command, search, job, todo, web, image tools, the question tool, and the
// working directory. Their schemas are compared byte for byte with the web preset,
// because a renamed field or a reworded description changes what the model does
// with them even when the tool still runs.
var coreToolNames = []string{
	"ask_user_question", "bash", "edit", "glob", "grep",
	"job_kill", "job_list", "job_output", "read", "read_image",
	"todo_write", "web_fetch", "web_search", "working_directory", "write",
}

// outOfScopeToolNames are the DSH tools this backend deliberately does not offer.
//
// Decision 6 keeps subagents, schedules, terminals, skills, goals, plugins,
// workflows and the Cordis inspector out of scope. Naming them here is what stops
// a half-built one from appearing: the model must not see a tool the backend
// cannot honour, because a plausible empty answer is worse than an absent tool.
var outOfScopeToolNames = []string{
	"cordis_inspect_list", "cordis_inspect_query", "create_goal", "get_goal",
	"list_agents", "list_subagent_models", "plugin_manager", "schedule_create",
	"schedule_edit", "schedule_list", "schedule_view", "send_message", "skill",
	"subagent", "subagent_fork", "update_goal", "wait_agent", "workflow",
}

// dshToolSchemas reads the web preset's schema list, keyed by tool name.
func dshToolSchemas(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	root := dshSnapshotRoot()
	if root == "" {
		t.Skip("no DeepSeek Harness checkout with tool schema snapshots; set DSH_GO_DSH_ROOT")
	}
	body, err := os.ReadFile(filepath.Join(root, "snapshots/web/creator-persona/tool-schemas.expected.json"))
	if err != nil {
		t.Skipf("no tool schema fixture: %v", err)
	}
	var fixture struct {
		Initial []struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"initial"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	schemas := make(map[string]json.RawMessage, len(fixture.Initial))
	for _, schema := range fixture.Initial {
		schemas[schema.Name] = schema.Parameters
	}
	return schemas
}

// TestCoreToolSchemasMatchTheWebPreset is the parity contract for the tools a
// session actually uses, field for field.
func TestCoreToolSchemasMatchTheWebPreset(t *testing.T) {
	dsh := dshToolSchemas(t)
	executor := agent.NewToolExecutor(t.TempDir())
	own := map[string]string{}
	for _, def := range executor.Definitions() {
		encoded, err := json.Marshal(def.Parameters)
		if err != nil {
			t.Fatalf("encode %s: %v", def.Name, err)
		}
		own[def.Name] = string(encoded)
	}
	for _, name := range coreToolNames {
		want, ok := dsh[name]
		if !ok {
			t.Fatalf("the fixture has no %s; the core list is stale", name)
		}
		got, ok := own[name]
		if !ok {
			t.Errorf("%s is a core tool but is not offered", name)
			continue
		}
		// Compare through a canonical decode: key order is not part of the contract.
		if canonicalJSON(t, got) != canonicalJSON(t, string(want)) {
			t.Errorf("%s schema differs from the web preset\n got: %s\nwant: %s", name, got, want)
		}
	}
}

// TestOutOfScopeToolsAreNotOffered is the other half of Decision 6: the model must
// not see a name this backend has no implementation for.
func TestOutOfScopeToolsAreNotOffered(t *testing.T) {
	dsh := dshToolSchemas(t)
	executor := agent.NewToolExecutor(t.TempDir())
	offered := map[string]bool{}
	for _, def := range executor.Definitions() {
		offered[def.Name] = true
	}
	var problems []string
	for _, name := range outOfScopeToolNames {
		if _, known := dsh[name]; !known {
			problems = append(problems, name+" is not a DSH tool any more; drop it from the list")
			continue
		}
		if offered[name] {
			problems = append(problems, name+" is out of scope but is offered to the model")
		}
	}
	if len(problems) > 0 {
		t.Fatalf("out-of-scope tools drifted:\n%s", strings.Join(problems, "\n"))
	}
}

// TestCoreToolListCoversTheWebPreset states the split in one place, so a new DSH
// tool cannot quietly become neither core nor explicitly out of scope.
func TestCoreToolListCoversTheWebPreset(t *testing.T) {
	dsh := dshToolSchemas(t)
	declared := map[string]bool{}
	for _, name := range coreToolNames {
		declared[name] = true
	}
	for _, name := range outOfScopeToolNames {
		declared[name] = true
	}
	var missing []string
	for name := range dsh {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	// present is a deliverables tool the web preset exposes but this backend has
	// not built; it is the only name allowed to sit outside both lists.
	for _, name := range missing {
		if name != "present" {
			t.Errorf("%s is in the web preset but is neither core nor out of scope", name)
		}
	}
}

// canonicalJSON re-encodes JSON with sorted keys so two encodings compare equal
// when they mean the same thing.
func canonicalJSON(t *testing.T, raw string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return string(encoded)
}
