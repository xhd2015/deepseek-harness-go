package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A skill is a Markdown file of instructions the model loads by name.
//
// The model does not read the whole library: the session carries a catalog of
// names and one-line descriptions, and `skill` pulls one body into the context
// when a task matches it. That split is what keeps a large library from spending
// the context it is meant to save.

// SkillSummary is one catalog entry.
type SkillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Path is the file the body lives in, for diagnostics.
	Path string `json:"path,omitempty"`
	// DisableModelInvocation marks a skill only a person may invoke.
	DisableModelInvocation bool `json:"disable_model_invocation,omitempty"`
}

// SkillDefinition is a catalog entry plus its body.
type SkillDefinition struct {
	SkillSummary
	Content string `json:"content"`
	// ResourceBase is the directory the skill's references resolve against.
	ResourceBase string `json:"resource_base,omitempty"`
}

// SkillRoot is one directory searched for skills.
type SkillRoot struct {
	Path string
	// Source names where the root came from, for diagnostics and precedence.
	Source string
}

// skillFileName is the bundle entry point; a flat `<name>.md` file is the other
// shape DSH accepts.
const skillFileName = "SKILL.md"

// SkillRoots resolves the directories to search, most specific first.
//
// The order is the precedence contract, and it is DSH's: a project skill overrides
// a custom root, which overrides the harness home, which overrides the user's
// agents home. Getting this list short would silently hide skills rather than
// fail — the model simply would not know a name exists — so all five sources are
// searched.
func SkillRoots(cwd, dshHome, agentsHome string, extra []string) []SkillRoot {
	roots := make([]SkillRoot, 0, 7)
	add := func(path, source string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		for _, existing := range roots {
			if existing.Path == path {
				return
			}
		}
		roots = append(roots, SkillRoot{Path: path, Source: source})
	}
	if project := findProjectRoot(cwd); project != "" {
		add(filepath.Join(project, ".dsh", "skills"), "project-dsh")
		add(filepath.Join(project, ".agents", "skills"), "project-agents")
	}
	for _, path := range extra {
		add(path, "custom")
	}
	if dshHome != "" {
		add(filepath.Join(dshHome, "skills"), "user-dsh")
	}
	if agentsHome != "" {
		// The user's own skills live here, next to the project's `.agents`:
		// the same convention at the other level.
		add(filepath.Join(agentsHome, "skills"), "user-agents")
	}
	return roots
}

// AgentsHome reports the user-level agents home: $DSH_AGENTS_HOME, else ~/.agents.
//
// It is a separate root from the harness home on purpose: the harness home holds
// one installation's state, while this one holds the person's own skills, shared
// by every checkout they work in.
func AgentsHome() string {
	if home := strings.TrimSpace(os.Getenv("DSH_AGENTS_HOME")); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(userHome, ".agents")
}

// findProjectRoot walks up from cwd looking for a repository marker.
func findProjectRoot(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	current, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	for {
		for _, marker := range []string{".git", ".dsh", ".agents"} {
			if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// LoadSkills reads every skill under the given roots, first occurrence winning.
func LoadSkills(roots []SkillRoot) ([]SkillDefinition, error) {
	seen := map[string]bool{}
	var skills []SkillDefinition
	for _, root := range roots {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			// A missing root is ordinary: most projects have no skills of their own.
			continue
		}
		for _, entry := range entries {
			path, ok := skillPathOf(root.Path, entry)
			if !ok {
				continue
			}
			skill, err := readSkill(path)
			if err != nil || skill.Name == "" || seen[skill.Name] {
				continue
			}
			seen[skill.Name] = true
			skills = append(skills, skill)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// skillPathOf resolves one directory entry to a skill file, if it is one.
func skillPathOf(root string, entry os.DirEntry) (string, bool) {
	if entry.IsDir() {
		path := filepath.Join(root, entry.Name(), skillFileName)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
		return "", false
	}
	if strings.HasSuffix(entry.Name(), ".md") && entry.Name() != skillFileName {
		return filepath.Join(root, entry.Name()), true
	}
	return "", false
}

// readSkill parses one skill file: YAML frontmatter then the body.
func readSkill(path string) (SkillDefinition, error) {
	file, err := os.Open(path)
	if err != nil {
		return SkillDefinition{}, err
	}
	defer file.Close()

	var frontmatter []string
	var body []string
	inFrontmatter := false
	closed := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			first = false
			if strings.TrimSpace(line) == "---" {
				inFrontmatter = true
				continue
			}
		}
		if inFrontmatter {
			if strings.TrimSpace(line) == "---" {
				inFrontmatter = false
				closed = true
				continue
			}
			frontmatter = append(frontmatter, line)
			continue
		}
		body = append(body, line)
	}
	if err := scanner.Err(); err != nil {
		return SkillDefinition{}, fmt.Errorf("%s: %w", path, err)
	}
	if !closed {
		// No frontmatter: the file is a flat skill whose name is its basename.
		body = frontmatter
		frontmatter = nil
	}

	skill := SkillDefinition{
		SkillSummary: SkillSummary{Path: path},
		Content:      strings.Join(body, "\n"),
		ResourceBase: filepath.Dir(path),
	}
	for _, line := range frontmatter {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "name":
			skill.Name = value
		case "description":
			skill.Description = value
		case "disable-model-invocation":
			skill.DisableModelInvocation = value == "true"
		}
	}
	if skill.Name == "" {
		base := filepath.Base(path)
		if base == skillFileName {
			base = filepath.Base(filepath.Dir(path))
		} else {
			base = strings.TrimSuffix(base, ".md")
		}
		skill.Name = base
	}
	skill.Content = strings.TrimSpace(skill.Content)
	return skill, nil
}

// FindSkill resolves one skill by exact name.
func FindSkill(skills []SkillDefinition, name string) (SkillDefinition, bool) {
	for _, skill := range skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return SkillDefinition{}, false
}

// RenderSkillContent frames one skill body the way DSH frames it.
//
// The framing is not cosmetic: it tells the model where the instructions end and
// where the referenced files live, and a body pasted without it reads as if the
// conversation itself had changed subject.
func RenderSkillContent(skill SkillDefinition) string {
	resources := []string{"Resources for this skill are managed by its provider."}
	if skill.ResourceBase != "" {
		resources = []string{"Referenced files resolve against: " + skill.ResourceBase, "Load referenced resources only as needed."}
	}
	return strings.Join([]string{
		`<skill_content name="` + skill.Name + `">`,
		"<skill_resources>",
		strings.Join(resources, "\n"),
		"</skill_resources>",
		"",
		"<skill_instructions>",
		skill.Content,
		"</skill_instructions>",
		"</skill_content>",
	}, "\n")
}
