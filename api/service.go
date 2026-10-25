// Package api maps the Gateway wire endpoints the web client opens onto the
// session store and the turn runner.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/session"
	"github.com/xhd2015/deepseek-harness-go/workspace"
)

// Gateway-internal endpoints.
const (
	EndpointInvoke = "$invoke"
	EndpointEvents = "$events"
)

// Error codes reported by this backend.
const (
	CodeBadRequest    = "gateway/bad-request"
	CodeInternal      = "gateway/internal"
	CodeUnimplemented = "unimplemented"
	CodeNotFound      = "session/not-found"
	CodeBusy          = "session/busy"
)

// Config configures the service.
type Config struct {
	Store       *session.Store
	Runner      *agent.Runner
	Catalog     *modelconfig.Catalog
	DefaultCwd  string
	AgentPreset string
	HomeDir     string
	// DSHHome is the harness state directory holding attachments. It defaults
	// to HomeDir/.dsh-go, the layout the runner uses.
	DSHHome string
	// Workspaces holds the directories the client groups Sessions by. A nil
	// registry gets a fresh one seeded with DefaultCwd.
	Workspaces *workspace.Registry
	Logf       func(format string, args ...any)
}

// Service serves mux streams for the web client.
type Service struct {
	cfg Config
	// Workspaces is the Workspace registry this service answers with.
	Workspaces *workspace.Registry
	drafts     drafts
	events     *eventHub
}

// New builds the endpoint service.
func New(cfg Config) *Service {
	if cfg.HomeDir == "" {
		cfg.HomeDir, _ = os.UserHomeDir()
	}
	if cfg.Workspaces == nil {
		cfg.Workspaces = workspace.NewRegistry()
	}
	if cfg.DefaultCwd != "" {
		cfg.Workspaces.Add(cfg.DefaultCwd)
	}
	service := &Service{cfg: cfg, Workspaces: cfg.Workspaces, events: newEventHub()}
	service.drafts.texts = map[string]string{}
	if cfg.Runner != nil {
		cfg.Runner.SetOnStatus(func(sessionID string, running bool) {
			service.Emit(EventSessionStatus, sessionID, running)
		})
	}
	return service
}

// EventSessionStatus is the forwarded event that carries one Agent's
// running-state transition.
const EventSessionStatus = "api-session/status"

// Emit forwards one process-local event to every connected browser.
func (s *Service) Emit(event string, args ...any) {
	if s.events == nil || event == "" {
		return
	}
	frame, err := encodeValue(map[string]any{
		"type":  "emit",
		"event": event,
		"args":  args,
	})
	if err != nil {
		s.logf("encode forwarded event %q: %v", event, err)
		return
	}
	s.events.publish(frame)
}

// Open dispatches one logical stream.
func (s *Service) Open(ctx context.Context, endpoint string, payload []byte, uplink mux.Uplink) (mux.Stream, error) {
	args, err := envelopeArgs(payload)
	if err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	switch endpoint {
	case EndpointInvoke:
		return s.openInvoke(args)
	case EndpointEvents:
		ready, err := encodeValue(map[string]any{
			"type":     "ready",
			"clientId": session.NewID(),
			"host":     map[string]any{"home": s.cfg.HomeDir},
		})
		if err != nil {
			return nil, fail(CodeInternal, err.Error())
		}
		feed := s.events.subscribe()
		return &eventsStream{
			ready:  ready,
			feed:   feed,
			cancel: func() { s.events.unsubscribe(feed) },
		}, nil
	case "session/follow":
		return s.follow(ctx, args)
	case "session/control":
		return s.control()
	case "workspace/follow":
		return s.followWorkspaces(ctx)
	}
	return nil, s.unimplemented(endpoint)
}

// unary answers one Gateway unary method.
func (s *Service) unary(endpoint string, args json.RawMessage) (any, error) {
	switch endpoint {
	case "session/list":
		return s.list(args)
	case "session/create":
		return s.create(args)
	case "session/rename":
		return s.rename(args)
	case "session/prompt":
		return s.prompt(args)
	case "session/cancel":
		return s.cancel(args)
	case "session/page":
		return s.page(args)
	case "session/projections":
		return s.projections(args)
	case "session/modelCatalog":
		return s.modelCatalog(args)
	case "session/selectModel":
		return s.selectModel(args)
	case "session/getDraft":
		return s.getDraft(args)
	case "session/setDraft":
		return s.setDraft(args)
	case "session/attachment":
		return s.attachment(args)
	case "workspace/initializeDefault":
		return s.initializeDefaultWorkspace(args)
	case "workspace/create":
		return s.createWorkspace(args)
	}
	return nil, s.unimplemented(endpoint)
}

// openInvoke serves the one-item unary mux stream.
func (s *Service) openInvoke(args json.RawMessage) (mux.Stream, error) {
	var request struct {
		Endpoint string          `json:"endpoint"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := decodeArgs(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if request.Endpoint == "" {
		return nil, fail(CodeBadRequest, "unary request is missing endpoint")
	}
	inner, err := envelopeArgs(request.Payload)
	if err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	value, err := s.unary(request.Endpoint, inner)
	if err != nil {
		var failure *mux.Failure
		if !errors.As(err, &failure) {
			failure = fail(CodeInternal, err.Error())
		}
		if failure.Details == nil {
			failure.Details = map[string]any{}
		}
		item, encodeErr := encodeValue(map[string]any{"ok": false, "error": failure})
		if encodeErr != nil {
			return nil, fail(CodeInternal, encodeErr.Error())
		}
		return valueStream(item), nil
	}
	item, err := encodeValue(map[string]any{"ok": true, "value": value})
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return valueStream(item), nil
}

// envelopeArgs validates the `{"args": {...}}` wire envelope every Remote
// request uses and returns the inner args object.
func envelopeArgs(payload []byte) (json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, errors.New("request payload is empty")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("request payload is not an object: %w", err)
	}
	if len(envelope) != 1 {
		return nil, errors.New("request payload must contain exactly one args field")
	}
	args, ok := envelope["args"]
	if !ok {
		return nil, errors.New("request payload must contain exactly one args field")
	}
	trimmed := strings.TrimSpace(string(args))
	if !strings.HasPrefix(trimmed, "{") {
		return nil, errors.New("request args must be an object")
	}
	return args, nil
}

// decodeRequest unwraps the single positional parameter the typed Remote client
// sends for one-argument methods (`{"request": {...}}`) and decodes it. A
// missing wrapper is an error so a shape change can never be mistaken for an
// empty request.
func decodeRequest(args json.RawMessage, target any) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(args, &envelope); err != nil {
		return fmt.Errorf("request args are not an object: %w", err)
	}
	raw, ok := envelope["request"]
	if !ok || len(envelope) != 1 {
		return errors.New("request args must contain exactly one request field")
	}
	return decodeArgs(raw, target)
}

// decodeArgs unmarshals one args object.
func decodeArgs(args json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(args)))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request args: %w", err)
	}
	return nil
}

func (s *Service) unimplemented(endpoint string) error {
	s.logf("unimplemented endpoint %q", endpoint)
	return fail(CodeUnimplemented, fmt.Sprintf("endpoint %q is not implemented by dsh-go", endpoint))
}

// fail builds a structured wire failure.
func fail(code, message string) *mux.Failure {
	return &mux.Failure{Code: code, Message: message, Details: map[string]any{}}
}

// notFound reports an unknown session id, carrying it in details the way the
// client's session/not-found error expects.
func notFound(sessionID string) error {
	return &mux.Failure{
		Code:    CodeNotFound,
		Message: fmt.Sprintf("session %s does not exist", sessionID),
		Details: map[string]any{"sessionId": sessionID},
	}
}

func (s *Service) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

// address is SessionAddress.
type address struct {
	Kind            string `json:"kind"`
	SessionID       string `json:"sessionId,omitempty"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	ChildSessionID  string `json:"childSessionId,omitempty"`
	Mode            string `json:"mode,omitempty"`
}

// resolveAddress opens the session one page or follow request names.
func (s *Service) resolveAddress(value address) (*session.Session, error) {
	switch value.Kind {
	case "", "session":
		if strings.TrimSpace(value.SessionID) == "" {
			return nil, fail(CodeBadRequest, "address is missing sessionId")
		}
		return s.openSession(value.SessionID)
	case "subagent":
		return nil, fail(CodeUnimplemented, "subagent sessions are not implemented by dsh-go")
	}
	return nil, fail(CodeBadRequest, fmt.Sprintf("unsupported session address kind %q", value.Kind))
}

func (s *Service) openSession(id string) (*session.Session, error) {
	opened, err := s.cfg.Store.Open(id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return nil, notFound(id)
		}
		return nil, fail(CodeInternal, err.Error())
	}
	return opened, nil
}

// turnWindow is SessionPageRequest.turnWindow.
type turnWindow struct {
	MinMessages int `json:"minMessages"`
	MinTurns    int `json:"minTurns"`
}

// pageRequest is SessionPageRequest.
type pageRequest struct {
	Address     address     `json:"address"`
	ThroughSeq  *int64      `json:"throughSeq"`
	BeforeSeq   *int64      `json:"beforeSeq"`
	MaxMessages int         `json:"maxMessages"`
	TurnWindow  *turnWindow `json:"turnWindow"`
}

func (request pageRequest) pageRequest() session.PageRequest {
	out := session.PageRequest{MaxMessages: request.MaxMessages}
	if request.BeforeSeq != nil {
		out.BeforeSeq = request.BeforeSeq
	}
	if request.TurnWindow != nil {
		out.TurnWindow = &session.TurnWindow{
			MinMessages: request.TurnWindow.MinMessages,
			MinTurns:    request.TurnWindow.MinTurns,
		}
	}
	return out
}

// pageOf slices one window out of the durable log.
func pageOf(request pageRequest, events []session.Event) session.Page {
	through := int64(-1)
	if len(events) > 0 {
		through = events[len(events)-1].Seq
	}
	if request.ThroughSeq != nil {
		through = *request.ThroughSeq
	}
	options := request.pageRequest()
	options.ThroughSeq = through
	return session.PageEvents(events, options)
}
