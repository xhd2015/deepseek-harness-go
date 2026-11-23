package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill plants one skill file in a root.
func writeSkill(t *testing.T, root, dir, frontmatter, body string) {
	t.Helper()
	target := filepath.Join(root, dir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\n" + frontmatter + "---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(target, skillFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestSkillLoadsItsInstructions is the acceptance criterion: a model names a real
// skill and gets its body, not a no-op.
func TestSkillLoadsItsInstructions(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "brainstorm", "name: brainstorm\ndescription: Plan before building.\n",
		"# Brainstorm\n\nAsk before implementing.")
	writeSkill(t, root, "office", "name: office\ndescription: Office files.\n", "Office body.")

	skills, err := LoadSkills([]SkillRoot{{Path: root, Source: "test"}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("loaded %d skills: %v", len(skills), skills)
	}
	// A flat `<name>.md` is the other accepted shape.
	flat := filepath.Join(root, "flat.md")
	if err := os.WriteFile(flat, []byte("# Flat\n\nno frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills, err = LoadSkills([]SkillRoot{{Path: root, Source: "test"}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := FindSkill(skills, "flat"); !ok {
		t.Fatalf("a flat skill was not named after its file: %v", namesOf(skills))
	}

	executor := NewToolExecutor(root)
	executor.SetSkills(skills)
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "skill-1"}, "skill", map[string]any{
		"name": "brainstorm",
	})
	if isError {
		t.Fatalf("skill failed: %s", output)
	}
	// The framing is what tells the model where the instructions begin.
	for _, want := range []string{"<skill_content name=\"brainstorm\">", "<skill_instructions>", "Ask before implementing."} {
		if !strings.Contains(output, want) {
			t.Fatalf("output is missing %q: %s", want, output)
		}
	}
}

// TestSkillNamesTheMissingName keeps a typo from reading as success.
func TestSkillNamesTheMissingName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "brainstorm", "name: brainstorm\ndescription: Plan.\n", "body")
	skills, err := LoadSkills([]SkillRoot{{Path: root, Source: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewToolExecutor(root)
	executor.SetSkills(skills)
	output, isError := callTool(t, executor, ToolInvocation{SessionID: "skill-2"}, "skill", map[string]any{
		"name": "brainstrm",
	})
	if !isError {
		t.Fatalf("an unknown skill was accepted: %s", output)
	}
	if !strings.Contains(output, "brainstrm") || !strings.Contains(output, "brainstorm") {
		t.Fatalf("the error neither names the typo nor lists the catalog: %s", output)
	}
}

// TestSkillRefusesUserOnlySkills respects the invocation policy in the file.
func TestSkillRefusesUserOnlySkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "secret", "name: secret\ndescription: x\ndisable-model-invocation: true\n", "body")
	skills, err := LoadSkills([]SkillRoot{{Path: root, Source: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewToolExecutor(root)
	executor.SetSkills(skills)
	if _, isError := callTool(t, executor, ToolInvocation{SessionID: "s"}, "skill", map[string]any{"name": "secret"}); !isError {
		t.Fatal("a user-only skill was loaded for the model")
	}
}

// TestUserAgentsSkillsAreSearched is the gap the verification agent found: the
// person's own skills live in ~/.agents/skills, and a backend that never looks
// there answers "unknown skill" for names that plainly exist.
func TestUserAgentsSkillsAreSearched(t *testing.T) {
	agentsHome := t.TempDir()
	writeSkill(t, filepath.Join(agentsHome, "skills"), "brainstorm", "name: brainstorm\ndescription: Plan first.\n", "Ask before building.")

	roots := SkillRoots(t.TempDir(), t.TempDir(), agentsHome, nil)
	var sources []string
	for _, root := range roots {
		sources = append(sources, root.Source)
	}
	if !strings.Contains(strings.Join(sources, ","), "user-agents") {
		t.Fatalf("sources = %v, want the user agents root among them", sources)
	}
	skills, err := LoadSkills(roots)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	skill, ok := FindSkill(skills, "brainstorm")
	if !ok {
		t.Fatalf("a user-level skill was not found; loaded %v", namesOf(skills))
	}
	if skill.Description != "Plan first." {
		t.Fatalf("skill = %+v", skill)
	}
}

// TestAgentsHomeHonoursTheEnvironment keeps the root overridable for a deployment
// that keeps agents state elsewhere.
func TestAgentsHomeHonoursTheEnvironment(t *testing.T) {
	t.Setenv("DSH_AGENTS_HOME", "/opt/agents")
	if got := AgentsHome(); got != "/opt/agents" {
		t.Fatalf("AgentsHome = %q", got)
	}
}

// TestProjectSkillWinsOverUserSkill pins the precedence the roots encode.
func TestProjectSkillWinsOverUserSkill(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	writeSkill(t, project, "shared", "name: shared\ndescription: project.\n", "project body")
	writeSkill(t, user, "shared", "name: shared\ndescription: user.\n", "user body")
	skills, err := LoadSkills([]SkillRoot{
		{Path: project, Source: "project-dsh"},
		{Path: user, Source: "user-dsh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := FindSkill(skills, "shared")
	if !ok || skill.Description != "project." {
		t.Fatalf("the user skill won: %+v", skill)
	}
}

// namesOf renders catalog names for a failure message.
func namesOf(skills []SkillDefinition) []string {
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		names = append(names, skill.Name)
	}
	return names
}

var _ = context.Background
