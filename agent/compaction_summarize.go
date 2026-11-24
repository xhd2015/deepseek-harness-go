package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// How the model-written summary is bounded.
//
// A compaction runs inside a turn's critical path: the next request has to go out
// after it. A summarizer that hangs, retries, or writes a whole transcript turns a
// window problem into a stalled turn, so every one of these is a hard edge rather
// than a preference.
const (
	// compactionSummaryTimeout caps one summarization call.
	compactionSummaryTimeout = 45 * time.Second
	// compactionSummaryMaxTokens caps what the summary may cost.
	compactionSummaryMaxTokens = 1200
	// compactionSummaryInputChars caps how much history is handed to the model.
	// The span being replaced can be tens of megabytes; a summary of it does not
	// need all of that in the prompt, and sending it would recreate the very
	// overflow the compaction exists to fix.
	compactionSummaryInputChars = 120_000
)

// summarizeSpan asks the session's own model to replace a span with prose.
//
// It makes exactly one attempt. A retry here would spend the turn's budget on a
// second full prompt, and the caller already has a model-free fallback that keeps
// compaction working: the point of the summary is quality, not correctness.
func (r *Runner) summarizeSpan(ctx context.Context, opened *session.Session, span session.SeqRange) (string, error) {
	route, client, err := r.resolve(lastSelection(opened))
	if err != nil {
		return "", err
	}
	selection := lastSelection(opened)
	if selection.Provider == "" || selection.Model == "" {
		route, client, err = r.resolve(r.catalog.Default)
		if err != nil {
			return "", err
		}
	}
	transcript := spanTranscript(opened.Events(), span, compactionSummaryInputChars)
	if strings.TrimSpace(transcript) == "" {
		return "", errors.New("the span to summarize is empty")
	}
	callCtx, cancel := context.WithTimeout(ctx, compactionSummaryTimeout)
	defer cancel()
	completion, err := client.Complete(callCtx, llm.Request{
		Model: modelOfRoute(route.Selection.Model),
		Messages: []llm.Message{
			{Role: "system", Content: compactionSummarySystemPrompt},
			{Role: "user", Content: transcript},
		},
		MaxTokens: compactionSummaryMaxTokens,
		SessionID: opened.ID(),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(completion.Text), nil
}

// spanTranscript renders the replaced span as plain text, oldest first.
//
// The cap is applied from the END: the newest part of the span is what the summary
// has to carry forward, and a head-truncated transcript would summarize the oldest
// exchange in the log instead of the one before the kept tail.
func spanTranscript(events []session.Event, span session.SeqRange, maxChars int) string {
	var builder strings.Builder
	for _, event := range events {
		if event.Seq < span.Start || event.Seq > span.End {
			continue
		}
		if event.Type != session.EventUserMessage && event.Type != session.EventAssistantMessage {
			continue
		}
		builder.WriteString(summaryLine(event))
	}
	text := builder.String()
	if len(text) <= maxChars {
		return text
	}
	return text[len(text)-maxChars:]
}

// summaryLine renders one message as a role-prefixed block.
//
// The two kinds are serialized differently: a user message stores its content on
// the event, an assistant message wraps it in `message`. Reading them the same way
// would summarize half the conversation and silently drop the other half.
func summaryLine(event session.Event) string {
	var text string
	if event.Type == session.EventUserMessage {
		var stored transcriptMessage
		if err := json.Unmarshal(event.Data, &stored); err != nil {
			return ""
		}
		text = assistantText(stored.Content)
	} else {
		var stored struct {
			Message transcriptMessage `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &stored); err != nil {
			return ""
		}
		text = assistantText(stored.Message.Content)
	}
	if strings.TrimSpace(text) == "" {
		return ""
	}
	role := "assistant"
	if event.Type == session.EventUserMessage {
		role = "user"
	}
	return role + ": " + text + "\n\n"
}

// compactionSummarySystemPrompt asks for the one thing the next turn needs.
const compactionSummarySystemPrompt = `You compress a coding conversation so work can continue without the full history.

Write a dense summary that preserves, in this order of importance:
1. what the user asked for and any constraint they stated,
2. decisions made and why, including approaches that were rejected,
3. files, paths, commands, and identifiers touched,
4. what was in progress when the history ends, and what the next step is.

Omit pleasantries, restated tool output, and reasoning that led nowhere. Use terse
prose or bullets. Do not invent facts that are not in the transcript. Output only
the summary.`

// modelOfRoute trims a provider-qualified model name to what the wire expects.
func modelOfRoute(model string) string {
	if index := strings.LastIndex(model, "/"); index >= 0 && index+1 < len(model) {
		return model[index+1:]
	}
	return model
}

// oversizedSummary replaces the model-free fallback with a note naming the rule.
//
// A retained node larger than the whole retain budget cannot be made to fit by
// keeping fewer neighbours: the node is the problem. Saying so is the only honest
// outcome, and it keeps the failure diagnosable instead of looking like a
// compaction that silently did nothing.
func oversizedSummary(span session.SeqRange, nodeTokens, budgetTokens int64) string {
	return fmt.Sprintf(
		"(%d earlier messages were compacted. One retained message is %d tokens, more than the %d-token retain budget; it was kept because dropping it would leave no recent context.)",
		span.End-span.Start+1, nodeTokens, budgetTokens)
}

// SummarizeSpan is the summarizer an embedder installs through CompactionConfig.
//
// Exported so the host can install it after the runner exists: the hook closes over
// the runner, and a self-reference inside the construction literal would be nil.
func (r *Runner) SummarizeSpan(ctx context.Context, opened *session.Session, span session.SeqRange) (string, error) {
	return r.summarizeSpan(ctx, opened, span)
}

// SetSummarizer installs the summary hook on an already-built runner.
func (r *Runner) SetSummarizer(fn func(ctx context.Context, opened *session.Session, span session.SeqRange) (string, error)) {
	r.mu.Lock()
	if r.cfg.Compaction == nil {
		r.cfg.Compaction = &CompactionConfig{}
	}
	r.cfg.Compaction.Summarize = fn
	r.mu.Unlock()
}
