package agent

import (
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// An agent preset is a named bundle of tools and instructions.
//
// DSH composes one from cordis plugin entries. This backend does not load plugins,
// so its presets are built in: each one is a tool-name subset plus a prompt. That
// is a real restriction and it is reported as one — the `plugins` field a client
// reads back is empty, and the preset's own description says the composition is
// fixed.

// PresetDefinition is one selectable preset.
type PresetDefinition struct {
	ID          string
	Name        string
	Description string
	Order       int
	// Tools is the tool-name allowlist; nil means every tool the executor offers.
	Tools []string
	// Prompt replaces the default instructions when non-empty.
	Prompt string
}

// builtinPresets is the preset table.
//
// `standard` mirrors what this backend has always offered. `minimal` exists so a
// preset switch is visibly effective: it drops every tool that can change the
// world beyond reading and editing one file, and it shortens the prompt. `creator`
// uses DSH's own Creator persona verbatim rather than an invented one.
func builtinPresets() []PresetDefinition {
	return []PresetDefinition{
		{
			ID:          "standard",
			Name:        "Standard",
			Description: "The full tool set and the coding-agent instructions.",
			Order:       10,
		},
		{
			ID:          "minimal",
			Name:        "Minimal",
			Description: "Read and edit files only; no shell, no jobs, no web.",
			Order:       20,
			Tools:       []string{"read", "write", "edit", "glob", "grep", "todo_write"},
			Prompt: "You are a helpful coding assistant with a deliberately small tool set: " +
				"read, write, edit, glob, grep and todo_write. " +
				"You cannot run shell commands or reach the web in this preset; work from what you can read " +
				"and say plainly when a task needs a capability you do not have.",
		},
		{
			ID:          "creator",
			Name:        "Creator",
			Description: "Customize DeepSeek Harness itself, following DSH's Creator persona.",
			Order:       30,
			Prompt:      creatorPrompt,
		},
	}
}

// creatorPrompt is DSH's Creator persona, reproduced from the shipped snapshot
// (snapshots/web/creator-persona/system-prompt.expected.md) rather than written
// here: an invented persona would be this backend's opinion, not Creator.
const creatorPrompt = "You are an AI agent powered by DeepSeek Harness.\n\n" +
	"You are a coding agent. You help the user customize DeepSeek Harness itself.\n\n" +
	"Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; " +
	"absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its " +
	"contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have " +
	"inspected it before reading. @\"...\" quotes a path containing spaces.\n\n" +
	"Check the [exit code: N] marker on every bash result; investigate failures before moving on."

// Presets lists the built-in presets in declaration order.
func Presets() []PresetDefinition { return builtinPresets() }

// PresetByID resolves one preset.
func PresetByID(id string) (PresetDefinition, bool) {
	for _, preset := range builtinPresets() {
		if preset.ID == id {
			return preset, true
		}
	}
	return PresetDefinition{}, false
}

// DefaultPresetID is the preset a new session starts on.
const DefaultPresetID = "standard"

// toolsFor filters the executor's definitions down to one preset's allowlist.
//
// The filter is by name and keeps the executor's order, so a preset changes which
// tools are offered without changing how the rest are described.
func (e *ToolExecutor) toolsFor(presetID string) []llm.ToolDef {
	definitions := e.Definitions()
	preset, ok := PresetByID(presetID)
	if !ok || preset.Tools == nil {
		return definitions
	}
	allowed := make(map[string]bool, len(preset.Tools))
	for _, name := range preset.Tools {
		allowed[name] = true
	}
	filtered := make([]llm.ToolDef, 0, len(preset.Tools))
	for _, definition := range definitions {
		if allowed[definition.Name] {
			filtered = append(filtered, definition)
		}
	}
	return filtered
}

// promptFor reports the instructions one preset uses.
//
// The preset is read from the session header, which is where a session records the
// composition it was created under. A session naming a preset this build no longer
// ships keeps the default instructions rather than an empty prompt.
func (r *Runner) promptFor(sess *session.Session) string {
	if sess != nil {
		if preset, ok := PresetByID(sess.Header().AgentPreset); ok && preset.Prompt != "" {
			return preset.Prompt
		}
	}
	return r.cfg.SystemPrompt
}
