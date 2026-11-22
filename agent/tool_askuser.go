package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// askUserQuestionToolName is the DSH tool name. The browser keys its question
// card on it, so the spelling is part of the wire contract, not a preference.
const askUserQuestionToolName = "ask_user_question"

// askUserQuestionParameters mirrors the DSH schema field for field.
//
// Every name here is read by the original UI: `id` pairs an answer with its
// question, `multi_select` decides whether the card allows more than one pick,
// and an option's `description` is the sentence under its label. Renaming any of
// them silently degrades the card instead of failing, which is why they are
// spelled exactly as DSH spells them.
func askUserQuestionParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type":        "array",
				"description": "Questions to ask the user before continuing.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": true,
					"properties": map[string]any{
						"id": map[string]any{
							"type":        "string",
							"description": "Stable id for this question; echoed in the answer.",
						},
						"question": map[string]any{
							"type":        "string",
							"description": "The specific question to ask the user.",
						},
						"header": map[string]any{
							"type":        "string",
							"description": `Optional short heading for the question, such as "Confirm" or "Choose Mode".`,
						},
						"options": map[string]any{
							"type":        "array",
							"description": `Optional choices to show the user. If you recommend one, put it first and append "(Recommended)" to that label.`,
							"items": map[string]any{
								"type":                 "object",
								"additionalProperties": true,
								"properties": map[string]any{
									"label": map[string]any{
										"type":        "string",
										"description": "Short user-facing option label.",
									},
									"description": map[string]any{
										"type":        "string",
										"description": "One sentence explaining the tradeoff or impact.",
									},
								},
								"required": []string{"label"},
							},
						},
						"multi_select": map[string]any{
							"type":        "boolean",
							"description": "Whether the user may select more than one option. Defaults to false.",
						},
					},
					"required": []string{"id", "question"},
				},
			},
		},
		"required": []string{"questions"},
	}
}

// askUserQuestionParametersWithWait adds the foreground window the page needs to
// see before it treats a call as answerable.
//
// The parameter is present in the logged request header, and the session page's
// question projection reads its presence to tell a call it can answer from one
// only an in-process waterfall could answer. Omitting it does not make the tool
// stricter: it makes the question unanswerable from a browser.
func askUserQuestionParametersWithWait() map[string]any {
	params := askUserQuestionParameters()
	params["properties"].(map[string]any)[askTimeoutParameter] = map[string]any{
		"type": "integer",
		"description": "Wait seconds for the entire batch (default 120); omit unless the user specifies a duration. " +
			"Use -1 only when an answer is required before proceeding.",
	}
	return params
}

// askTimeoutParameter is the parameter name the page reads out of the header.
const askTimeoutParameter = "timeout"

// askUserQuestionDescrption tells the model when to ask instead of guessing.
const askUserQuestionDescription = "Ask the user a concise question when you need confirmation, a choice, or missing " +
	"information before proceeding. If the user skips a question, continue with the most reasonable assumption and say " +
	"which assumption you made. A pending result means no answer arrived before the wait window; the questions stay " +
	"answerable, so continue useful independent work instead of asking them again."

// askUserQuestionDescriptionLegacy is kept only for reference by the timed form.
const askUserQuestionDescriptionLegacy = "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding. " +
	"If the user skips a question, continue with the most reasonable assumption and say which assumption you made."

// askUserQuestion waits for a person to answer through the browser.
//
// The wait has a window. When it closes, the call returns a pending result and the
// questions remain answerable: the turn is not held hostage by someone who walked
// away, and the model is told plainly that nothing was decided.
func (e *ToolExecutor) askUserQuestion(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	items, err := decodeQuestions(args)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	wait, err := decodeWait(args)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	if inv.SessionID == "" {
		// Without a session there is no card to put the question on and no call to
		// key an answer by; asking would wait with nothing able to answer.
		return ToolOutcome{Text: "ask_user_question needs an open session", IsError: true}
	}
	callID := strings.TrimSpace(inv.CallID)
	if callID == "" {
		return ToolOutcome{Text: "ask_user_question needs a tool call id", IsError: true}
	}

	batch, timedOut, err := awaitQuestion(ctx, inv.SessionID, callID, items, wait)
	if err != nil {
		// The turn ended while the question stood. Reporting that is what lets the
		// turn close instead of leaving a card waiting forever.
		return ToolOutcome{Text: "the turn ended before the question was answered", IsError: true}
	}
	if timedOut {
		encoded, marshalErr := json.Marshal(PendingQuestionResult{
			Pending: true,
			CallID:  callID,
			Message: PendingNotice,
		})
		if marshalErr != nil {
			return ToolOutcome{Text: "encode pending result: " + marshalErr.Error(), IsError: true}
		}
		return ToolOutcome{Text: string(encoded)}
	}
	if batch == nil {
		return ToolOutcome{Text: "no answer was recorded", IsError: true}
	}
	// The answers are the tool result the model reads, so the shape has to be the
	// one DSH writes: an object with an `answers` array.
	encoded, err := json.Marshal(batch)
	if err != nil {
		return ToolOutcome{Text: "encode answers: " + err.Error(), IsError: true}
	}
	return ToolOutcome{Text: string(encoded), Meta: map[string]any{"answers": batch.Answers}}
}

// decodeWait reads the foreground window in milliseconds.
//
// Zero means the model named none, which uses the default. A negative value other
// than -1 is a mistake worth reporting: "wait -5 seconds" would otherwise silently
// become "wait forever".
func decodeWait(args map[string]any) (time.Duration, error) {
	raw, ok := args[askTimeoutParameter]
	if !ok || raw == nil {
		return DefaultQuestionWait, nil
	}
	seconds, ok := toInt(raw)
	if !ok {
		return 0, fmt.Errorf("%s must be an integer number of seconds", askTimeoutParameter)
	}
	switch {
	case seconds == -1:
		// Indefinite: the model said an answer is required before proceeding, so
		// the turn waits for one.
		return 0, nil
	case seconds == 0:
		return DefaultQuestionWait, nil
	case seconds < 0:
		return 0, fmt.Errorf("%s must be -1 or a positive number of seconds", askTimeoutParameter)
	default:
		return time.Duration(seconds) * time.Second, nil
	}
}

// toInt accepts the JSON numbers a model produces for an integer parameter.
func toInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	}
	return 0, false
}

// decodeQuestions reads the model's questions into the shape the card renders.
//
// A question without options is still a question: the user answers it in the
// free-text field. A deployment that must not ask at all disables the tool
// instead of making it reject calls.
func decodeQuestions(args map[string]any) ([]QuestionItem, error) {
	raw, ok := args["questions"]
	if !ok {
		return nil, fmt.Errorf("questions is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("questions: %w", err)
	}
	var items []QuestionItem
	if err := json.Unmarshal(encoded, &items); err != nil {
		return nil, fmt.Errorf("questions: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("at least one question is required")
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		items[i].ID = strings.TrimSpace(items[i].ID)
		items[i].Question = strings.TrimSpace(items[i].Question)
		if items[i].ID == "" {
			return nil, fmt.Errorf("questions[%d] has no id", i)
		}
		if items[i].Question == "" {
			return nil, fmt.Errorf("questions[%d] has no question text", i)
		}
		if seen[items[i].ID] {
			// Duplicate ids would make the answer ambiguous: the model could not
			// tell which answer belongs to which question.
			return nil, fmt.Errorf("questions[%d] repeats the id %q", i, items[i].ID)
		}
		seen[items[i].ID] = true
	}
	return items, nil
}
