package session

import (
	"encoding/json"
	"strings"
)

// The userQuestions projection is what makes an answered-later question visible.
//
// The session page renders its question card from this projection, not from the
// tool call: a call whose ask has already returned is only answerable because the
// projection still lists it as active. A backend that records the call and skips
// this fold therefore looks like it never asked anything — the model has stopped,
// the log holds the question, and the page shows no way to answer it.

// QuestionProjectionKey is the projection name the page reads.
const QuestionProjectionKey = "userQuestions"

// askUserQuestionName is the tool whose calls this projection tracks.
const askUserQuestionName = "ask_user_question"

// userQuestionsProjection folds the session's events into the page's view.
//
// A question becomes active the moment its call is recorded and stays active —
// with state "continued", which is the page's word for "the call returned but you
// can still answer" — until a reply names it. A failed call is dropped instead:
// a question the tool refused was never answerable.
func userQuestionsProjection(events []Event) map[string]any {
	active := make([]any, 0, 2)
	settled := make([]any, 0, 2)
	// Pending calls keyed by call id, in the order the model asked them, so the
	// page shows the oldest question first.
	var order []string
	pending := map[string]map[string]any{}

	for _, event := range events {
		switch event.Type {
		case EventToolCall:
			var call struct {
				CallID    string `json:"callId"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(event.Data, &call) != nil || call.Name != askUserQuestionName || call.CallID == "" {
				continue
			}
			questions := questionItemsOf(call.Arguments)
			if len(questions) == 0 {
				// Unreadable arguments never reached the tool; there is no card.
				continue
			}
			if _, exists := pending[call.CallID]; !exists {
				order = append(order, call.CallID)
			}
			pending[call.CallID] = map[string]any{
				"callId":    call.CallID,
				"questions": questions,
				// The call has returned by the time the page reads this; the
				// questions remain answerable, which is exactly "continued".
				"state": "continued",
			}
		case EventToolResult:
			var result struct {
				Message struct {
					ToolCallID string `json:"toolCallId"`
					IsError    bool   `json:"isError"`
				} `json:"message"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(event.Data, &result) != nil {
				continue
			}
			callID := result.Message.ToolCallID
			if _, tracked := pending[callID]; !tracked {
				continue
			}
			// A refused question disappears: nothing was asked, so nothing can be
			// answered. A successful result leaves it active — the result says the
			// question was recorded, not that an answer arrived.
			if result.Message.IsError || result.Error != nil {
				delete(pending, callID)
				order = removeID(order, callID)
			}
		case EventUserMessage:
			var message struct {
				Source struct {
					Kind   string `json:"kind"`
					CallID string `json:"callId"`
				} `json:"source"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(event.Data, &message) != nil ||
				message.Source.Kind != "user-question-reply" || message.Source.CallID == "" {
				continue
			}
			callID := message.Source.CallID
			if _, tracked := pending[callID]; !tracked {
				// A reply to a question this session no longer holds changes
				// nothing, which is what keeps a duplicated reply harmless.
				continue
			}
			settled = append(settled, map[string]any{
				"callId":  callID,
				"answers": answersOf(message.Content),
			})
			delete(pending, callID)
			order = removeID(order, callID)
		}
	}

	for _, callID := range order {
		if item, ok := pending[callID]; ok {
			active = append(active, item)
		}
	}
	return map[string]any{"active": active, "settled": settled}
}

// questionItemsOf reads the questions out of a call's arguments in the page's
// vocabulary, which spells the multi-select flag differently from the model.
func questionItemsOf(arguments string) []any {
	var args struct {
		Questions []struct {
			ID       string `json:"id"`
			Question string `json:"question"`
			Header   string `json:"header"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
			MultiSelect bool `json:"multi_select"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return nil
	}
	items := make([]any, 0, len(args.Questions))
	for _, question := range args.Questions {
		if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Question) == "" {
			continue
		}
		item := map[string]any{
			"id":       question.ID,
			"question": question.Question,
		}
		if question.Header != "" {
			item["header"] = question.Header
		}
		if len(question.Options) > 0 {
			options := make([]any, 0, len(question.Options))
			for _, option := range question.Options {
				entry := map[string]any{"label": option.Label}
				if option.Description != "" {
					entry["description"] = option.Description
				}
				options = append(options, entry)
			}
			item["options"] = options
		}
		if question.MultiSelect {
			item["multiSelect"] = true
		}
		items = append(items, item)
	}
	return items
}

// answersOf reads the answer batch out of a recorded reply.
func answersOf(content []struct {
	Text string `json:"text"`
}) []any {
	answers := []any{}
	for _, block := range content {
		var payload struct {
			Answers []struct {
				ID       string   `json:"id"`
				Selected []string `json:"selected"`
				Custom   string   `json:"custom"`
			} `json:"answers"`
		}
		if json.Unmarshal([]byte(block.Text), &payload) != nil {
			continue
		}
		for _, answer := range payload.Answers {
			entry := map[string]any{"id": answer.ID, "selected": answer.Selected}
			if entry["selected"] == nil {
				entry["selected"] = []string{}
			}
			if answer.Custom != "" {
				entry["custom"] = answer.Custom
			}
			answers = append(answers, entry)
		}
		return answers
	}
	return answers
}

// removeID drops one id from a slice, preserving the rest.
func removeID(ids []string, target string) []string {
	for i, id := range ids {
		if id != target {
			continue
		}
		return append(ids[:i:i], ids[i+1:]...)
	}
	return ids
}
