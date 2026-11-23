package session

import (
	"encoding/json"
)

// A reasoning-only answer that hits the output cap is not a finished answer: the
// model spent its whole budget thinking and never said anything. DSH offers a
// per-session policy to follow such a stop with `continue(auto)`, a bounded number
// of times, so a person does not have to type "keep going" after every cap.

// ThinkingCapAutoContinueEvent is the durable policy toggle.
const ThinkingCapAutoContinueEvent = "session/thinking-cap-auto-continue"

// ThinkingCapAutoContinueText is the model-visible followup.
const ThinkingCapAutoContinueText = "continue(auto)"

// ThinkingCapAutoContinueLimit is how many consecutive caps are auto-continued.
const ThinkingCapAutoContinueLimit = 2

// ThinkingCapAutoContinueState is the fold a client reads as one boolean.
type ThinkingCapAutoContinueState struct {
	// Enabled is the latest durable policy.
	Enabled bool
	// LastAssistantIsThinkingCapStop is whether the newest assistant message is a
	// reasoning-only output-cap stop.
	LastAssistantIsThinkingCapStop bool
	// ConsecutiveAutoContinues counts the streak already followed up.
	ConsecutiveAutoContinues int
	// PendingMaxTokens is whether the latest turn ended on the cap and is still
	// waiting for whatever comes next.
	PendingMaxTokens bool
}

// FoldThinkingCapAutoContinue replays the policy and the streak from the log.
func FoldThinkingCapAutoContinue(events []Event) ThinkingCapAutoContinueState {
	state := ThinkingCapAutoContinueState{}
	for _, event := range events {
		switch event.Type {
		case ThinkingCapAutoContinueEvent:
			var payload struct {
				Enabled bool `json:"enabled"`
			}
			if json.Unmarshal(event.Data, &payload) == nil {
				state.Enabled = payload.Enabled
			}
		case EventAssistantMessage:
			state.LastAssistantIsThinkingCapStop = isThinkingCapStop(event)
		case EventTurnEnd:
			if turnEndKind(event) == TurnMaxTokens {
				state.PendingMaxTokens = true
			} else {
				state.PendingMaxTokens = false
				state.ConsecutiveAutoContinues = 0
			}
		case EventUserMessage:
			if !state.PendingMaxTokens {
				if !isThinkingCapAutoContinueMessage(event) {
					state.ConsecutiveAutoContinues = 0
				}
				continue
			}
			if isThinkingCapAutoContinueMessage(event) {
				state.PendingMaxTokens = false
				state.ConsecutiveAutoContinues++
			} else {
				state.PendingMaxTokens = false
				state.ConsecutiveAutoContinues = 0
			}
		}
	}
	return state
}

// ShouldFollowupThinkingCap reports whether this stop earns a `continue(auto)`.
//
// The bound is the point: a model that keeps hitting the cap while thinking is not
// making progress, and continuing it forever would spend the user's budget on a
// loop instead of returning control.
func ShouldFollowupThinkingCap(state ThinkingCapAutoContinueState) bool {
	if !state.Enabled {
		return false
	}
	if !state.LastAssistantIsThinkingCapStop {
		return false
	}
	return state.ConsecutiveAutoContinues < ThinkingCapAutoContinueLimit
}

// isThinkingCapStop reports whether an assistant message is thinking-only.
//
// "Visible" means any non-reasoning block with content: empty text is not an
// answer, and a tool call is.
func isThinkingCapStop(event Event) bool {
	var payload struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return false
	}
	hasVisible := false
	sawReasoning := false
	for _, block := range payload.Message.Content {
		switch block.Type {
		case "reasoning":
			sawReasoning = true
		case "text":
			if trimmed := trimSpace(block.Text); trimmed != "" {
				hasVisible = true
			}
		default:
			hasVisible = true
		}
	}
	return !hasVisible && sawReasoning
}

// isThinkingCapAutoContinueMessage recognizes the followup this policy writes.
func isThinkingCapAutoContinueMessage(event Event) bool {
	if sourceKindOfEvent(event) != "thinking-cap-auto-continue" {
		return false
	}
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return false
	}
	return len(payload.Content) == 1 &&
		payload.Content[0].Type == "text" &&
		payload.Content[0].Text == ThinkingCapAutoContinueText
}

// sourceKindOfEvent reads a message source kind.
func sourceKindOfEvent(event Event) string {
	var payload struct {
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return ""
	}
	return payload.Source.Kind
}

// turnEndKind reads a turn/end reason kind.
func turnEndKind(event Event) string {
	var payload struct {
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return ""
	}
	return payload.Reason.Kind
}

// trimSpace trims the whitespace the fold cares about without importing strings
// into a file that already has enough dependencies.
func trimSpace(text string) string {
	start, end := 0, len(text)
	for start < end && isSpace(text[start]) {
		start++
	}
	for end > start && isSpace(text[end-1]) {
		end--
	}
	return text[start:end]
}

// isSpace reports whether a byte is whitespace.
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// ThinkingCapProjectionKey is the client-facing projection name.
const ThinkingCapProjectionKey = "thinkingCapAutoContinue"
