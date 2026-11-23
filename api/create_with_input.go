package api

import (
	"encoding/json"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// createWithInput creates a Session and leaves its first input in one request.
//
// The point is the round trip: a caller that had to create and then prompt would
// hold a new id across two calls, and a failure between them would leave a session
// nobody is using. The rules below are DSH's, adopted verbatim rather than
// re-derived, because a client written against a native host sends exactly these
// combinations.

// encodedImage is one image a caller attaches to a draft or a prompt.
type encodedImage struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
	Name      string `json:"name"`
}

// createWithInput answers session/createWithInput.
func (s *Service) createWithInput(args json.RawMessage) (any, error) {
	// The wire shape is `{request: {...}}`, the same envelope as every other
	// method. The create fields are read out of that object rather than embedded in
	// a struct, because an embedded struct cannot also carry the outer `request`
	// key the decoder insists on.
	var request struct {
		WorkspaceID    string                  `json:"workspaceId"`
		Cwd            string                  `json:"cwd"`
		SessionID      string                  `json:"sessionId"`
		AgentPreset    string                  `json:"agentPreset"`
		ModelSelection *session.ModelSelection `json:"modelSelection"`
		Draft          string                  `json:"draft"`
		DraftImages    []encodedImage          `json:"draftImages"`
		Prompt         string                  `json:"prompt"`
		Images         []encodedImage          `json:"images"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	draft := request.Draft
	promptText := request.Prompt
	wantsDraft := strings.TrimSpace(draft) != "" || len(request.DraftImages) > 0
	wantsPrompt := strings.TrimSpace(promptText) != "" || len(request.Images) > 0
	if wantsDraft && wantsPrompt {
		return nil, fail(CodeBadRequest, "createWithInput takes a draft or a prompt, not both")
	}
	if len(request.Images) > 0 && strings.TrimSpace(promptText) == "" {
		// An image is part of a message; without text there is no message to attach
		// it to. A draft is different: it may hold images alone.
		return nil, fail(CodeBadRequest, "createWithInput needs prompt text to carry prompt images")
	}

	// The session is created through the same path as session/create, so preset,
	// model selection and workspace resolution behave identically.
	createArgs, err := json.Marshal(map[string]any{"request": map[string]any{
		"workspaceId":    request.WorkspaceID,
		"cwd":            request.Cwd,
		"sessionId":      request.SessionID,
		"agentPreset":    request.AgentPreset,
		"modelSelection": request.ModelSelection,
	}})
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	created, err := s.create(createArgs)
	if err != nil {
		return nil, err
	}
	sessionID := sessionIDOfCreate(created)
	if sessionID == "" {
		// Without an id there is nothing to attach input to, and inventing a
		// success would hide the real problem.
		return created, nil
	}
	if !wantsDraft && !wantsPrompt {
		return created, nil
	}
	if wantsDraft {
		if err := s.drafts.Set(sessionID, draft); err != nil {
			return withInputError(created, err), nil
		}
		return created, nil
	}

	parts := make([]agent.ContentPart, 0, len(request.Images)+1)
	if strings.TrimSpace(promptText) != "" {
		parts = append(parts, agent.ContentPart{Type: "text", Text: promptText})
	}
	for _, image := range request.Images {
		parts = append(parts, agent.ContentPart{
			Type:      "image",
			MediaType: image.MediaType,
			Data:      image.Data,
			Name:      image.Name,
		})
	}
	promptArgs, err := json.Marshal(map[string]any{
		"request": map[string]any{
			"sessionId": sessionID,
			"requestId": session.NewID(),
			"content":   parts,
		},
	})
	if err != nil {
		return withInputError(created, err), nil
	}
	if _, err := s.prompt(promptArgs); err != nil {
		// The session stays: its id is how the caller retries the input, and
		// discarding it would leave a second session behind on the retry.
		return withInputError(created, err), nil
	}
	return created, nil
}

// withInputError attaches the failure to the created session without discarding it.
func withInputError(created any, err error) map[string]any {
	value := map[string]any{}
	if record, ok := created.(map[string]any); ok {
		for key, item := range record {
			value[key] = item
		}
	}
	value["inputError"] = inputErrorMessage(err)
	return value
}

// inputErrorMessage renders one input failure the way DSH reports it.
func inputErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if failure, ok := err.(*mux.Failure); ok && failure.Message != "" {
		return failure.Message
	}
	return err.Error()
}

// sessionIDOfCreate reads the id out of a session/create reply.
func sessionIDOfCreate(created any) string {
	record, ok := created.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := record["sessionId"].(string)
	return strings.TrimSpace(id)
}
