// Package agent runs one chat turn at a time per session on top of the raw
// session event log and the llm client.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// DefaultSystemPrompt is the instruction sent ahead of the transcript.
const DefaultSystemPrompt = "You are a helpful assistant. Answer the user directly and concisely."

// ContentPart is one prompt content part.
type ContentPart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"`
	Name      string `json:"name,omitempty"`
}

// TextContent returns the concatenated text parts.
func TextContent(parts []ContentPart) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			if builder.Len() > 0 {
				builder.WriteString("\n\n")
			}
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// Config configures the runner.
type Config struct {
	// Client is the legacy single-route client. It is used for every catalog
	// route when Clients is nil, which keeps tests and single-model setups
	// working.
	Client *llm.Client
	// Clients maps "<provider>\x00<model>" to the client that speaks its wire
	// protocol and endpoint.
	Clients map[string]*llm.Client
	// Catalog is the model surface the runner may route to. A nil catalog is
	// synthesized from Provider/Model/ReasoningEffort.
	Catalog          *modelconfig.Catalog
	Provider         string
	Model            string
	ReasoningEffort  string
	PermissionPreset string
	SandboxMode      string
	ApprovalPolicy   string
	SystemPrompt     string
	// HistoryLimit caps how many transcript messages are sent back; zero keeps
	// the whole transcript.
	HistoryLimit int
	// ToolRoot is the only directory the read-only tools may touch. Empty
	// disables tool use entirely.
	ToolRoot string
	// MaxSteps caps model steps per turn. Zero uses the default of 8.
	MaxSteps int
	Logf     func(format string, args ...any)
}

// PromptRequest is one accepted prompt.
type PromptRequest struct {
	Session        *session.Session
	Content        []ContentPart
	RPCID          string
	ClientTimeZone string
}

// Runner serializes turns per session.
type Runner struct {
	cfg Config

	catalog *modelconfig.Catalog
	clients map[string]*llm.Client
	tools   *ToolExecutor

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	onStatus func(sessionID string, running bool)
}

// New builds a runner.
func New(cfg Config) *Runner {
	if cfg.PermissionPreset == "" {
		cfg.PermissionPreset = "danger-full-access"
	}
	if cfg.SandboxMode == "" {
		cfg.SandboxMode = cfg.PermissionPreset
	}
	if cfg.ApprovalPolicy == "" {
		cfg.ApprovalPolicy = "never"
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = DefaultSystemPrompt
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	tools := NewToolExecutor(cfg.ToolRoot)
	if cfg.ToolRoot != "" && tools.InitError() != nil && cfg.Logf != nil {
		cfg.Logf("tools disabled: %v", tools.InitError())
	}
	if cfg.Provider == "" {
		cfg.Provider = "deepseek"
	}
	catalog := cfg.Catalog
	if catalog == nil {
		catalog = modelconfig.Single(cfg.Provider, cfg.Model, cfg.ReasoningEffort)
	}
	if selection, err := catalog.Resolve(modelconfig.Selection{
		Provider:        cfg.Provider,
		Model:           cfg.Model,
		ReasoningEffort: cfg.ReasoningEffort,
	}); err == nil {
		cfg.Provider = selection.Provider
		cfg.Model = selection.Model
		cfg.ReasoningEffort = selection.ReasoningEffort
	} else {
		cfg.Provider = catalog.Default.Provider
		cfg.Model = catalog.Default.Model
		cfg.ReasoningEffort = catalog.Default.ReasoningEffort
	}
	clients := cfg.Clients
	if clients == nil {
		clients = map[string]*llm.Client{}
	}
	if cfg.Client != nil && len(clients) == 0 {
		for _, provider := range catalog.Providers {
			for _, model := range provider.Models {
				clients[routeKey(provider.ID, model.ID)] = cfg.Client
			}
		}
	}
	return &Runner{
		cfg:     cfg,
		catalog: catalog,
		clients: clients,
		tools:   tools,
		cancels: map[string]context.CancelFunc{},
	}
}

// Catalog returns the model catalog this runner routes over.
func (r *Runner) Catalog() *modelconfig.Catalog { return r.catalog }

// Model returns the configured model name.
func (r *Runner) Model() string { return r.cfg.Model }

// Provider returns the configured provider name.
func (r *Runner) Provider() string { return r.cfg.Provider }

// ReasoningEffort returns the configured reasoning effort, empty when unset.
func (r *Runner) ReasoningEffort() string { return r.cfg.ReasoningEffort }

// Busy reports whether the session is running a turn.
func (r *Runner) Busy(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cancels[sessionID]
	return ok
}

// SetOnStatus installs the callback that observes per-session running-state
// transitions. It must be set before turns start.
func (r *Runner) SetOnStatus(callback func(sessionID string, running bool)) {
	r.mu.Lock()
	r.onStatus = callback
	r.mu.Unlock()
}

// Start begins a turn in the background. It reports false when a turn is
// already running for the session.
func (r *Runner) Start(parent context.Context, req PromptRequest) bool {
	id := req.Session.ID()
	r.mu.Lock()
	if _, running := r.cancels[id]; running {
		r.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancels[id] = cancel
	req.Session.SetBusy(true)
	r.notifyStatusLocked(id, true)
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.cancels, id)
			req.Session.SetBusy(false)
			r.notifyStatusLocked(id, false)
			r.mu.Unlock()
		}()
		r.run(ctx, req)
	}()
	return true
}

// notifyStatusLocked reports one transition while the runner lock serializes
// it against adjacent turns, so a stale false can never overtake a true.
func (r *Runner) notifyStatusLocked(sessionID string, running bool) {
	if r.onStatus != nil {
		r.onStatus(sessionID, running)
	}
}

// Cancel aborts a running turn and reports whether one was running.
func (r *Runner) Cancel(sessionID string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[sessionID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// Close aborts every running turn.
func (r *Runner) Close() {
	r.mu.Lock()
	pending := r.cancels
	r.cancels = map[string]context.CancelFunc{}
	r.mu.Unlock()
	for _, cancel := range pending {
		cancel()
	}
}

func (r *Runner) ensurePreamble(sess *session.Session) error {
	seen := map[string]bool{}
	for _, event := range sess.Events() {
		seen[event.Type] = true
	}
	steps := []struct {
		typ  string
		data any
	}{
		{session.EventPermissionPreset, map[string]any{"preset": r.cfg.PermissionPreset}},
		{session.EventSandboxMode, map[string]any{"mode": r.cfg.SandboxMode}},
		{session.EventApprovalPolicy, map[string]any{"policy": r.cfg.ApprovalPolicy}},
	}
	for _, step := range steps {
		if seen[step.typ] {
			continue
		}
		if _, err := sess.Append(step.typ, step.data); err != nil {
			return err
		}
	}
	// Older sessions predate the model catalog. Give them a valid selection
	// instead of leaving the composer stuck on an unusable route.
	if _, err := r.catalog.Resolve(modelconfig.Selection{
		Provider:        selectionProvider(sess),
		Model:           selectionModel(sess),
		ReasoningEffort: selectionEffort(sess),
	}); err != nil || !seen[session.EventModelSelection] {
		selection := r.catalog.Default
		if _, err := sess.Append(session.EventModelSelection, session.ModelSelection{
			Provider:        selection.Provider,
			Model:           selection.Model,
			ReasoningEffort: selection.ReasoningEffort,
		}); err != nil {
			return err
		}
	}
	return nil
}

// currentSelection reads the last selection that this catalog can serve.
func (r *Runner) currentSelection(sess *session.Session) modelconfig.Selection {
	var (
		selection modelconfig.Selection
		seq       int64 = -1
	)
	for _, event := range sess.Events() {
		if event.Type != session.EventModelSelection || event.Seq <= seq {
			continue
		}
		var stored session.ModelSelection
		if err := json.Unmarshal(event.Data, &stored); err != nil {
			continue
		}
		candidate := modelconfig.Selection{
			Provider:        stored.Provider,
			Model:           stored.Model,
			ReasoningEffort: stored.ReasoningEffort,
		}
		if _, err := r.catalog.Resolve(candidate); err != nil {
			continue
		}
		selection, seq = candidate, event.Seq
	}
	if seq >= 0 {
		return selection
	}
	return r.catalog.Default
}

func (r *Runner) resolve(selection modelconfig.Selection) (modelconfig.Route, *llm.Client, error) {
	route, err := r.catalog.Resolve(selection)
	if err != nil {
		return modelconfig.Route{}, nil, err
	}
	client := r.clients[routeKey(route.Provider, route.Model)]
	if client == nil {
		return modelconfig.Route{}, nil, fmt.Errorf("no client configured for %s/%s", route.Provider, route.Model)
	}
	return route, client, nil
}

func routeKey(provider, model string) string { return provider + "\x00" + model }

func selectionProvider(sess *session.Session) string { return lastSelection(sess).Provider }
func selectionModel(sess *session.Session) string    { return lastSelection(sess).Model }
func selectionEffort(sess *session.Session) string   { return lastSelection(sess).ReasoningEffort }

func lastSelection(sess *session.Session) modelconfig.Selection {
	var selection modelconfig.Selection
	for _, event := range sess.Events() {
		if event.Type != session.EventModelSelection {
			continue
		}
		var stored session.ModelSelection
		if err := json.Unmarshal(event.Data, &stored); err != nil {
			continue
		}
		selection = modelconfig.Selection{
			Provider:        stored.Provider,
			Model:           stored.Model,
			ReasoningEffort: stored.ReasoningEffort,
		}
	}
	return selection
}

func (r *Runner) startTurn(sess *session.Session, req PromptRequest, turn int, text string) error {
	if _, err := sess.Append(session.EventTurnStart, map[string]any{"turn": turn}); err != nil {
		return err
	}
	content := contentPartsJSON(req.Content)
	if content == nil {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}
	message, err := sess.AppendSurface(session.EventUserMessage, map[string]any{
		"content": content,
		"source": map[string]any{
			"kind":           "user",
			"rpcId":          req.RPCID,
			"clientTimeZone": req.ClientTimeZone,
		},
		"role": "user",
		"id":   session.NewID(),
	})
	if err != nil {
		return err
	}
	if err := r.ensureTitle(sess, text, message.Seq); err != nil {
		r.logf("session %s: title: %v", sess.ID(), err)
	}
	if _, err := sess.Append(session.EventStepStart, map[string]any{"turn": turn, "step": 1}); err != nil {
		return err
	}
	return nil
}

func (r *Runner) ensureTitle(sess *session.Session, text string, messageSeq int64) error {
	for _, event := range sess.Events() {
		if event.Type == session.EventSessionTitle {
			return nil
		}
	}
	title := firstLine(text)
	if title == "" {
		title = "New session"
	}
	_, err := sess.Append(session.EventSessionTitle, map[string]any{
		"title":       title,
		"messageSeqs": []int64{messageSeq},
		"source":      map[string]any{"kind": "fallback"},
	})
	return err
}

func failureOf(err error) map[string]any {
	var failure *llm.Failure
	if errors.As(err, &failure) && failure != nil {
		code := failure.Code
		if code == "" {
			code = "UNKNOWN"
		}
		return map[string]any{"code": code, "message": failure.Message}
	}
	return map[string]any{"code": "UNKNOWN", "message": err.Error()}
}

func contentPartsJSON(parts []ContentPart) []any {
	var out []any
	for _, part := range parts {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": part.Text})
		case "image":
			out = append(out, map[string]any{
				"type":      "image",
				"mediaType": part.MediaType,
				"data":      part.Data,
				"name":      part.Name,
			})
		default:
			continue
		}
	}
	return out
}

func nextTurn(events []session.Event) int {
	turn := 0
	for _, event := range events {
		if event.Type == session.EventTurnStart {
			var data struct {
				Turn int `json:"turn"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil && data.Turn > turn {
				turn = data.Turn
			}
		}
	}
	return turn + 1
}

func firstLine(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if utf8.RuneCountInString(line) > 80 {
		runes := []rune(line)
		line = string(runes[:80]) + "…"
	}
	return line
}

func (r *Runner) logf(format string, args ...any) {
	if r.cfg.Logf != nil {
		r.cfg.Logf(format, args...)
	}
}
