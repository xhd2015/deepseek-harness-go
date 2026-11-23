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
	"time"
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
	// Compaction configures the pressure-driven compaction of long sessions. Nil
	// leaves it off, which is what a caller that manages its own window wants.
	Compaction *CompactionConfig
	// Store opens a session by id for work that has no handle in hand, such as a
	// manual compaction request. Nil disables the manual path.
	Store *session.Store
	// Skills is the catalog the skill tool may load from.
	Skills []SkillDefinition
	// ResolveSkills re-resolves the catalog for a session whose cwd differs from
	// the boot directory. Nil keeps the boot-time catalog.
	ResolveSkills func(cwd string) []SkillDefinition
	// ToolRoot is the fallback workspace for tool calls whose session has no
	// recorded cwd, and the directory validated at startup. It does not limit
	// reads. Empty uses the process working directory.
	ToolRoot string
	// DSHHome is the harness home exported to bash children as DSH_HOME.
	// Empty omits the variable.
	DSHHome string
	// ModelsPath is the patch file the catalog was loaded from. Empty means the
	// catalog came from the single-route fallback.
	ModelsPath string
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
	// Source replaces the default user-message source. Background-job
	// completion notices use it to identify themselves as tool-jobs notices.
	Source map[string]any
	// Continuation opens a turn without appending a user message. History
	// injection and editing use it to let the agent act on committed events.
	Continuation bool
}

// Runner serializes turns per session.
type Runner struct {
	cfg Config

	catalog *modelconfig.Catalog
	clients map[string]*llm.Client
	tools   *ToolExecutor

	mu             sync.Mutex
	cancels        map[string]context.CancelFunc
	onStatus       func(sessionID string, running bool)
	pendingNotices map[string][]string
	waking         map[string]bool
	// pendingFollowup marks a session whose next turn is already committed to the
	// log. The follow-up must start only after the current turn has fully unwound,
	// because Start refuses a session that is still running.
	pendingFollowup map[string]bool
	// approvals carries pending escalation questions between a tool call and the
	// page that answers it.
	approvals *approvalGate
	// answerer, when set, decides approvals in-process instead of waiting for the
	// page. Nil means the page owns the decision.
	answerer func(ApprovalRequest) ApprovalOutcome
	// notifyApproval tells the page a question is waiting.
	notifyApproval func(ApprovalRequest)
	// approvalWait bounds how long a call waits for an answer before failing closed.
	approvalWait time.Duration
}

// New builds a runner.
func New(cfg Config) *Runner {
	// A new session starts where DSH's web/base bundle does. Starting wider would
	// be a drift in the unsafe direction: the user would be told nothing and a
	// workspace-write profile would silently not apply.
	if cfg.PermissionPreset == "" {
		cfg.PermissionPreset = session.PresetWorkspaceWrite
	}
	if cfg.SandboxMode == "" {
		cfg.SandboxMode = session.SandboxWorkspaceWrite
	}
	if cfg.ApprovalPolicy == "" {
		cfg.ApprovalPolicy = session.ApprovalAsk
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = DefaultSystemPrompt
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	tools := NewToolExecutor(cfg.ToolRoot)
	tools.SetPolicy(cfg.PermissionPreset, cfg.SandboxMode)
	tools.SetApproval(cfg.ApprovalPolicy)
	// A widened retry is the one path that may reach outside the workspace, and it
	// must be a user decision. The hook closes over the runner so the ask lands on
	// the session's own log.
	var runner *Runner
	tools.askApproval = func(inv ToolInvocation, requested, effective, command string) ApprovalOutcome {
		if runner == nil || inv.Session == nil {
			// No session to record the pair on: fail closed rather than grant.
			return ApprovalUnavailable
		}
		return runner.requestApproval(inv.Session, requested, effective, inv, command)
	}
	tools.SetDSHHome(cfg.DSHHome)
	// Skills are resolved once per runner: a load must not walk the directories
	// again on every tool call, and a catalog that changes mid-session would make
	// the model's earlier choices look wrong.
	tools.SetSkills(cfg.Skills)
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
		cfg:             cfg,
		catalog:         catalog,
		clients:         clients,
		tools:           tools,
		cancels:         map[string]context.CancelFunc{},
		pendingNotices:  map[string][]string{},
		pendingFollowup: map[string]bool{},
		approvals:       newApprovalGate(),
		approvalWait:    5 * time.Minute,
		waking:          map[string]bool{},
	}
}

// Catalog returns the model catalog this runner routes over.
func (r *Runner) Catalog() *modelconfig.Catalog { return r.catalog }

// ModelsPath returns the patch file the catalog was loaded from, or "" when it
// came from the single-route fallback.
func (r *Runner) ModelsPath() string { return r.cfg.ModelsPath }

// ReplaceCatalog swaps the route catalog after a successful refresh. Running
// turns keep the client they already resolved.
func (r *Runner) ReplaceCatalog(catalog *modelconfig.Catalog, clients map[string]*llm.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.catalog = catalog
	r.cfg.Catalog = catalog
	r.clients = clients
}

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
			r.wakeIdle(req.Session)
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
	r.tools.closeJobs()
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
		{session.EventPermissionPreset, map[string]any{"preset": session.DefaultPermission().Preset}},
		{session.EventSandboxMode, map[string]any{"mode": session.DefaultPermission().Sandbox}},
		{session.EventApprovalPolicy, map[string]any{"policy": session.DefaultPermission().Approval}},
	}
	for _, step := range steps {
		if seen[step.typ] {
			continue
		}
		if _, err := sess.Append(step.typ, step.data); err != nil {
			return err
		}
	}
	// The catalog is injected once, before the first turn: a model that cannot see
	// a skill's name cannot ask for it, and the `skill` tool would be reachable
	// only by guessing.
	if !seen[skillCatalogEventType] {
		if catalog := renderSkillCatalog(r.skillsFor(sess)); catalog != "" {
			if _, err := sess.AppendSurface(session.EventUserMessage, map[string]any{
				"role":    "user",
				"id":      session.NewID(),
				"content": []any{map[string]any{"type": "text", "text": catalog}},
				"source": map[string]any{
					"kind": "skill-catalog",
					"form": "catalog",
					// The entries are recorded beside the prose, so a consumer that
					// lists skills does not have to parse the model-facing block.
					"entries": skillCatalogEntries(r.skillsFor(sess)),
				},
			}); err != nil {
				return err
			}
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
	source := req.Source
	if source == nil {
		source = map[string]any{
			"kind":           "user",
			"rpcId":          req.RPCID,
			"clientTimeZone": req.ClientTimeZone,
		}
	}
	message, err := sess.AppendSurface(session.EventUserMessage, map[string]any{
		"content": content,
		"source":  source,
		"role":    "user",
		"id":      session.NewID(),
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

// notifyCompletion queues a background-job completion notice. An idle session
// is woken with a new turn; a busy session receives the notice before its next
// model step, which keeps the running turn's tool sequence intact.
func (r *Runner) notifyCompletion(sess *session.Session, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	r.mu.Lock()
	r.pendingNotices[sess.ID()] = append(r.pendingNotices[sess.ID()], text)
	r.mu.Unlock()
	r.wakeIdle(sess)
}

func (r *Runner) takeNotices(sessionID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	notices := r.pendingNotices[sessionID]
	if len(notices) == 0 {
		return nil
	}
	delete(r.pendingNotices, sessionID)
	return notices
}

// wakeIdle starts a notice turn when no turn is running. The waking marker
// prevents two settling jobs from opening duplicate notice turns.
func (r *Runner) wakeIdle(sess *session.Session) {
	id := sess.ID()
	r.mu.Lock()
	if _, running := r.cancels[id]; running || r.waking[id] {
		r.mu.Unlock()
		return
	}
	r.waking[id] = true
	r.mu.Unlock()

	go func() {
		// A committed follow-up runs before any notice: the person's conversation
		// should not be interrupted by a job report that arrived a moment earlier.
		if r.takeFollowup(id) {
			if !r.Start(context.Background(), PromptRequest{
				Session:      sess,
				Content:      []ContentPart{{Type: "text", Text: session.ThinkingCapAutoContinueText}},
				Source:       map[string]any{"kind": "thinking-cap-auto-continue"},
				Continuation: true,
			}) {
				// Lost the race to another turn; the log still holds the message,
				// so the next prompt reads it as history rather than as a new turn.
				r.mu.Lock()
				delete(r.waking, id)
				r.mu.Unlock()
				return
			}
			r.mu.Lock()
			delete(r.waking, id)
			r.mu.Unlock()
			return
		}
		notices := r.takeNotices(id)
		r.mu.Lock()
		delete(r.waking, id)
		r.mu.Unlock()
		if len(notices) == 0 {
			return
		}
		text := strings.Join(notices, "\n\n")
		if !r.Start(context.Background(), PromptRequest{
			Session: sess,
			Content: []ContentPart{{Type: "text", Text: text}},
			Source: map[string]any{
				"kind":    "tool-jobs",
				"form":    "notice",
				"summary": text,
			},
		}) {
			r.mu.Lock()
			r.pendingNotices[id] = append(notices, r.pendingNotices[id]...)
			r.mu.Unlock()
		}
	}()
}

// appendPendingNotices writes queued completion notices into the transcript
// between steps of an already-running turn.
func (r *Runner) appendPendingNotices(sess *session.Session) {
	notices := r.takeNotices(sess.ID())
	if len(notices) == 0 {
		return
	}
	text := strings.Join(notices, "\n\n")
	if _, err := sess.AppendSurface(session.EventUserMessage, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"source":  map[string]any{"kind": "tool-jobs", "form": "notice", "summary": text},
		"role":    "user",
		"id":      session.NewID(),
	}); err != nil {
		r.logf("session %s: append job notice: %v", sess.ID(), err)
	}
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

// queueFollowup marks one session whose next turn is already in the log.
func (r *Runner) queueFollowup(sessionID string) {
	r.mu.Lock()
	r.pendingFollowup[sessionID] = true
	r.mu.Unlock()
}

// takeFollowup consumes and reports the pending follow-up flag.
func (r *Runner) takeFollowup(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pendingFollowup[sessionID] {
		return false
	}
	delete(r.pendingFollowup, sessionID)
	return true
}
