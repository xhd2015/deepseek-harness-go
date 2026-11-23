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

// PublishApproval sends the question to the page and returns immediately.
//
// The wait belongs to the tool call, which is blocked on the runner's approval
// gate. Waiting here as well is what made an answer arrive with nothing reading it:
// the page showed no pending item and the caller sat until the window expired.
//
// The approval id, not the waterfall id, names the audit pair. `$events/result`
// carries the waterfall id, so the mapping is kept explicitly.
func (s *Service) PublishApproval(request agent.ApprovalRequest) {
	eventID, err := s.publishWaterfall("approval/request", request.Session, map[string]any{
		"toolName": request.ToolName,
		"callId":   request.CallID,
		"reason":   request.Reason,
		// The approval id travels with the request as well, so a page that keeps the
		// id it rendered can answer without depending on the frame correlation.
		"id": request.ID,
	})
	if err != nil {
		s.logf("publish approval: %v", err)
		return
	}
	s.approvalFrames.track(eventID, request.ID, request.Session)
}

// WithdrawApproval tells the page the question is over.
//
// Every exit from the wait passes here: a timeout, a cancellation, or a decision
// delivered by another answerer. A page that learned only about questions would
// keep offering a button for an answer already on disk.
func (s *Service) WithdrawApproval(request agent.ApprovalRequest, outcome agent.ApprovalOutcome) {
	s.approvalFrames.mu.Lock()
	eventID := ""
	for frame, entry := range s.approvalFrames.byEvent {
		if entry.approvalID == request.ID {
			eventID = frame
			break
		}
	}
	s.approvalFrames.mu.Unlock()
	if eventID == "" {
		return
	}
	s.approvalFrames.take(eventID)
	s.withdrawWaterfall(eventID)
}

// settleApprovalFrame delivers a page's answer to the approval it asked about.
func (s *Service) settleApprovalFrame(eventID string, value json.RawMessage) bool {
	approvalID, ok := s.approvalFrames.take(eventID)
	if !ok {
		return false
	}
	if s.cfg.Runner != nil {
		s.cfg.Runner.AnswerApproval(approvalID, outcomeOf(value))
	}
	return true
}

// outcomeOf reads the outcome a page returned, in either accepted spelling.
func outcomeOf(value json.RawMessage) agent.ApprovalOutcome {
	var outcome string
	if len(value) > 0 {
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
