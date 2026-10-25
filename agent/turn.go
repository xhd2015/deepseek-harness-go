package agent

import (
	"context"
	"fmt"
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
	if err := r.startTurn(sess, req, turn, TextContent(req.Content)); err != nil {
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

func (r *Runner) drive(ctx context.Context, sess *session.Session, turn int, route modelconfig.Route, client *llm.Client) {
	for step := 1; step <= r.cfg.MaxSteps; step++ {
		if ctx.Err() != nil {
			r.endTurn(sess, turn, abortedReason())
			return
		}
		if step > 1 {
			if _, err := sess.Append(session.EventStepStart, map[string]any{"turn": turn, "step": step}); err != nil {
				r.finishStepError(sess, turn, step, err)
				return
			}
		}
		attempt := sess.BeginAssistantAttempt(turn, step)
		completion, streamErr := client.Stream(ctx, llm.Request{
			Model:           route.Model,
			ReasoningEffort: route.ReasoningEffort,
			Messages:        r.history(sess),
			Tools:           r.tools.Definitions(),
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
	output := "cancelled"
	isError := true
	if ctx.Err() == nil {
		output, isError = r.tools.Execute(ctx, call.block.Name, call.block.Arguments)
	}
	if len(output) > toolOutputLimit {
		output = output[:toolOutputLimit]
	}
	data := map[string]any{
		"turn": turn,
		"step": step,
		"message": map[string]any{
			"id":         session.NewID(),
			"role":       "tool",
			"toolCallId": call.block.ID,
			"content":    []any{map[string]any{"type": "text", "text": output}},
			"source":     map[string]any{"kind": "tool", "callId": call.block.ID},
			"isError":    isError,
		},
		"sourceEventSeqs": []int64{call.seq},
	}
	if isError {
		data["error"] = map[string]any{
			"name":   "ToolError",
			"code":   "TOOL_ERROR",
			"reason": output,
		}
	}
	if _, err := sess.AppendSurface(session.EventToolResult, data); err != nil {
		r.logf("session %s: append tool result: %v", sess.ID(), err)
	}
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
