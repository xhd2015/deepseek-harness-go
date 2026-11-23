package session

import "encoding/json"

// Permission state is three knobs that always travel together.
//
// DSH's presets bundle them — `workspace-write` means the workspace-write
// sandbox *and* the `ask` approval policy — so a session that recorded only one
// of the three would restore into a state no preset describes. They are recorded
// as separate events because that is what DSH writes, and folded together here.

// Sandbox mode names, ordered from narrowest to widest.
const (
	SandboxReadOnly       = "read-only"
	SandboxWorkspaceWrite = "workspace-write"
	SandboxDangerFull     = "danger-full-access"
)

// Approval policies.
const (
	ApprovalAsk   = "ask"
	ApprovalNever = "never"
)

// Preset names, as DSH spells them.
const (
	PresetReadOnly       = "read-only"
	PresetWorkspaceWrite = "workspace-write"
	PresetDangerFull     = "danger-full-access"
	// PresetCustom is the derived state when the three knobs match no preset.
	PresetCustom = "custom"
)

// PermissionState is the folded view of one session's permission knobs.
type PermissionState struct {
	Preset   string
	Sandbox  string
	Approval string
	// Recorded is whether the log carried any of the three events. A session that
	// recorded none predates this backend's permission support.
	Recorded bool
}

// DefaultPermission is where a new session starts: DSH's web/base bundle composes
// `workspace-write` + `ask` as the default preset, so starting anywhere wider
// would be a behavioural drift in the unsafe direction.
func DefaultPermission() PermissionState {
	return PermissionState{
		Preset:   PresetWorkspaceWrite,
		Sandbox:  SandboxWorkspaceWrite,
		Approval: ApprovalAsk,
	}
}

// PresetSpec is one row of the preset table.
type PresetSpec struct {
	Name        string `json:"name"`
	Sandbox     string `json:"sandbox"`
	Approval    string `json:"approval"`
	Description string `json:"description"`
}

// PermissionPresets is the preset table, matching DSH's default configuration.
func PermissionPresets() []PresetSpec {
	return []PresetSpec{
		{
			Name:        PresetReadOnly,
			Sandbox:     SandboxReadOnly,
			Approval:    ApprovalAsk,
			Description: "Read files and run read-only commands; any write requires approval.",
		},
		{
			Name:        PresetWorkspaceWrite,
			Sandbox:     SandboxWorkspaceWrite,
			Approval:    ApprovalAsk,
			Description: "Write inside the workspace and permitted temporary directories; wider retries require approval.",
		},
		{
			Name:        PresetDangerFull,
			Sandbox:     SandboxDangerFull,
			Approval:    ApprovalNever,
			Description: "Full file access without approval prompts.",
		},
	}
}

// PresetNamed resolves one preset, reporting whether it exists.
func PresetNamed(name string) (PresetSpec, bool) {
	for _, preset := range PermissionPresets() {
		if preset.Name == name {
			return preset, true
		}
	}
	return PresetSpec{}, false
}

// DerivePreset names the preset whose knobs match, or `custom` when none does.
//
// The derived name matters because the UI shows it: a session whose knobs came
// from separate writes may match no preset, and calling it by the nearest name
// would claim a bundle the session is not in.
func DerivePreset(sandbox, approval string) string {
	for _, preset := range PermissionPresets() {
		if preset.Sandbox == sandbox && preset.Approval == approval {
			return preset.Name
		}
	}
	return PresetCustom
}

// FoldPermission replays the three permission events.
//
// Each knob is read independently because that is how they are stored: a preset
// switch writes all three, but a session written by another build may carry only
// some of them, and the missing ones keep the default rather than inventing a
// state the log never recorded.
func FoldPermission(events []Event) PermissionState {
	state := DefaultPermission()
	for _, event := range events {
		switch event.Type {
		case EventPermissionPreset:
			var payload struct {
				Preset string `json:"preset"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Preset != "" {
				state.Recorded = true
				// A named preset is authoritative for all three knobs: it is a
				// bundle, and honouring only its name would leave the sandbox on an
				// older mode the name contradicts.
				if preset, ok := PresetNamed(payload.Preset); ok {
					state.Preset = preset.Name
					state.Sandbox = preset.Sandbox
					state.Approval = preset.Approval
				} else {
					state.Preset = payload.Preset
				}
			}
		case EventSandboxMode:
			var payload struct {
				Mode string `json:"mode"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Mode != "" {
				state.Recorded = true
				state.Sandbox = payload.Mode
			}
		case EventApprovalPolicy:
			var payload struct {
				Policy string `json:"policy"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Policy != "" {
				state.Recorded = true
				state.Approval = payload.Policy
			}
		}
	}
	// The knobs are the truth; the name is derived from them so a session that
	// received separate writes still reports the matching preset (or `custom`).
	state.Preset = DerivePreset(state.Sandbox, state.Approval)
	if named := presetNameFromEvents(events, state); named != "" {
		state.Preset = named
	}
	return state
}

// presetNameFromEvents keeps an explicitly recorded preset name when the knobs
// still match it; a name that its own knobs contradict is dropped for `custom`.
func presetNameFromEvents(events []Event, state PermissionState) string {
	named := ""
	for _, event := range events {
		if event.Type != EventPermissionPreset {
			continue
		}
		var payload struct {
			Preset string `json:"preset"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && payload.Preset != "" {
			named = payload.Preset
		}
	}
	if named == "" {
		return ""
	}
	if preset, ok := PresetNamed(named); ok {
		if preset.Sandbox == state.Sandbox && preset.Approval == state.Approval {
			return preset.Name
		}
		return PresetCustom
	}
	return named
}

// AllowsSandbox reports whether a call may run under the session's sandbox mode.
//
// The ordering is DSH's: a request may only widen, and only by naming a mode this
// one does not already permit.
func AllowsSandbox(requested, effective string) bool {
	return sandboxRank(requested) > sandboxRank(effective)
}

// sandboxRank orders the modes from narrowest to widest.
func sandboxRank(mode string) int {
	switch mode {
	case SandboxReadOnly:
		return 0
	case SandboxWorkspaceWrite:
		return 1
	case SandboxDangerFull:
		return 2
	default:
		return -1
	}
}
