package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/session"
)

type pendingCall struct {
	block llm.ContentBlock
	seq   int64
}

// run executes one accepted prompt to its durable turn/end.
func (r *Runner) run(ctx context.Context, req PromptRequest) {
	sess := req.Session
	if err := r.ensurePreamble(sess); err != nil {
		r.logf("session %s: preamble: %v", sess.ID(), err)
	}
	turn := nextTurn(sess.Events())
	// A bug in one model turn must cost that turn, not the process: without
	// this, a decoder fault leaves the turn open and takes every other session
	// down with it.
	defer r.recoverTurn(sess, turn)
	r.driveTurn(ctx, req, sess, turn)
}

// driveTurn runs one turn, from its opening event to its close.
func (r *Runner) driveTurn(ctx context.Context, req PromptRequest, sess *session.Session, turn int) {
	started := r.startTurn
	if req.Continuation {
		// A continuation acts on history that was already committed (an injected
		// batch or an edit), so it opens a turn without appending a user message.
		started = r.startContinuationTurn
	}
	// Pressure is checked before the turn's request is assembled: a session that
	// no longer fits its window must be shrunk before the model is sent a history
	// the provider will refuse.
	//
	// The model-free prune runs first and the result is re-measured, exactly as
	// DSH orders it: cutting a few oversized tool results usually closes the
	// excess on its own, and summarizing history to reclaim text that could
	// simply be trimmed would spend a model call to lose more than it saved.
	if budget, pressure := r.shouldCompact(sess); pressure {
		if pruned, err := r.PruneSession(sess); err != nil {
			r.logf("session %s: prune: %v", sess.ID(), err)
		} else if pruned.Pruned > 0 {
			r.logf("session %s: pruned %d tool results (-%d chars)", sess.ID(), pruned.Pruned, pruned.CharsRemoved)
		}
		if _, stillPressed := r.shouldCompact(sess); stillPressed {
			turnNumber := int64(turn)
			if compacted, err := r.compactSurface(ctx, sess, budget, &turnNumber); err != nil {
				r.logf("session %s: compaction: %v", sess.ID(), err)
			} else if compacted {
				r.logf("session %s: compacted under pressure", sess.ID())
			}
		}
	}
	if err := started(sess, req, turn, TextContent(req.Content)); err != nil {
		r.logf("session %s: start turn: %v", sess.ID(), err)
		r.endTurn(sess, turn, map[string]any{"kind": "error", "error": failureOf(err)})
		return
	}
	route, client, err := r.resolve(r.currentSelection(sess))
	if err != nil {
		r.endTurn(sess, turn, map[string]any{"kind": "error", "error": failureOf(err)})
		return
	}
	r.drive(ctx, sess, turn, route, client)
}

// recoverTurn closes a turn that panicked and reports the fault, so the log
// keeps a terminal event and the server keeps running.
func (r *Runner) recoverTurn(sess *session.Session, turn int) {
	failure := recover()
	if failure == nil {
		return
	}
	r.logf("session %s: turn %d panicked: %v\n%s", sess.ID(), turn, failure, debug.Stack())
	r.endTurn(sess, turn, map[string]any{
		"kind": "error",
		"error": map[string]any{
			"code":    "PANIC",
			"message": fmt.Sprintf("internal error: %v", failure),
		},
	})
}

// startContinuationTurn opens a turn on already-committed history.
func (r *Runner) startContinuationTurn(sess *session.Session, _ PromptRequest, turn int, _ string) error {
	if _, err := sess.Append(session.EventTurnStart, map[string]any{"turn": turn}); err != nil {
		return err
	}
	if _, err := sess.Append(session.EventStepStart, map[string]any{"turn": turn, "step": 1}); err != nil {
		return err
	}
	return nil
}

func (r *Runner) drive(ctx context.Context, sess *session.Session, turn int, route modelconfig.Route, client *llm.Client) {
	for step := 1; step <= r.cfg.MaxSteps; step++ {
		if ctx.Err() != nil {
			r.endTurn(sess, turn, abortedReason())
			return
		}
		if step > 1 {
			// A model selected mid-turn takes effect here, at the boundary: swapping
			// inside a request would cut the reasoning and tool-call semantics the
			// step already committed to.
			if next, nextClient, err := r.resolve(r.currentSelection(sess)); err == nil {
				route, client = next, nextClient
			}
			r.appendPendingNotices(sess)
			if _, err := sess.Append(session.EventStepStart, map[string]any{"turn": turn, "step": step}); err != nil {
				r.finishStepError(sess, turn, step, err)
				return
			}
		}
		// The request header records the route and the assembled tool schemas a
		// committed request actually used; consumers fold it into lastUsed.
		if err := r.logRequestHeader(sess, route); err != nil {
			r.finishStepError(sess, turn, step, err)
			return
		}
		attempt := sess.BeginAssistantAttempt(turn, step)
		completion, streamErr := client.Stream(ctx, llm.Request{
			Model:           route.Model,
			ReasoningEffort: route.ReasoningEffort,
			Messages:        r.history(sess),
			Tools:           r.tools.toolsFor(presetIDOf(sess)),
			SessionID:       sess.ID(),
		}, func(chunk llm.Chunk) error {
			return attempt.Push(chunk)
		})
		if streamErr != nil {
			r.settleFailedAttempt(ctx, sess, turn, step, route, attempt, completion, streamErr)
			return
		}
		content := assistantContent(completion.Blocks, true)
		seq, err := r.appendAssistantMessage(sess, turn, step, route, completion, content, attempt, false)
		if err != nil {
			attempt.Abandon()
			r.finishStepError(sess, turn, step, err)
			return
		}
		attempt.Settle(session.EventAssistantMessage, seq)

		if completion.FinishReason == "max-tokens" {
			r.endStep(sess, turn, step)
			r.endTurn(sess, turn, map[string]any{"kind": "max-tokens"})
			// A reasoning-only stop spent the whole budget thinking and said nothing.
			// With the per-session policy on, follow it up instead of leaving the
			// person to type "keep going" — bounded, so a model stuck at the cap
			// returns control rather than looping on the user's budget.
			r.followupThinkingCap(ctx, sess)
			return
		}

		calls, callErr := r.appendToolCalls(sess, turn, step, completion.Blocks)
		if callErr != nil {
			r.finishStepError(sess, turn, step, callErr)
			return
		}
		if len(calls) == 0 {
			r.endStep(sess, turn, step)
			r.endTurn(sess, turn, map[string]any{"kind": "completed"})
			return
		}
		for _, call := range calls {
			r.runTool(ctx, sess, turn, step, call)
		}
		r.endStep(sess, turn, step)
		if ctx.Err() != nil {
			r.endTurn(sess, turn, abortedReason())
			return
		}
		if step == r.cfg.MaxSteps {
			r.endTurn(sess, turn, map[string]any{
				"kind":  "error",
				"error": map[string]any{"code": "MAX_STEPS", "message": fmt.Sprintf("turn reached the %d-step limit", r.cfg.MaxSteps)},
			})
			return
		}
	}
}

func (r *Runner) appendAssistantMessage(
	sess *session.Session, turn, step int, route modelconfig.Route,
	completion llm.Completion, content []any, attempt *session.AssistantAttempt, interrupted bool,
) (int64, error) {
	model := completion.Model
	if model == "" {
		model = route.Model
	}
	source := map[string]any{
		"kind":     "model",
		"provider": route.Provider,
		"model":    model,
	}
	if len(completion.Replay) > 0 && !interrupted {
		source["replayState"] = completion.Replay
	}
	data := map[string]any{
		"turn": turn,
		"step": step,
		"message": map[string]any{
			"id":      session.NewID(),
			"role":    "assistant",
			"content": content,
			"source":  source,
		},
		"stream": attempt.CompactStream(),
	}
	if !emptyUsage(completion.Usage) {
		data["usage"] = completion.Usage
	}
	if interrupted {
		data["interrupted"] = true
	}
	event, err := sess.AppendSurface(session.EventAssistantMessage, data)
	if err != nil {
		return 0, err
	}
	return event.Seq, nil
}

func assistantContent(blocks []llm.ContentBlock, includeTools bool) []any {
	out := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": block.Text})
		case "reasoning":
			out = append(out, map[string]any{"type": "reasoning", "text": block.Text})
		case "tool-call":
			if !includeTools {
				continue
			}
			out = append(out, map[string]any{
				"type":      "tool-call",
				"id":        block.ID,
				"name":      block.Name,
				"arguments": block.Arguments,
			})
		}
	}
	return out
}

func visibleContent(blocks []llm.ContentBlock) []any {
	out := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" && block.Type != "reasoning" {
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		out = append(out, map[string]any{"type": block.Type, "text": block.Text})
	}
	return out
}

func (r *Runner) appendToolCalls(sess *session.Session, turn, step int, blocks []llm.ContentBlock) ([]pendingCall, error) {
	var calls []pendingCall
	for _, block := range blocks {
		if block.Type != "tool-call" {
			continue
		}
		event, err := sess.Append(session.EventToolCall, map[string]any{
			"turn":      turn,
			"step":      step,
			"callId":    block.ID,
			"name":      block.Name,
			"arguments": block.Arguments,
		})
		if err != nil {
			return nil, err
		}
		calls = append(calls, pendingCall{block: block, seq: event.Seq})
	}
	return calls, nil
}

func (r *Runner) runTool(ctx context.Context, sess *session.Session, turn, step int, call pendingCall) {
	outcome := ToolOutcome{Text: "cancelled", IsError: true}
	if ctx.Err() == nil {
		outcome = r.tools.ExecuteOutcome(ctx, ToolInvocation{
			SessionID:  sess.ID(),
			SessionDir: sess.Dir(),
			Cwd:        sess.Cwd(),
			Name:       call.block.Name,
			Arguments:  call.block.Arguments,
			CallID:     call.block.ID,
			RootCallID: call.block.ID,
			Emit: func(eventType string, data any) error {
				_, err := sess.Append(eventType, data)
				return err
			},
			Notify: func(text string) {
				r.notifyCompletion(sess, text)
			},
			Session: sess,
		})
	}
	content := toolResultContent(outcome, call.block.ID)
	data := map[string]any{
		"turn": turn,
		"step": step,
		"message": map[string]any{
			"id":         session.NewID(),
			"role":       "tool",
			"toolCallId": call.block.ID,
			"content":    content,
			"source":     map[string]any{"kind": "tool", "callId": call.block.ID},
			"isError":    outcome.IsError,
		},
	}
	if outcome.Meta != nil {
		data["meta"] = outcome.Meta
	}
	if outcome.IsError {
		data["error"] = map[string]any{
			"name":   "ToolError",
			"code":   "TOOL_ERROR",
			"reason": outcome.Text,
		}
	}
	// The call this answers belongs to the envelope, not to the payload.
	if _, err := sess.AppendWithSource(session.EventToolResult, data, []int64{call.seq}); err != nil {
		r.logf("session %s: append tool result: %v", sess.ID(), err)
	}
}

// toolResultContent returns the durable content blocks for one result. A
// single-text result keeps the historical truncation; structured results
// truncate only their text block so image attachments survive intact.
func toolResultContent(outcome ToolOutcome, callID string) []any {
	content := make([]any, 0, len(outcome.Content))
	for _, raw := range outcome.Content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		copied := make(map[string]any, len(block))
		for key, value := range block {
			copied[key] = value
		}
		content = append(content, copied)
	}
	if len(content) == 0 {
		return []any{map[string]any{"type": "text", "text": truncateUTF8(outcome.Text, toolOutputLimit)}}
	}
	if len(outcome.Text) > toolOutputLimit {
		outcome.Text = truncateUTF8(outcome.Text, toolOutputLimit)
	}
	for _, raw := range content {
		block := raw.(map[string]any)
		if block["type"] != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			block["text"] = truncateUTF8(text, toolOutputLimit)
		}
		return content
	}
	content = append([]any{map[string]any{"type": "text", "text": outcome.Text}}, content...)
	return content
}

func (r *Runner) settleFailedAttempt(
	ctx context.Context, sess *session.Session, turn, step int, route modelconfig.Route,
	attempt *session.AssistantAttempt, completion llm.Completion, streamErr error,
) {
	visible := visibleContent(completion.Blocks)
	if ctx.Err() != nil {
		settled := false
		if len(visible) > 0 {
			interrupted := completion
			interrupted.Blocks = interruptibleBlocks(completion.Blocks)
			seq, err := r.appendAssistantMessage(sess, turn, step, route, interrupted, visible, attempt, true)
			if err == nil {
				attempt.Settle(session.EventAssistantMessage, seq)
				settled = true
			}
		}
		if !settled {
			r.appendAttempt(sess, turn, step, attempt)
			attempt.Abandon()
		}
		r.endStep(sess, turn, step)
		r.endTurn(sess, turn, abortedReason())
		return
	}
	r.appendAttempt(sess, turn, step, attempt)
	attempt.Abandon()
	r.endStep(sess, turn, step)
	r.endTurn(sess, turn, map[string]any{"kind": "error", "error": failureOf(streamErr)})
}

func (r *Runner) appendAttempt(sess *session.Session, turn, step int, attempt *session.AssistantAttempt) {
	if _, err := sess.Append(session.EventAssistantAttempt, map[string]any{
		"turn":   turn,
		"step":   step,
		"stream": attempt.CompactStream(),
	}); err != nil {
		r.logf("session %s: append failed attempt: %v", sess.ID(), err)
	}
}

func interruptibleBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" && block.Type != "reasoning" {
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		out = append(out, block)
	}
	return out
}

func (r *Runner) finishStepError(sess *session.Session, turn, step int, err error) {
	r.endStep(sess, turn, step)
	r.endTurn(sess, turn, map[string]any{"kind": "error", "error": failureOf(err)})
}

func (r *Runner) endStep(sess *session.Session, turn, step int) {
	if _, err := sess.Append(session.EventStepEnd, map[string]any{"turn": turn, "step": step}); err != nil {
		r.logf("session %s: append step end: %v", sess.ID(), err)
	}
}

func (r *Runner) endTurn(sess *session.Session, turn int, reason map[string]any) {
	if _, err := sess.Append(session.EventTurnEnd, map[string]any{"turn": turn, "reason": reason}); err != nil {
		r.logf("session %s: append turn end: %v", sess.ID(), err)
	}
}

func abortedReason() map[string]any {
	return map[string]any{"kind": "aborted", "reason": map[string]any{"kind": "user"}}
}

func emptyUsage(usage llm.Usage) bool {
	return usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 &&
		usage.CacheWriteTokens == 0 && usage.TotalTokens == 0 && usage.ReasoningTokens == 0
}

// followupThinkingCap queues `continue(auto)` after a reasoning-only output-cap stop.
//
// The decision is read from the log, not from a live counter: a resumed session
// must not start its streak over, and the log is the only thing that survives a
// restart.
func (r *Runner) followupThinkingCap(ctx context.Context, sess *session.Session) {
	state := session.FoldThinkingCapAutoContinue(sess.Events())
	if !session.ShouldFollowupThinkingCap(state) {
		return
	}
	// Only when nothing else is waiting: a queued prompt is the person's own next
	// turn, and interleaving an automatic one would reorder their conversation.
	if _, err := sess.AppendSurface(session.EventUserMessage, map[string]any{
		"role": "user",
		"id":   session.NewID(),
		"content": []any{map[string]any{
			"type": "text",
			"text": session.ThinkingCapAutoContinueText,
		}},
		"source": map[string]any{"kind": "thinking-cap-auto-continue"},
	}); err != nil {
		r.logf("session %s: append auto-continue: %v", sess.ID(), err)
		return
	}
	// The current turn is still unwinding here, so the follow-up is queued rather
	// than started: Start refuses a session that is already running, by design.
	r.queueFollowup(sess.ID())
}

// logRequestHeader records the request envelope when it differs from the last one.
//
// Writing it every step would grow the log with records that say the same thing,
// and a reader folding `request/header` would see a change on every step. The
// first write of a session is `initial`; a session that already carries one and
// then changes route or tools gets `change`.
func (r *Runner) logRequestHeader(sess *session.Session, route modelconfig.Route) error {
	header := map[string]any{
		"config": map[string]any{
			"provider":        route.Provider,
			"model":           route.Model,
			"reasoningEffort": route.ReasoningEffort,
		},
		"tools": r.tools.toolsFor(presetIDOf(sess)),
	}
	previous, hasPrevious := lastRequestHeader(sess.Events())
	if hasPrevious && sameRequestHeader(previous, header) {
		return nil
	}
	reason := "change"
	if !hasPrevious {
		reason = "initial"
	}
	_, err := sess.Append(session.EventRequestHeader, map[string]any{"header": header, "reason": reason})
	return err
}

// lastRequestHeader reads the most recent recorded envelope.
func lastRequestHeader(events []session.Event) (map[string]any, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != session.EventRequestHeader {
			continue
		}
		var payload struct {
			Header map[string]any `json:"header"`
		}
		if json.Unmarshal(events[index].Data, &payload) != nil || payload.Header == nil {
			return nil, false
		}
		return payload.Header, true
	}
	return nil, false
}

// sameRequestHeader reports whether two envelopes would send the same request.
//
// Only the parts a provider can see are compared: the route and the tool names.
// Comparing the whole document would report a change whenever a schema's prose was
// rewritten, which is not something the model's request depends on.
func sameRequestHeader(left, right map[string]any) bool {
	if left == nil || right == nil {
		return false
	}
	leftConfig, _ := left["config"].(map[string]any)
	rightConfig, _ := right["config"].(map[string]any)
	for _, key := range []string{"provider", "model", "reasoningEffort"} {
		if stringField(leftConfig, key) != stringField(rightConfig, key) {
			return false
		}
	}
	return strings.Join(toolNamesOf(left["tools"]), ",") == strings.Join(toolNamesOf(right["tools"]), ",")
}

// stringField reads one string out of a loose map.
func stringField(source map[string]any, key string) string {
	if source == nil {
		return ""
	}
	value, _ := source[key].(string)
	return value
}

// toolNamesOf lists the tool names in a recorded schema list.
func toolNamesOf(tools any) []string {
	list, ok := tools.([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		if record, ok := entry.(map[string]any); ok {
			if name, ok := record["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}
