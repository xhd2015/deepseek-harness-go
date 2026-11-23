package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// present declares files as the turn's deliverables.
//
// The model uses it once a result is final, so the page can offer the file
// directly instead of making the user search for a path in the transcript. The
// files are not copied or moved: the declaration names what is already on disk.

// presentedFileMax bounds one call. A long list stops being a deliverable list
// and becomes a directory listing.
const presentedFileMax = 8

// presentedFile is one declared deliverable.
type presentedFile struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// presentResult is the tool result: the turn the files belong to and the files.
type presentResult struct {
	Turn  int64           `json:"turn"`
	Files []presentedFile `json:"files"`
}

// present records the declared files on the session.
//
// It refuses a path that is missing or not a regular file: a deliverable the user
// cannot open is worse than no deliverable, because the model has already told
// them it exists.
func (e *ToolExecutor) present(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	_ = ctx
	files, err := decodePresentedFiles(args)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	if inv.Session == nil {
		return ToolOutcome{Text: "present needs an open session", IsError: true}
	}
	base, err := e.baseDir(inv)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	resolved := make([]presentedFile, 0, len(files))
	for _, file := range files {
		target := file.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(base, target)
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return ToolOutcome{
				Text:    fmt.Sprintf("Cannot present %s: file not found. Check the path, create the file if needed, and retry.", file.Path),
				IsError: true,
			}
		}
		if !info.Mode().IsRegular() {
			return ToolOutcome{
				Text:    fmt.Sprintf("Cannot present %s: not a regular file", file.Path),
				IsError: true,
			}
		}
		absolute, absErr := filepath.Abs(target)
		if absErr != nil {
			return ToolOutcome{Text: absErr.Error(), IsError: true}
		}
		resolved = append(resolved, presentedFile{Path: absolute, Description: file.Description})
	}
	turn := currentTurn(inv.Session)
	// The durable event is separate from the tool result: the page reads the
	// event to build the deliverable list, and a result alone would leave the
	// delivery invisible after a reload.
	if _, err := inv.Session.AppendSurface("deliverables/presented", map[string]any{
		"turn":   turn,
		"callId": inv.CallID,
		"files":  resolved,
	}); err != nil {
		return ToolOutcome{Text: "record deliverables: " + err.Error(), IsError: true}
	}
	encoded, marshalErr := json.Marshal(presentResult{Turn: turn, Files: resolved})
	if marshalErr != nil {
		return ToolOutcome{Text: "encode result: " + marshalErr.Error(), IsError: true}
	}
	return ToolOutcome{Text: string(encoded), Meta: map[string]any{"files": resolved}}
}

// decodePresentedFiles reads the declaration and enforces the per-call limit.
func decodePresentedFiles(args map[string]any) ([]presentedFile, error) {
	raw, ok := args["files"]
	if !ok {
		return nil, fmt.Errorf("files is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	var files []presentedFile
	if err := json.Unmarshal(encoded, &files); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("present accepts 1 to %d files", presentedFileMax)
	}
	if len(files) > presentedFileMax {
		return nil, fmt.Errorf("present accepts 1 to %d files", presentedFileMax)
	}
	for i := range files {
		files[i].Path = strings.TrimSpace(files[i].Path)
		if files[i].Path == "" {
			return nil, fmt.Errorf("present requires a non-empty file path")
		}
	}
	return files, nil
}

// currentTurn reports the turn a call belongs to, from the newest turn/start.
func currentTurn(opened *session.Session) int64 {
	var turn int64
	for _, event := range opened.Events() {
		if event.Type != session.EventTurnStart {
			continue
		}
		var payload struct {
			Turn int64 `json:"turn"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && payload.Turn > 0 {
			turn = payload.Turn
		}
	}
	return turn
}

// presentToolName is the DSH tool name.
const presentToolName = "present"

// presentDescription is the DSH wording, verbatim.
const presentDescription = "Declare existing files as final deliverables for the user. " +
	"Hosts may show presented files only after your turn ends, so call it once the files are final. " +
	"If the user needs to review files earlier, such as before a tool call that waits for their answer, link them in your message instead. " +
	"Use it when the user needs a separate file, especially Office documents, spreadsheets, and slide decks; " +
	"prefer your final response when that suffices. The user opens the current files; their contents are not copied."
