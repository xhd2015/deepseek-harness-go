package agent

import (
	"context"
	"encoding/json"
	"strings"
)

// skill loads one skill's instructions into the conversation.
//
// The session carries a catalog of names and descriptions; this tool pulls the
// body of one. It never answers "not found" silently: a model that asked for
// instructions and got nothing would proceed as if it had read them.

// skillToolName is the DSH tool name.
const skillToolName = "skill"

// skillDescription is the DSH wording, verbatim.
const skillDescription = "Load the full instructions for a skill. Call it before acting on a task that names or clearly matches a skill in the session skill catalog."

// skillResult is the tool result: the skill's identity and its instructions.
type skillResult struct {
	Name     string          `json:"name"`
	Provider string          `json:"provider"`
	Content  string          `json:"content"`
	Resource json.RawMessage `json:"resourceBase,omitempty"`
}

// skill loads and frames one skill.
func (e *ToolExecutor) skill(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	_ = ctx
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolOutcome{Text: "name is required", IsError: true}
	}
	definition, ok := FindSkill(e.skills, name)
	if !ok {
		// Listing the names is what lets the model retry with a real one instead
		// of guessing again.
		return ToolOutcome{
			Text:    "skill " + `"` + name + `"` + " is unknown or no longer available. Available skills: " + strings.Join(skillNames(e.skills), ", "),
			IsError: true,
		}
	}
	if definition.DisableModelInvocation {
		return ToolOutcome{Text: "skill " + `"` + name + `"` + " is not available for model invocation", IsError: true}
	}
	resource := map[string]any{"kind": "directory", "path": definition.ResourceBase}
	encodedResource, err := json.Marshal(resource)
	if err != nil {
		return ToolOutcome{Text: "encode resource base: " + err.Error(), IsError: true}
	}
	encoded, err := json.Marshal(skillResult{
		Name:     definition.Name,
		Provider: "filesystem",
		Content:  definition.Content,
		Resource: encodedResource,
	})
	if err != nil {
		return ToolOutcome{Text: "encode result: " + err.Error(), IsError: true}
	}
	// The model reads the framed form, so the instructions arrive exactly where
	// the conversation says they do.
	return ToolOutcome{
		Text:    RenderSkillContent(definition),
		Meta:    map[string]any{"skill": definition.Name},
		Content: nil,
		Present: json.RawMessage(encoded),
	}
}

// skillNames lists catalog names for an error message.
func skillNames(skills []SkillDefinition) []string {
	names := make([]string, 0, len(skills))
	for _, skill := range skills {
		if skill.DisableModelInvocation {
			continue
		}
		names = append(names, skill.Name)
	}
	return names
}
