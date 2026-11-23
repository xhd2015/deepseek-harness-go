package api

import (
	"encoding/json"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// Human commands are lines like `/compact`. The page sends the line and expects
// the command to happen; this backend answers the ones it implements and says so
// plainly for the rest, because a slash command that silently does nothing is
// indistinguishable from one that did nothing because there was nothing to do.

// supportedCommand is one slash command this backend implements.
type supportedCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Argumentless commands reject trailing input rather than ignoring it: a
	// `/compact foo` that quietly compacts would confirm a misuse.
	Argumentless bool `json:"argumentless,omitempty"`
}

// supportedCommands lists what this backend can run.
var supportedCommands = []supportedCommand{
	{Name: "compact", Description: "Compact older conversation history", Argumentless: true},
}

// commandsList answers commands/list.
func (s *Service) commandsList(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	return map[string]any{"commands": supportedCommands}, nil
}

// commandsExecute answers commands/execute: run one slash command.
//
// The lifecycle is recorded first, so a session read back lists the command even
// when it failed — the user typed it, and the log should say what happened.
func (s *Service) commandsExecute(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Line      string `json:"line"`
	}
	// The page sends this one flat, like the question answer: three named
	// parameters rather than the single `request` object most methods use.
	if err := decodeEitherShape(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	line := strings.TrimSpace(request.Line)
	if !strings.HasPrefix(line, "/") {
		// Not a command at all: the caller should have sent a prompt.
		return nil, nil
	}
	name, rawInput := parseCommandLine(line)
	var command *supportedCommand
	for i := range supportedCommands {
		if supportedCommands[i].Name == name {
			command = &supportedCommands[i]
			break
		}
	}
	if command == nil {
		return nil, nil
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	commandID := "command-" + session.NewID()
	if _, err := opened.Append(session.EventCommandRun, map[string]any{
		"commandId": commandID,
		"name":      name,
		"args":      rawInput,
		"source":    map[string]any{"kind": "user"},
	}); err != nil {
		return nil, fail(CodeInternal, "record command: "+err.Error())
	}
	settle := func(kind, text string, sourceSeq int64) (any, error) {
		payload := map[string]any{"commandId": commandID, "kind": kind}
		if text != "" {
			payload["text"] = text
		}
		if sourceSeq > 0 {
			payload["sourceEventSeq"] = sourceSeq
		}
		if _, appendErr := opened.Append(session.EventCommandDone, payload); appendErr != nil {
			return nil, fail(CodeInternal, "record command result: "+appendErr.Error())
		}
		return map[string]any{
			"commandId": commandID,
			"result":    map[string]any{"kind": kind, "text": text},
		}, nil
	}

	switch name {
	case "compact":
		if rawInput != "" {
			return settle("error", "Usage: /compact (no arguments)", 0)
		}
		if s.cfg.Runner == nil {
			return settle("error", "Compaction is unavailable: this backend has no runner.", 0)
		}
		compacted, compactErr := s.cfg.Runner.CompactNow(request.SessionID)
		if compactErr != nil {
			return settle("error", compactErr.Error(), 0)
		}
		if !compacted {
			return settle("success", "No compactable history yet.", 0)
		}
		// The source event is the summary's seq: the page scrolls to it.
		state := session.FoldCompaction(opened.Events())
		return settle("success", "Compacted older conversation history.", state.LastEndSeq)
	default:
		return nil, nil
	}
}

// parseCommandLine splits `/name args` into its two parts.
func parseCommandLine(line string) (name, rawInput string) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(line), "/")
	parsed, rest, _ := strings.Cut(trimmed, " ")
	return strings.TrimSpace(parsed), strings.TrimSpace(rest)
}

// runPromptCommand executes a slash command that arrived as prompt text.
//
// It reports handled=false for anything that is not a command this backend knows,
// so ordinary messages beginning with a slash still reach the model instead of
// being swallowed.
func (s *Service) runPromptCommand(sessionID, text string) (bool, any, error) {
	name, rawInput := parseCommandLine(text)
	known := false
	for i := range supportedCommands {
		if supportedCommands[i].Name == name {
			known = true
			break
		}
	}
	if !known {
		return false, nil, nil
	}
	body, err := json.Marshal(map[string]any{"sessionId": sessionID, "line": text})
	if err != nil {
		return true, nil, fail(CodeInternal, err.Error())
	}
	value, err := s.commandsExecute(body)
	if err != nil {
		return true, nil, err
	}
	_ = rawInput
	return true, value, nil
}
