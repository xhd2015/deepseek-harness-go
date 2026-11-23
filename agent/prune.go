package agent

import (
	"encoding/json"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// Pruning is the model-free half of keeping a session small.
//
// A long tool result is worth keeping in outline and worthless in full: the model
// already read it, and its middle is what it will never need again. Pruning cuts
// that middle and records the replacement the same way a compaction does — a
// `compaction/prune` shadow price followed immediately by the replacement — so a
// reader can subtract the old node's cost without keeping per-node prices.

// Prune defaults, matching DSH's tool-result pruner.
const (
	// pruneThresholdChars is the size above which a result is worth cutting.
	pruneThresholdChars = 8192
	// pruneHeadChars is what stays from the start, where the shape of the output
	// usually lives.
	pruneHeadChars = 4096
	// pruneTailChars is what stays from the end, where the conclusion usually is.
	pruneTailChars = 1024
)

// pruneMarker is the text that marks the cut. It is deliberately loud: a model
// that reads a silently shortened result will assume nothing was removed.
const pruneMarker = "\n\n[... tool result middle pruned ...]\n\n"

// PruneResult reports one pass over a session.
type PruneResult struct {
	// Pruned is how many results were replaced.
	Pruned int
	// CharsRemoved is how much text the replacements saved.
	CharsRemoved int
}

// PruneSession cuts the over-budget tool results on the current surface.
//
// Only the surface is considered: a result an earlier compaction already replaced
// is not sent to the model, so cutting it again would spend writes on bytes nobody
// reads.
func (r *Runner) PruneSession(opened *session.Session) (PruneResult, error) {
	result := PruneResult{}
	surface := opened.Surface()
	for _, event := range surface {
		if event.Type != session.EventToolResult {
			continue
		}
		var payload struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(event.Data, &payload) != nil {
			continue
		}
		var message map[string]any
		if json.Unmarshal(payload.Message, &message) != nil {
			continue
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			continue
		}
		pruned, removed := pruneBlocks(blocks)
		if pruned == nil {
			continue
		}
		message["content"] = pruned
		// The shadow price first: the replacement must follow it immediately, so a
		// reader that walks the log prices the node it is about to replace.
		if _, err := opened.Append(session.EventCompactionPrune, map[string]any{
			"shadowedRange":      session.SeqRange{Start: event.Seq, End: event.Seq},
			"shadowedSeqs":       []int64{event.Seq},
			"shadowedTokenCount": estimateTokens(resultText(blocks)),
		}); err != nil {
			return result, err
		}
		raw, err := session.EncodeData(map[string]any{
			"turn":    eventTurn(event),
			"step":    eventStep(event),
			"message": message,
		})
		if err != nil {
			return result, err
		}
		if _, err := opened.AppendReplacing(session.EventToolResult, raw, session.SeqRange{
			Start: event.Seq,
			End:   event.Seq,
		}); err != nil {
			return result, err
		}
		result.Pruned++
		result.CharsRemoved += removed
	}
	return result, nil
}

// pruneBlocks cuts the text middle of one result, or reports that it fits.
func pruneBlocks(blocks []any) ([]any, int) {
	total := 0
	for _, block := range blocks {
		total += blockChars(block)
	}
	if total <= pruneThresholdChars {
		return nil, 0
	}
	removedStart := pruneHeadChars
	removedEnd := total - pruneTailChars
	if removedEnd <= removedStart {
		return nil, 0
	}
	pruned := make([]any, 0, len(blocks)+1)
	consumed := 0
	inserted := false
	for _, block := range blocks {
		record, ok := block.(map[string]any)
		if !ok {
			pruned = append(pruned, block)
			continue
		}
		text, isText := record["text"].(string)
		if !isText || record["type"] != "text" {
			pruned = append(pruned, block)
			continue
		}
		points := runes(text)
		blockStart := consumed
		blockEnd := blockStart + len(points)
		// keep is how many code points at each edge of this block survive: the
		// part before the cut starts, and the part after it ends.
		keepHead := clampInt(removedStart-blockStart, 0, len(points))
		keepTail := clampInt(blockEnd-removedEnd, 0, len(points))
		if keepTail < keepHead {
			keepTail = keepHead
		}
		intersects := blockStart < removedEnd && blockEnd > removedStart
		marker := ""
		if intersects && !inserted {
			marker = pruneMarker
			inserted = true
		}
		// keepHead counts from the start, keepTail from the end.
		replacement := string(points[:keepHead]) + marker + string(points[len(points)-keepTail:])
		if replacement != "" {
			updated := map[string]any{}
			for key, value := range record {
				updated[key] = value
			}
			updated["text"] = replacement
			pruned = append(pruned, updated)
		}
		consumed = blockEnd
	}
	if !inserted {
		return nil, 0
	}
	after := 0
	for _, block := range pruned {
		after += blockChars(block)
	}
	return pruned, total - after
}

// runes is the code-point view of a string, so a cut cannot split a character.
func runes(text string) []rune { return []rune(text) }

// blockChars reports one content block's text length in code points.
func blockChars(block any) int {
	record, ok := block.(map[string]any)
	if !ok {
		return 0
	}
	text, ok := record["text"].(string)
	if !ok {
		return 0
	}
	return len(runes(text))
}

// resultText concatenates a result's text blocks, for pricing it.
func resultText(blocks []any) string {
	combined := ""
	for _, block := range blocks {
		if record, ok := block.(map[string]any); ok {
			if text, ok := record["text"].(string); ok {
				combined += text
			}
		}
	}
	return combined
}

// clampInt bounds a value into a range.
func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// eventTurn reads one event's turn, or zero.
func eventTurn(event session.Event) int64 { return eventNumber(event, "turn") }

// eventStep reads one event's step, or zero.
func eventStep(event session.Event) int64 { return eventNumber(event, "step") }

// eventNumber reads one integer field out of an event payload.
func eventNumber(event session.Event, field string) int64 {
	var payload map[string]any
	if json.Unmarshal(event.Data, &payload) != nil {
		return 0
	}
	value, ok := toInt(payload[field])
	if !ok {
		return 0
	}
	return value
}
