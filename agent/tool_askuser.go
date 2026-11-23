package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// askUserQuestionToolName is the DSH tool name. The page keys its question card on
// it, so the spelling is part of the wire contract, not a preference.
const askUserQuestionToolName = "ask_user_question"

// askUserQuestionDescription is the DSH wording, verbatim.
//
// It carries the skip semantics and nothing else: no wait, no timeout, no
// "pending". Those belong to the opt-in timed variant DSH keeps in a separate
// package for deployments that want a foreground window, and advertising them
// here would tell the model about a contract this backend does not implement.
const askUserQuestionDescription = "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding. " +
	"An answer item with empty selected and no custom means the user skipped that question: continue without it, do not ask it again, and do not treat the skip as permission."

// askUserQuestionParameters is the DSH schema, field for field.
//
// Every name here is read by the page: `id` pairs an answer with its question,
// `multi_select` decides whether the card allows more than one pick, and an
// option's `description` is the sentence under its label. There is deliberately no
// `timeout`: the standard web schema has none, and a parameter that exists only
// here would make this tool differ from the one the model is calibrated on.
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

// askUserQuestion records the question and returns at once.
//
// It does not wait for the answer, on purpose. A person may take minutes or days,
// and holding a turn open for them would leave the session running with nothing
// happening — and would make an unattended question look like a hung backend. The
// call returns immediately, the turn ends normally, and the answer arrives later
// as a message that continues the conversation.
func (e *ToolExecutor) askUserQuestion(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	// ctx is deliberately unused: this call never waits, so there is nothing to
	// cancel. Aborting the turn still works — it ends after this call returns.
	_ = ctx
	items, err := decodeQuestions(args)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	if inv.SessionID == "" {
		// Without a session there is no card to put the question on and no call to
		// key an answer by; recording it would leave an answer with nowhere to go.
		return ToolOutcome{Text: "ask_user_question needs an open session", IsError: true}
	}
	callID := strings.TrimSpace(inv.CallID)
	if callID == "" {
		return ToolOutcome{Text: "ask_user_question needs a tool call id", IsError: true}
	}
	registerQuestion(inv.SessionID, callID, items)

	encoded, marshalErr := json.Marshal(AskedQuestionResult{
		Asked:   true,
		CallID:  callID,
		Message: AskedNotice,
	})
	if marshalErr != nil {
		return ToolOutcome{Text: "encode result: " + marshalErr.Error(), IsError: true}
	}
	return ToolOutcome{Text: string(encoded)}
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
