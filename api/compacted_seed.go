package api

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// A compacted reset starts the child from the source's *current context*, not
// from its transcript.
//
// A compaction replaced an old range with a summary checkpoint and kept the recent
// tail; that surface is exactly what the source would send next. Copying the log
// prefix (a fork) would keep the old events instead, which is why a fork and a
// reset are different operations with the same-looking result until you measure
// the child's size.

// Compacted-seed retention. The last turn stays whole; older kept turns shrink to
// their user message and their final assistant message.
const (
	// seedRetainTurns is how many turns survive, including the last.
	seedRetainTurns = 10
	// seedMaxToolResultBytes is the per-result cap inside the retained tail.
	seedMaxToolResultBytes = 64 * 1024
)

// seedPart is one surface event inside a turn being rebuilt.
type seedPart struct {
	kind  string // user, assistant, tool
	event session.Event
}

// seedRound is one question-and-answer round of the rebuilt seed.
type seedRound struct {
	nodes       []seedPart
	toolResults int
}

// buildCompactedSeed materializes a surface into a child's log.
//
// The events read as ordinary history: whole turns for the retained window, one
// summary checkpoint kept as-is, and an end-seed cut that says the rest is
// inherited. Nothing in it points back at the source log, so the child is smaller
// by construction rather than by truncation.
func buildCompactedSeed(events []session.Event, surface []session.Event) []session.Event {
	bySeq := make(map[int64]session.Event, len(events))
	for _, event := range events {
		bySeq[event.Seq] = event
	}
	turns := groupSeedTurns(surface, bySeq)
	checkpoint := -1
	for index, turn := range turns {
		if len(turn.nodes) == 0 {
			continue
		}
		if sourceKindOf(turn.nodes[0].event) == "compact-checkpoint" {
			checkpoint = index
		}
	}
	// The checkpoint, then the most recent turns beside it. `-1` because the slice
	// count includes the checkpoint turn itself.
	start := 0
	if len(turns) > seedRetainTurns+1 {
		start = len(turns) - (seedRetainTurns + 1)
	}
	selected := make([]seedRound, 0, len(turns)-start+1)
	selected = append(selected, turns[start:]...)
	if checkpoint >= 0 && !containsTurn(turns, selected, checkpoint) {
		selected = append([]seedRound{turns[checkpoint]}, selected...)
	}

	seed := make([]session.Event, 0, 8*len(selected))
	add := func(typ string, data any, surfaceOp *session.SurfaceOp) {
		encoded, err := session.EncodeData(data)
		if err != nil {
			return
		}
		seed = append(seed, session.Event{Type: typ, Data: encoded, SurfaceOp: surfaceOp})
	}
	turnNumber := 0
	step := 1
	emit := func(source seedRound, whole bool) {
		lastAssistant := -1
		for index, node := range source.nodes {
			if node.kind == "assistant" {
				lastAssistant = index
			}
		}
		nodes := source.nodes
		if !whole {
			// A shrunk turn keeps the question and the final answer: the middle is
			// what a summary already covers.
			shrunk := make([]seedPart, 0, 2)
			for index, node := range source.nodes {
				if node.kind == "user" || index == lastAssistant {
					shrunk = append(shrunk, node)
				}
			}
			nodes = shrunk
		}
		if len(nodes) == 0 {
			return
		}
		turnNumber++
		step = 1
		add(session.EventTurnStart, map[string]any{"turn": turnNumber}, nil)
		add(session.EventStepStart, map[string]any{"turn": turnNumber, "step": step}, nil)
		if turnNumber == 1 {
			add(session.EventSystemMessage, map[string]any{
				"turn": turnNumber, "step": step,
				"message": map[string]any{
					"role": "system", "id": session.NewID(),
					"content": []any{},
					"source":  map[string]any{"kind": "system-prompt"},
				},
			}, session.SplitSurface)
		}
		assistantSeen := false
		for _, node := range nodes {
			switch node.kind {
			case "user":
				add(session.EventUserMessage, seedUserData(node.event, source.toolResults, whole), session.SplitSurface)
			case "assistant":
				if assistantSeen {
					add(session.EventStepEnd, map[string]any{"turn": turnNumber, "step": step}, nil)
					step++
					add(session.EventStepStart, map[string]any{"turn": turnNumber, "step": step}, nil)
				}
				assistantSeen = true
				add(session.EventAssistantMessage, seedAssistantData(node.event, turnNumber, step), session.SplitSurface)
				for _, callID := range seedToolCallIDs(node.event) {
					add(session.EventToolCall, map[string]any{
						"turn": turnNumber, "step": step,
						"callId": callID.id, "name": callID.name, "arguments": callID.arguments,
					}, nil)
				}
			case "tool":
				add(session.EventToolResult, seedToolResultData(node.event, turnNumber, step), session.SplitSurface)
			}
		}
		add(session.EventStepEnd, map[string]any{"turn": turnNumber, "step": step}, nil)
		add(session.EventTurnEnd, map[string]any{"turn": turnNumber, "reason": map[string]any{"kind": "completed"}}, nil)
	}
	for index, turn := range selected {
		isCheckpoint := len(turn.nodes) > 0 && sourceKindOf(turn.nodes[0].event) == "compact-checkpoint"
		emit(turn, isCheckpoint || index == len(selected)-1)
	}
	add(session.EventSessionEndSeed, map[string]any{"inherited": true}, nil)
	return seed
}

// groupSeedTurns groups surface events into turns.
//
// Only conversation counts: runtime context (time, environment) is not a turn, and
// a compaction checkpoint is the summary itself, so it starts one.
func groupSeedTurns(surface []session.Event, bySeq map[int64]session.Event) []seedRound {
	turns := make([]seedRound, 0, 16)
	for _, node := range surface {
		event, ok := bySeq[node.Seq]
		if !ok {
			continue
		}
		switch event.Type {
		case session.EventUserMessage:
			kind := sourceKindOf(event)
			if kind != "user" && kind != "compact-checkpoint" {
				continue
			}
			turns = append(turns, seedRound{nodes: []seedPart{{kind: "user", event: event}}})
		case session.EventAssistantMessage:
			if len(turns) == 0 {
				turns = append(turns, seedRound{})
			}
			tail := &turns[len(turns)-1]
			tail.nodes = append(tail.nodes, seedPart{kind: "assistant", event: event})
		case session.EventToolResult:
			if len(turns) == 0 {
				turns = append(turns, seedRound{})
			}
			tail := &turns[len(turns)-1]
			tail.nodes = append(tail.nodes, seedPart{kind: "tool", event: event})
			tail.toolResults++
		}
	}
	return turns
}

// containsTurn reports whether one of the selected turns is the checkpoint.
func containsTurn(all, selected []seedRound, checkpoint int) bool {
	for _, turn := range selected {
		if len(turn.nodes) > 0 && len(all[checkpoint].nodes) > 0 &&
			turn.nodes[0].event.Seq == all[checkpoint].nodes[0].event.Seq {
			return true
		}
	}
	return false
}

// sourceKindOf reads the message source kind, or "" for an event without one.
func sourceKindOf(event session.Event) string {
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

// seedUserData copies a user message, noting what a shrunk turn omitted.
func seedUserData(event session.Event, omitted int, whole bool) map[string]any {
	var payload map[string]any
	if json.Unmarshal(event.Data, &payload) != nil {
		return map[string]any{"role": "user", "id": session.NewID(), "content": []any{}}
	}
	payload["role"] = "user"
	if _, ok := payload["id"]; !ok {
		payload["id"] = session.NewID()
	}
	if !whole && omitted > 0 {
		content, _ := payload["content"].([]any)
		content = append(content, map[string]any{
			"type": "text",
			"text": fmt.Sprintf("[omitted %d tool calls]", omitted),
		})
		payload["content"] = content
	}
	return payload
}

// seedAssistantData copies an assistant message at its new turn and step.
func seedAssistantData(event session.Event, turn, step int) map[string]any {
	var payload struct {
		Message json.RawMessage `json:"message"`
		Stream  json.RawMessage `json:"stream"`
	}
	_ = json.Unmarshal(event.Data, &payload)
	data := map[string]any{"turn": turn, "step": step}
	if len(payload.Message) > 0 {
		var message any
		_ = json.Unmarshal(payload.Message, &message)
		data["message"] = message
	}
	if len(payload.Stream) > 0 {
		var stream any
		_ = json.Unmarshal(payload.Stream, &stream)
		data["stream"] = stream
	}
	return data
}

// seedToolResultData copies a tool result, truncating an oversized one.
func seedToolResultData(event session.Event, turn, step int) map[string]any {
	var payload struct {
		Message map[string]any `json:"message"`
	}
	_ = json.Unmarshal(event.Data, &payload)
	message := payload.Message
	if message == nil {
		message = map[string]any{"role": "tool", "id": session.NewID()}
	}
	// A result in the retained tail can still be large; the child exists to be
	// small, so its content is capped rather than copied verbatim.
	if content, ok := message["content"].([]any); ok {
		if truncated, changed := truncateSeedContent(content, seedMaxToolResultBytes); changed {
			message["content"] = truncated
		}
	}
	return map[string]any{"turn": turn, "step": step, "message": message}
}

// truncateSeedContent replaces oversized content with a head/tail excerpt.
func truncateSeedContent(content []any, maxBytes int) ([]any, bool) {
	encoded, err := json.Marshal(content)
	if err != nil || len(encoded) <= maxBytes {
		return content, false
	}
	half := maxBytes / 2
	if half < 1 {
		half = 1
	}
	text := string(encoded)
	omitted := len(text) - maxBytes
	return []any{map[string]any{
		"type": "text",
		"text": fmt.Sprintf("%s\n… [omitted %d chars] …\n%s", text[:half], omitted, text[len(text)-half:]),
	}}, true
}

// seedToolCall is one tool call inside an assistant message.
type seedToolCall struct {
	id        string
	name      string
	arguments string
}

// seedToolCallIDs reads the tool calls an assistant message declared.
//
// The calls live inside the message's content, but the log records them as their
// own events; a seed that dropped them would leave the results answering nothing.
func seedToolCallIDs(event session.Event) []seedToolCall {
	var payload struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return nil
	}
	calls := make([]seedToolCall, 0, len(payload.Message.Content))
	for _, block := range payload.Message.Content {
		if block.Type != "tool-call" {
			continue
		}
		calls = append(calls, seedToolCall{id: block.ID, name: block.Name, arguments: block.Arguments})
	}
	return calls
}

// compactedSeedTokens reports how much context a seed carries, for the reply.
func compactedSeedTokens(seed []session.Event) int {
	total := 0
	for _, event := range seed {
		total += len(event.Data)/4 + 1
	}
	return total
}

// sortedSeqsOf returns an event list's seqs in ascending order.
func sortedSeqsOf(events []session.Event) []int64 {
	seqs := make([]int64, 0, len(events))
	for _, event := range events {
		seqs = append(seqs, event.Seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}
