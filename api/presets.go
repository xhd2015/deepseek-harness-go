package api

import (
	"encoding/json"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// agentPresetsList answers agentPresets/list.
//
// The roster reports what this backend can actually run. Each row carries an empty
// `plugins` array and says in its description that the composition is fixed: a
// client that renders the field will show "no plugins", which is true, instead of a
// fabricated bundle list.
func (s *Service) agentPresetsList(json.RawMessage) (any, error) {
	presets := agent.Presets()
	rows := make([]map[string]any, 0, len(presets))
	for _, preset := range presets {
		rows = append(rows, map[string]any{
			"id":          preset.ID,
			"name":        preset.Name,
			"description": preset.Description,
			"order":       preset.Order,
			"isDefault":   preset.ID == agent.DefaultPresetID,
			// Empty on purpose: this backend does not compose presets from plugins.
			"plugins": []any{},
		})
	}
	return map[string]any{"presets": rows}, nil
}

// agentPresetsRead answers agentPresets/read.
func (s *Service) agentPresetsRead(args json.RawMessage) (any, error) {
	var request struct {
		AgentPreset string `json:"agentPreset"`
		Preset      string `json:"preset"`
	}
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	id := strings.TrimSpace(request.AgentPreset)
	if id == "" {
		id = strings.TrimSpace(request.Preset)
	}
	preset, ok := agent.PresetByID(id)
	if !ok {
		return nil, fail(CodeNotFound, "unknown agent preset: "+id)
	}
	document := map[string]any{
		"id":          preset.ID,
		"name":        preset.Name,
		"description": preset.Description,
		"order":       preset.Order,
		"plugins":     []any{},
		"tools":       presetToolNames(preset),
	}
	if preset.Prompt != "" {
		document["prompt"] = preset.Prompt
	}
	return document, nil
}

// presetToolNames reports the tools a preset offers, for the read view.
func presetToolNames(preset agent.PresetDefinition) []string {
	if preset.Tools != nil {
		return preset.Tools
	}
	names := make([]string, 0, 20)
	for _, definition := range agent.NewToolExecutor("").Definitions() {
		names = append(names, definition.Name)
	}
	return names
}

// agentPresetsSelect answers agentPresets/select.
//
// The choice is recorded and takes effect on the next request. It is refused while
// a turn is running: the tool list for an in-flight step is already fixed, and
// switching mid-step would send a request that disagrees with the one before it.
func (s *Service) agentPresetsSelect(args json.RawMessage) (any, error) {
	var request struct {
		SessionID   string `json:"sessionId"`
		AgentPreset string `json:"agentPreset"`
		Preset      string `json:"preset"`
	}
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	id := strings.TrimSpace(request.AgentPreset)
	if id == "" {
		id = strings.TrimSpace(request.Preset)
	}
	preset, ok := agent.PresetByID(id)
	if !ok {
		return nil, fail(CodeBadRequest, "unknown agent preset: "+id)
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	if s.cfg.Runner != nil && s.cfg.Runner.Busy(opened.ID()) {
		return nil, fail(CodeBusy, "session is running; wait for the current turn before switching presets")
	}
	if _, err := opened.Append(session.EventAgentPreset, map[string]any{"agentPreset": preset.ID}); err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return map[string]any{"agentPreset": preset.ID, "preset": preset.ID}, nil
}
