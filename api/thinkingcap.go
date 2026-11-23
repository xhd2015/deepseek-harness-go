package api

import (
	"encoding/json"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// setThinkingCapAutoContinue answers session/setThinkingCapAutoContinue.
//
// The policy is durable because a person turns it on once and expects it to stay:
// re-arming it after every reload would make the toggle feel broken. When the
// requested value already holds, nothing is written and the caller gets the seq of
// the event that established it, so the reply is honest about what changed.
func (s *Service) setThinkingCapAutoContinue(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Enabled   bool   `json:"enabled"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return nil, fail(CodeBadRequest, "sessionId is required")
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	events := opened.Events()
	current := session.FoldThinkingCapAutoContinue(events).Enabled
	if current == request.Enabled {
		seq := opened.LastSeq()
		if seq < 0 {
			seq = 0
		}
		return map[string]any{"enabled": request.Enabled, "seq": seq}, nil
	}
	event, err := opened.AppendIgnorable(session.ThinkingCapAutoContinueEvent, map[string]any{
		"enabled": request.Enabled,
	})
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return map[string]any{"enabled": request.Enabled, "seq": event.Seq}, nil
}
