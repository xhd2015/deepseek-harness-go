package api

import (
	"encoding/json"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// The permission knobs are read and written through DSH's own two methods, so the
// access-mode control in the page is the same control that works against a native
// host. Inventing a different name would leave that control dead.

// permissionPresetsCatalog answers permissionPresets/catalog.
func (s *Service) permissionPresetsCatalog(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	_ = decodeEitherShape(args, &request)

	options := make([]map[string]any, 0, len(session.PermissionPresets()))
	for _, preset := range session.PermissionPresets() {
		options = append(options, map[string]any{
			"value":       preset.Name,
			"name":        preset.Name,
			"description": preset.Description,
			"sandbox":     preset.Sandbox,
			"approval":    preset.Approval,
		})
	}
	defaults := make([]string, 0, len(session.PermissionPresets()))
	for _, preset := range session.PermissionPresets() {
		defaults = append(defaults, preset.Name)
	}
	current := session.DefaultPermission().Preset
	if request.SessionID != "" {
		if opened, err := s.openSession(request.SessionID); err == nil {
			current = session.FoldPermission(opened.Events()).Preset
		}
	}
	return map[string]any{
		"options":        options,
		"defaultOptions": defaults,
		"defaultPreset":  session.DefaultPermission().Preset,
		"currentValue":   current,
	}, nil
}

// permissionPresetsSet answers permissionPresets/set.
//
// All three records are written because a preset is a bundle: writing only the
// name would leave the sandbox on the previous mode, which is exactly the state the
// user asked to leave. The reply reports the selection the way the page renders it.
func (s *Service) permissionPresetsSet(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Preset    string `json:"preset"`
		Value     string `json:"value"`
	}
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	name := strings.TrimSpace(request.Preset)
	if name == "" {
		name = strings.TrimSpace(request.Value)
	}
	if name == "" {
		return nil, fail(CodeBadRequest, "a permission preset name is required")
	}
	preset, ok := session.PresetNamed(name)
	if !ok {
		return nil, fail(CodeBadRequest, "unknown permission preset \""+name+"\"")
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	if err := applyPreset(opened, preset); err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return map[string]any{
		"currentValue": preset.Name,
		"preset":       preset.Name,
		"sandbox":      preset.Sandbox,
		"approval":     preset.Approval,
	}, nil
}

// applyPreset writes the three permission records for one preset.
func applyPreset(opened *session.Session, preset session.PresetSpec) error {
	if _, err := opened.Append(session.EventPermissionPreset, map[string]any{"preset": preset.Name}); err != nil {
		return err
	}
	if _, err := opened.Append(session.EventSandboxMode, map[string]any{"mode": preset.Sandbox}); err != nil {
		return err
	}
	if _, err := opened.Append(session.EventApprovalPolicy, map[string]any{"policy": preset.Approval}); err != nil {
		return err
	}
	return nil
}

// answerApproval answers approval/answer: the page's decision on one question.
//
// The outcome is delivered, not recorded here: the waiting tool call writes the
// matching `approval/decided` so the pair stays turn-enclosed and in order.
func (s *Service) answerApproval(args json.RawMessage) (any, error) {
	var request struct {
		ID         string `json:"id"`
		ApprovalID string `json:"approvalId"`
		Outcome    string `json:"outcome"`
	}
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	id := strings.TrimSpace(request.ID)
	if id == "" {
		id = strings.TrimSpace(request.ApprovalID)
	}
	if id == "" {
		return nil, fail(CodeBadRequest, "an approval id is required")
	}
	outcome := strings.TrimSpace(request.Outcome)
	if outcome == "" {
		return nil, fail(CodeBadRequest, "an approval outcome is required")
	}
	if s.cfg.Runner == nil {
		return nil, fail(CodeUnimplemented, "approvals need a runner")
	}
	if !s.cfg.Runner.AnswerApproval(id, agent.ApprovalOutcome(outcome)) {
		// No such question: it was already settled, or it never existed. Reporting
		// success would let a stale page believe it changed a decision.
		return nil, fail(CodeNotFound, "no approval is waiting for "+id)
	}
	return map[string]any{"id": id, "outcome": outcome}, nil
}

// pendingApprovals answers approval/pending, for a page that just reconnected.
func (s *Service) pendingApprovals(args json.RawMessage) (any, error) {
	if s.cfg.Runner == nil {
		return map[string]any{"approvals": []any{}}, nil
	}
	requests := s.cfg.Runner.PendingApprovals()
	return map[string]any{"approvals": requests}, nil
}

// requestApprovalOverWaterfall asks the page through the waterfall protocol.
//
// This is the shape the original UI answers: a `waterfall` frame the client's
// `approval/request` listener receives, and a `$events/result` reply. A plain
// `emit` would show nothing clickable, which is how the first attempt failed.
func (s *Service) requestApprovalOverWaterfall(request agent.ApprovalRequest) agent.ApprovalOutcome {
	value, answered := s.requestWaterfall("approval/request", request.Session, map[string]any{
		"toolName": request.ToolName,
		"callId":   request.CallID,
		"reason":   request.Reason,
	})
	if !answered {
		// Nobody decided. Fail closed: a turn must not hang on a page that is gone.
		return agent.ApprovalUnavailable
	}
	var outcome string
	if len(value) > 0 {
		// The listener returns the outcome as a JSON string or as an object with an
		// `outcome` field; accept both, since both spellings appear in the protocol.
		if err := json.Unmarshal(value, &outcome); err != nil {
			var wrapped struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(value, &wrapped) == nil {
				outcome = wrapped.Outcome
			}
		}
	}
	switch agent.ApprovalOutcome(outcome) {
	case agent.ApprovalAllowedOnce, agent.ApprovalRejected, agent.ApprovalCancelled, agent.ApprovalUnavailable:
		return agent.ApprovalOutcome(outcome)
	default:
		return agent.ApprovalUnavailable
	}
}

// RequestApprovalOverWaterfall asks the page and reports its decision.
//
// The caller delivers the outcome back through the runner, which unblocks the tool
// call; the tool then writes the matching `approval/decided`. Splitting it here
// keeps the audit pair in the order it happened.
func (s *Service) RequestApprovalOverWaterfall(request agent.ApprovalRequest) agent.ApprovalOutcome {
	return s.requestApprovalOverWaterfall(request)
}

// Runner exposes the runner this service drives, for the approval round trip.
func (s *Service) Runner() *agent.Runner { return s.cfg.Runner }
