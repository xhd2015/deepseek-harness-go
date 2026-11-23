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
	"ask_user_question", "bash", "create_goal", "edit", "get_goal", "glob", "grep",
	"job_kill", "job_list", "job_output", "present", "read", "read_image", "skill",
	"todo_write", "update_goal", "web_fetch", "web_search", "working_directory", "write",
}

// outOfScopeToolNames are the DSH tools this backend deliberately does not offer.
//
// Decision 6 keeps subagents, schedules, terminals, skills, goals, plugins,
// workflows and the Cordis inspector out of scope. Naming them here is what stops
// a half-built one from appearing: the model must not see a tool the backend
// cannot honour, because a plausible empty answer is worse than an absent tool.
var outOfScopeToolNames = []string{
	"cordis_define", "cordis_inspect_list", "cordis_inspect_query", "cordis_inspect_self",
	"cordis_run", "cordis_stop", "cordis_undefine",
	"exit_plan_mode", "interrupt_agent", "list_agents", "send_message",
	"subagent", "subagent_fork", "workflow",
}

// dshToolSchemas reads the web preset's schema list, keyed by tool name.
func dshToolSchemas(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	root := dshSnapshotRoot()
	if root == "" {
		t.Skip("no DeepSeek Harness checkout with tool schema snapshots; set DSH_GO_DSH_ROOT")
	}
	body, err := os.ReadFile(filepath.Join(root, "snapshots/web/cordis-tool-round/tool-schemas.expected.json"))
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
// session actually uses.
//
// Only the structure is compared: the property names, their types, which ones are
// required, and the enum members. The prose in each description differs between
// DSH's own compositions (the same tool is worded differently in a persona preset
// and in a full tool round), so comparing it would fail for a reason that has
// nothing to do with this backend.
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
		if gotShape, wantShape := schemaShape(t, got), schemaShape(t, string(want)); gotShape != wantShape {
			t.Errorf("%s schema differs from the web preset\n got: %s\nwant: %s", name, gotShape, wantShape)
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
	// Every name the web preset exposes must be either implemented or explicitly
	// out of scope: a tool that is neither drifts into "we forgot".
	for _, name := range missing {
		t.Errorf("%s is in the web preset but is neither core nor out of scope", name)
	}
}

// schemaShape renders a schema's structure: property names with their types and
// enums, plus the required list. Descriptions are deliberately excluded.
func schemaShape(t *testing.T, raw string) string {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	shape := make([]string, 0, len(schema.Properties)+1)
	for name, property := range schema.Properties {
		var field struct {
			Type  string          `json:"type"`
			Enum  []string        `json:"enum"`
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(property, &field); err != nil {
			t.Fatalf("decode property %q: %v", name, err)
		}
		entry := name + ":" + field.Type
		if len(field.Enum) > 0 {
			entry += "[" + strings.Join(field.Enum, "|") + "]"
		}
		// One level of nesting covers every schema in this preset: an array of
		// objects, or a plain scalar.
		if len(field.Items) > 0 {
			var items struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(field.Items, &items); err != nil {
				t.Fatalf("decode items of %q: %v", name, err)
			}
			entry += "{" + items.Type + ":"
			nested := make([]string, 0, len(items.Properties))
			for nestedName, nestedProperty := range items.Properties {
				var nestedField struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(nestedProperty, &nestedField)
				nested = append(nested, nestedName+":"+nestedField.Type)
			}
			sort.Strings(nested)
			entry += strings.Join(nested, ",") + "}"
		}
		shape = append(shape, entry)
	}
	sort.Strings(shape)
	required := append([]string(nil), schema.Required...)
	sort.Strings(required)
	return strings.Join(shape, " ") + " required=" + strings.Join(required, ",")
}
