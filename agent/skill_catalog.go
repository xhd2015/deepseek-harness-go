package agent

import (
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// The model cannot load a skill it has never heard of.
//
// The catalog is one message at the start of the session: the available names and
// one line each. It is written once — a session that already carries it is not
// given a second copy — because a repeated catalog would grow with every turn
// while saying the same thing.

// skillCatalogEventType marks the message that published the catalog.
//
// The source kind is the marker: the message is an ordinary user message so an
// older reader still shows something, but its source says what it is.
const skillCatalogEventType = "skill-catalog"

// renderSkillCatalog renders the model-facing list, or "" when there is none.
func renderSkillCatalog(skills []SkillDefinition) string {
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		if skill.DisableModelInvocation {
			// A user-only skill is not in the model's namespace at all.
			continue
		}
		names = append(names, skill.Name)
	}
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString("A skill is a reusable set of task-specific instructions. The following skills are available in this session:\n\n")
	b.WriteString("<available_skills>\n")
	for _, skill := range skills {
		if skill.DisableModelInvocation {
			continue
		}
		description := strings.Join(strings.Fields(skill.Description), " ")
		if description == "" {
			description = "No description."
		}
		b.WriteString("- `" + skill.Name + "`: " + description + "\n")
	}
	b.WriteString("</available_skills>\n\n")
	b.WriteString("Load one with the skill tool before acting on a task it covers.\n")
	b.WriteString("</system-reminder>")
	return b.String()
}

// skillCatalogEntries mirrors the rendered lines for non-model consumers.
func skillCatalogEntries(skills []SkillDefinition) []map[string]any {
	entries := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		if skill.DisableModelInvocation {
			continue
		}
		entries = append(entries, map[string]any{"name": skill.Name, "description": skill.Description})
	}
	return entries
}

// skillsFor reports the catalog for one session, honouring its workspace.
//
// A server started in one directory can host sessions in many; resolving once at
// boot would hand every session the skills of the launch directory.
func (r *Runner) skillsFor(sess *session.Session) []SkillDefinition {
	if r.cfg.ResolveSkills != nil && sess != nil {
		if resolved := r.cfg.ResolveSkills(sess.Cwd()); len(resolved) > 0 {
			return resolved
		}
	}
	return r.cfg.Skills
}
