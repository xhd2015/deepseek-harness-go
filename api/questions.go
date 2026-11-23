package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// answerQuestion answers an ask_user_question call.
//
// The call that asked has already returned, so the answer cannot be its tool
// result: the model learns it from a user message that continues the turn. DSH
// records that message with a `user-question-reply` source naming the call, which
// is also what lets the page show who answered what instead of a message the
// person appears to have typed unprompted.
func (s *Service) answerQuestion(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		// Session is the older spelling for the same value.
		Session string `json:"session"`
		CallID  string `json:"callId"`
		Answer  struct {
			Answers []struct {
				ID       string   `json:"id"`
				Selected []string `json:"selected"`
				Custom   string   `json:"custom"`
			} `json:"answers"`
		} `json:"answer"`
	}
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	sessionID := strings.TrimSpace(request.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(request.Session)
	}
	callID := strings.TrimSpace(request.CallID)
	if sessionID == "" {
		return nil, fail(CodeBadRequest, "sessionId is required")
	}
	if callID == "" {
		return nil, fail(CodeBadRequest, "callId is required")
	}

	batch := agent.QuestionAnswerBatch{Answers: make([]agent.QuestionAnswer, 0, len(request.Answer.Answers))}
	for _, answer := range request.Answer.Answers {
		selected := answer.Selected
		if selected == nil {
			// An empty selection is a real answer (the user skipped), and the model
			// must read it as an empty array rather than as a missing field.
			selected = []string{}
		}
		batch.Answers = append(batch.Answers, agent.QuestionAnswer{
			ID:       strings.TrimSpace(answer.ID),
			Selected: selected,
			Custom:   answer.Custom,
		})
	}

	if err := agent.AnswerQuestion(sessionID, callID, batch); err != nil {
		return nil, fail(CodeBusy, err.Error())
	}
	// The call returned as soon as the question was recorded, so the answer is
	// always a late one: it becomes a message that continues the conversation.
	if err := s.continueWithAnswer(sessionID, callID, batch); err != nil {
		return nil, err
	}
	return true, nil
}

// continueWithAnswer records the late answer as a user message and runs the turn
// it belongs to.
//
// The message is the record: reopening this session must show that the person
// answered, so it is written before the turn runs. The reply is a continuation,
// not a new prompt — the session already has the pending tool result, and adding
// another user message as if the person had typed it unprompted would lose the
// fact that it answers a specific call.
func (s *Service) continueWithAnswer(sessionID, callID string, batch agent.QuestionAnswerBatch) error {
	if s.cfg.Runner == nil {
		return fail(CodeUnimplemented, "continuing a turn is not configured")
	}
	opened, err := s.openSession(sessionID)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"kind":    "answer_to_pending_question",
		"tool":    "ask_user_question",
		"callId":  callID,
		"answers": batch.Answers,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fail(CodeInternal, "encode answer: "+err.Error())
	}
	if _, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(encoded)}},
		"source": map[string]any{
			"kind":    "user-question-reply",
			"callId":  callID,
			"outcome": "answered",
		},
		"role": "user",
		"id":   session.NewID(),
	}); err != nil {
		return fail(CodeInternal, "record answer: "+err.Error())
	}
	started := s.cfg.Runner.Start(context.Background(), agent.PromptRequest{
		Session: opened,
		// The reply is already recorded above, so the turn must not append a
		// second user message for it.
		Continuation: true,
	})
	if !started {
		// The session was already running a turn. The message is durable, so the
		// next turn reads it; reporting a failure would make the user submit again
		// and duplicate their answer.
		return nil
	}
	return nil
}

// decodeEitherShape accepts both request shapes the same page uses.
//
// Most methods wrap their arguments in {"request": {...}}. This one carries three
// named parameters, which the browser sends flat. Accepting only one shape would
// make the answer path fail with a message about envelopes instead of about the
// answer.
func decodeEitherShape(args json.RawMessage, target any) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(args, &envelope); err != nil {
		return fmt.Errorf("request args are not an object: %w", err)
	}
	if raw, ok := envelope["request"]; ok && len(envelope) == 1 {
		return decodeArgs(raw, target)
	}
	return decodeArgs(args, target)
}
