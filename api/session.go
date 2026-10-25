package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// list answers session/list.
func (s *Service) list(json.RawMessage) (any, error) {
	entries, err := s.cfg.Store.List()
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		items = append(items, summaryOf(entry))
	}
	return map[string]any{"items": items}, nil
}

// summaryOf renders one SessionSummary row.
func summaryOf(entry session.ListEntry) map[string]any {
	summary := map[string]any{
		"agentAvailable": true,
		"sessionId":      entry.Header.ID,
		"updatedAt":      entry.UpdatedAt,
		"running":        entry.Busy,
		"blank":          entry.Blank,
		"cwd":            entry.Header.Cwd,
		"projections":    session.SummaryProjections(entry),
	}
	if entry.Header.ParentSession != "" {
		summary["parentSessionId"] = entry.Header.ParentSession
	}
	if entry.Header.Origin != "" {
		summary["origin"] = entry.Header.Origin
	}
	return summary
}

// createRequest is SessionCreateRequest.
type createRequest struct {
	WorkspaceID string `json:"workspaceId"`
	Cwd         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	AgentPreset string `json:"agentPreset"`
}

// create answers session/create.
func (s *Service) create(args json.RawMessage) (any, error) {
	var request createRequest
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if request.WorkspaceID != "" && strings.TrimSpace(request.Cwd) != "" {
		return nil, fail(CodeBadRequest, "session.create accepts workspaceId or cwd, not both")
	}
	cwd := strings.TrimSpace(request.Cwd)
	if request.WorkspaceID != "" {
		item, ok := s.Workspaces.Get(request.WorkspaceID)
		if !ok {
			return nil, &mux.Failure{
				Code:    CodeWorkspaceNotFound,
				Message: fmt.Sprintf("workspace %q does not exist", request.WorkspaceID),
				Details: map[string]any{"workspaceId": request.WorkspaceID},
			}
		}
		cwd = item.Path
	}
	if cwd == "" {
		cwd = s.defaultCwd()
	}
	preset := strings.TrimSpace(request.AgentPreset)
	if preset == "" {
		preset = s.cfg.AgentPreset
	}
	var (
		opened *session.Session
		err    error
	)
	if request.SessionID != "" {
		opened, err = s.cfg.Store.CreateWithID(cwd, preset, request.SessionID)
	} else {
		opened, err = s.cfg.Store.Create(cwd, preset)
	}
	if err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if _, created := s.Workspaces.Add(cwd); !created {
		s.Workspaces.Touch(cwd)
	}
	return map[string]any{"sessionId": opened.ID(), "agentPreset": preset}, nil
}

// rename answers session/rename.
func (s *Service) rename(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
		Title     string `json:"title"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return nil, fail(CodeBadRequest, "title is required")
	}
	event, err := opened.AppendSurface(session.EventSessionTitle, map[string]any{
		"title":       title,
		"messageSeqs": []int64{},
		"source":      map[string]any{"kind": "user"},
	})
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return map[string]any{"title": title, "seq": event.Seq}, nil
}

// prompt answers session/prompt.
func (s *Service) prompt(args json.RawMessage) (any, error) {
	var request struct {
		RequestID      string              `json:"requestId"`
		SessionID      string              `json:"sessionId"`
		Mode           string              `json:"mode"`
		Content        []agent.ContentPart `json:"content"`
		ClientTimeZone string              `json:"clientTimeZone"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	parts := make([]agent.ContentPart, 0, len(request.Content))
	for _, part := range request.Content {
		switch part.Type {
		case "text":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			parts = append(parts, part)
		case "image", "file":
			return nil, fail(CodeUnimplemented, fmt.Sprintf("%s prompt content is not implemented by dsh-go", part.Type))
		default:
			return nil, fail(CodeBadRequest, fmt.Sprintf("unsupported prompt content type %q", part.Type))
		}
	}
	if len(parts) == 0 {
		return nil, fail(CodeBadRequest, "prompt needs at least one text part")
	}
	if s.cfg.Runner == nil {
		return nil, fail(CodeUnimplemented, "prompting is not configured")
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	if request.Mode == "steer" {
		s.cfg.Runner.Cancel(request.SessionID)
	}
	started := s.cfg.Runner.Start(context.Background(), agent.PromptRequest{
		Session:        opened,
		Content:        parts,
		RPCID:          request.RequestID,
		ClientTimeZone: request.ClientTimeZone,
	})
	if !started {
		return nil, &mux.Failure{
			Code:    CodeBusy,
			Message: "session is already running a turn",
			Details: map[string]any{"sessionId": request.SessionID},
		}
	}
	return map[string]any{"accepted": true}, nil
}

// cancel answers session/cancel.
func (s *Service) cancel(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if s.cfg.Runner != nil {
		s.cfg.Runner.Cancel(request.SessionID)
	}
	return map[string]any{"accepted": true}, nil
}

// page answers session/page.
func (s *Service) page(args json.RawMessage) (any, error) {
	var request pageRequest
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	_, err := s.resolveAddress(request.Address)
	if err != nil {
		return nil, err
	}
	events, err := s.eventsOf(request.Address.SessionID)
	if err != nil {
		return nil, err
	}
	window := pageOf(request, events)
	return map[string]any{
		"records": recordsOf(window.Records),
		"hasMore": window.HasMore,
	}, nil
}

// projections answers session/projections.
func (s *Service) projections(args json.RawMessage) (any, error) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	baseline := session.Baseline(opened.Header(), opened.Events())
	return map[string]any{
		"kind":    "sequenced",
		"asOfSeq": baseline.AsOfSeq,
		"values":  baseline.Values,
	}, nil
}

// follow answers session/follow with a snapshot plus the live tail.
func (s *Service) follow(ctx context.Context, args json.RawMessage) (mux.Stream, error) {
	var request struct {
		Address         address     `json:"address"`
		MaxMessages     int         `json:"maxMessages"`
		TurnWindow      *turnWindow `json:"turnWindow"`
		AssistantStream bool        `json:"assistantStream"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	opened, err := s.resolveAddress(request.Address)
	if err != nil {
		return nil, err
	}
	events, assistantBaseline, live, cancel := opened.FollowSnapshot()

	cursor := int64(-1)
	if len(events) > 0 {
		cursor = events[len(events)-1].Seq
	}
	window := session.PageEvents(events, session.PageRequest{
		ThroughSeq:  cursor,
		MaxMessages: request.MaxMessages,
		TurnWindow:  turnWindowOf(request.TurnWindow),
	})
	baseline := session.Baseline(opened.Header(), events)
	snapshotData := map[string]any{
		"type":        "snapshot",
		"header":      opened.Header().Wire(),
		"cursor":      cursor,
		"records":     recordsOf(window.Records),
		"hasMore":     window.HasMore,
		"projections": baseline,
	}
	if request.AssistantStream {
		// The web client opts into assistant presentation frames and rejects a
		// snapshot that omits the baseline, even when no attempt is active.
		snapshotData["assistantStream"] = assistantBaseline
	}
	snapshot, err := encodeValue(snapshotData)
	if err != nil {
		cancel()
		return nil, fail(CodeInternal, err.Error())
	}
	s.logf("follow %s from cursor %d", request.Address.SessionID, cursor)
	return &sessionEventStream{
		snapshot: snapshot,
		feed:     live,
		cancel:   cancel,
	}, nil
}

// control answers session/control with one empty projection baseline.
func (s *Service) control() (mux.Stream, error) {
	baseline, err := encodeValue(map[string]any{
		"type":  "baseline",
		"value": map[string]any{"projections": map[string]any{}},
	})
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return &controlStream{baseline: baseline}, nil
}

func turnWindowOf(window *turnWindow) *session.TurnWindow {
	if window == nil {
		return nil
	}
	return &session.TurnWindow{MinMessages: window.MinMessages, MinTurns: window.MinTurns}
}

// eventsOf reads the durable log of one known session.
func (s *Service) eventsOf(id string) ([]session.Event, error) {
	opened, err := s.openSession(id)
	if err != nil {
		return nil, err
	}
	return opened.Events(), nil
}

func (s *Service) defaultCwd() string {
	if s.cfg.DefaultCwd != "" {
		return s.cfg.DefaultCwd
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "/"
}

// attachment answers session/attachment. Only attachments referenced by the
// session are served, so a caller cannot probe unrelated content-addressed
// objects by id.
func (s *Service) attachment(args json.RawMessage) (any, error) {
	var request struct {
		SessionID    string `json:"sessionId"`
		AttachmentID string `json:"attachmentId"`
	}
	if err := decodeArgs(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if strings.TrimSpace(request.AttachmentID) == "" {
		return nil, fail(CodeBadRequest, "attachmentId is required")
	}
	events, err := s.eventsOf(request.SessionID)
	if err != nil {
		return nil, err
	}
	ref, referenced := referencedAttachment(events, request.AttachmentID)
	if !referenced {
		return nil, fail("ATTACHMENT_NOT_REFERENCED", "Image is not referenced by this session.")
	}
	home := s.cfg.DSHHome
	if home == "" {
		home = filepath.Join(s.cfg.HomeDir, ".dsh-go")
	}
	data, err := agent.LoadStoredAttachment(home, request.AttachmentID)
	if err != nil {
		return nil, fail(CodeInternal, fmt.Sprintf("cannot read attachment %s: %v", request.AttachmentID, err))
	}
	return map[string]any{
		"attachment": ref,
		"data":       base64.StdEncoding.EncodeToString(data),
	}, nil
}

// referencedAttachment finds the durable attachment reference in a session's
// events. Tool results carry the full ref next to the image block.
func referencedAttachment(events []session.Event, attachmentID string) (map[string]any, bool) {
	for _, event := range events {
		var value any
		if err := json.Unmarshal(event.Data, &value); err != nil {
			continue
		}
		if ref := findAttachmentValue(value, attachmentID); ref != nil {
			return ref, true
		}
	}
	return nil, false
}

func findAttachmentValue(value any, attachmentID string) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		if id, _ := typed["attachmentId"].(string); id == attachmentID {
			return typed
		}
		for _, child := range typed {
			if ref := findAttachmentValue(child, attachmentID); ref != nil {
				return ref
			}
		}
	case []any:
		for _, child := range typed {
			if ref := findAttachmentValue(child, attachmentID); ref != nil {
				return ref
			}
		}
	}
	return nil
}
