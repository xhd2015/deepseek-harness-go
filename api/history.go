package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// Error codes the SDK-facing endpoints report.
const (
	CodeModelConflict    = "session/model-conflict"
	CodeAlreadyOwned     = "session/already-owned"
	CodeForkUnavailable  = "session/fork-unavailable"
	CodeEditRejected     = "session/edit-rejected"
	CodeInjectRejected   = "session/inject-rejected"
	CodeCatalogUnchanged = "session/catalog-unchanged"
)

// maxReceipts bounds the idempotency memory. A retry arrives within seconds of
// its first attempt, so a bounded ring is enough and cannot grow without limit.
const maxReceipts = 512

// requestReceipts dedupes client-minted request ids so a transport retry never
// commits the same batch twice.
type requestReceipts struct {
	mu    sync.Mutex
	seen  map[string]any
	order []string
}

func (r *requestReceipts) get(key string) (any, bool) {
	if key == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.seen[key]
	return value, ok
}

func (r *requestReceipts) put(key string, value any) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]any{}
	}
	if _, exists := r.seen[key]; !exists {
		r.order = append(r.order, key)
	}
	r.seen[key] = value
	for len(r.order) > maxReceipts {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.seen, oldest)
	}
}

// status answers session/status: which of the named sessions run a turn. An id
// this backend never loaded is absent, which means "unknown", not "idle".
func (s *Service) status(args json.RawMessage) (any, error) {
	var request struct {
		IDs []string `json:"ids"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	running := map[string]bool{}
	ids := request.IDs
	if ids == nil {
		entries, err := s.cfg.Store.List()
		if err != nil {
			return nil, fail(CodeInternal, err.Error())
		}
		for _, entry := range entries {
			ids = append(ids, entry.Header.ID)
		}
	}
	for _, id := range ids {
		id = session.NormalizeSessionID(id)
		if id == "" {
			continue
		}
		// An id this backend does not hold is unknown, not idle: absence from
		// the answer is the only honest report.
		if !s.cfg.Store.Exists(id) {
			continue
		}
		if s.cfg.Runner == nil {
			running[id] = false
			continue
		}
		running[id] = s.cfg.Runner.Busy(id)
	}
	return map[string]any{"running": running}, nil
}

// refreshModelCatalog answers session/refreshModelCatalog. dsh-go resolves its
// routes from a patch file at startup, so a refresh re-reads that file.
func (s *Service) refreshModelCatalog(json.RawMessage) (any, error) {
	if s.cfg.Runner != nil && s.cfg.ReloadModels != nil {
		if path := s.cfg.Runner.ModelsPath(); path != "" {
			catalog, clients, err := s.cfg.ReloadModels(path)
			if err != nil {
				return nil, &mux.Failure{
					Code:    CodeCatalogUnchanged,
					Message: fmt.Sprintf("cannot reload %s: %v", path, err),
					Details: map[string]any{"path": path},
				}
			}
			if catalog != nil {
				s.cfg.Runner.ReplaceCatalog(catalog, clients)
			}
		}
	}
	return s.modelCatalog(nil)
}

// selectDefaultModel answers session/selectDefaultModel: it stores the model a
// new session starts on without touching any existing session.
func (s *Service) selectDefaultModel(args json.RawMessage) (any, error) {
	var request struct {
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	route, err := s.catalog().Resolve(modelconfig.Selection{
		Provider:        request.Provider,
		Model:           request.Model,
		ReasoningEffort: request.ReasoningEffort,
	})
	if err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if s.cfg.SaveDefaultModel != nil {
		if err := s.cfg.SaveDefaultModel(route.Selection); err != nil {
			return nil, fail(CodeInternal, err.Error())
		}
	}
	if s.cfg.SetDefaultModel != nil {
		s.cfg.SetDefaultModel(route.Selection)
	}
	return map[string]any{"selected": sessionSelection(route.Selection)}, nil
}

// injectMessage is one entry of an injected batch.
type injectMessage struct {
	Kind      string          `json:"kind"`
	Text      string          `json:"text"`
	Tool      string          `json:"tool"`
	Arguments string          `json:"arguments"`
	Call      int64           `json:"call"`
	IsError   bool            `json:"isError"`
	raw       json.RawMessage `json:"-"`
}

// inject answers session/inject: one ordered batch commits atomically, then the
// agent acts on it without a second call.
func (s *Service) inject(args json.RawMessage) (any, error) {
	var request struct {
		RequestID string          `json:"requestId"`
		SessionID string          `json:"sessionId"`
		Messages  []injectMessage `json:"messages"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if receipt, ok := s.receipts.get("inject\x00" + request.RequestID); ok {
		return receipt, nil
	}
	if len(request.Messages) == 0 {
		return nil, fail(CodeBadRequest, "an injected batch needs at least one message")
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	events, err := s.injectEvents(opened, request.Messages)
	if err != nil {
		return nil, err
	}
	committed, err := opened.AppendEvents(events)
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	seqs := make([]int64, len(committed))
	for index, event := range committed {
		seqs[index] = event.Seq
	}
	started := s.actOnHistory(opened, request.RequestID)
	result := map[string]any{
		"seqs":     seqs,
		"callSeqs": seqsOfType(committed, session.EventToolCall),
		"started":  started,
	}
	if results := seqsOfType(committed, session.EventToolResult); len(results) > 0 {
		result["resultSeqs"] = results
	}
	s.receipts.put("inject\x00"+request.RequestID, result)
	return result, nil
}

// seqsOfType returns the committed seqs of one event type, in batch order.
func seqsOfType(committed []session.Event, typ string) []int64 {
	out := make([]int64, 0, len(committed))
	for _, event := range committed {
		if event.Type == typ {
			out = append(out, event.Seq)
		}
	}
	return out
}

// actOnHistory makes the agent continue after a history write: it stays in the
// running turn when one exists, and otherwise opens a new one.
func (s *Service) actOnHistory(opened *session.Session, requestID string) bool {
	if s.cfg.Runner == nil {
		return false
	}
	if s.cfg.Runner.Busy(opened.ID()) {
		return true
	}
	return s.cfg.Runner.Start(context.Background(), agent.PromptRequest{
		Session: opened,
		Content: []agent.ContentPart{{Type: "text", Text: ""}},
		RPCID:   requestID,
		Source: map[string]any{
			"kind":  "history-edit",
			"rpcId": requestID,
		},
		Continuation: true,
	})
}

// injectEvents builds the durable rows for one injected batch, validating tool
// call/result pairing before anything is written.
func (s *Service) injectEvents(opened *session.Session, messages []injectMessage) ([]session.Event, error) {
	turn := nextTurn(opened.Events())
	var events []session.Event
	// A tool result cites its call by seq. DSH numbers the batch entries from the
	// current head, so a call inside this batch is addressed by the seq it is about
	// to receive — `head + index` — and a call from an earlier turn by its
	// committed seq. Both spellings must resolve, because agent-pro sends either.
	batchCalls := map[int64]string{}
	batchCallSeqs := map[int64]int64{}
	head := opened.LastSeq() + 1
	knownCalls := existingCallIDs(opened.Events())
	for index, message := range messages {
		switch message.Kind {
		case "user":
			if strings.TrimSpace(message.Text) == "" {
				return nil, fail(CodeBadRequest, "injected user message has no text")
			}
			events = append(events, session.Event{
				Type: session.EventUserMessage,
				Data: mustRaw(map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "text", "text": message.Text}},
					"id":      session.NewID(),
					"source":  map[string]any{"kind": "user", "rpcId": fmt.Sprintf("inject-%d", index)},
				}),
				SurfaceOp: session.SplitSurface,
			})
		case "thinking", "assistant":
			if strings.TrimSpace(message.Text) == "" {
				return nil, fail(CodeBadRequest, "injected assistant message has no text")
			}
			blockType := "text"
			if message.Kind == "thinking" {
				blockType = "reasoning"
			}
			events = append(events, session.Event{
				Type: session.EventAssistantMessage,
				Data: mustRaw(map[string]any{
					"turn": nextTurnOf(opened.Events()),
					"step": 1,
					"message": map[string]any{
						"role":    "assistant",
						"id":      session.NewID(),
						"content": []any{map[string]any{"type": blockType, "text": message.Text}},
						"source":  map[string]any{"kind": "model"},
					},
				}),
				SurfaceOp: session.SplitSurface,
			})
		case "tool-call":
			if strings.TrimSpace(message.Tool) == "" || strings.TrimSpace(message.Arguments) == "" {
				return nil, fail(CodeBadRequest, "injected tool-call needs a tool name and arguments")
			}
			// A call is addressed by the seq it will occupy, so a result later in this
			// same batch can name it before it exists.
			callID := session.NewID()
			callSeq := head + int64(len(events))
			batchCalls[callSeq] = callID
			events = append(events, session.Event{
				Type: session.EventToolCall,
				Data: mustRaw(map[string]any{
					"turn":      turn,
					"step":      1,
					"callId":    callID,
					"name":      message.Tool,
					"arguments": message.Arguments,
				}),
				Ignorable: true,
			})
			batchCallSeqs[callSeq] = callSeq
		case "tool-result":
			if strings.TrimSpace(message.Text) == "" {
				return nil, fail(CodeBadRequest, "injected tool-result has no text")
			}
			callID := batchCalls[message.Call]
			sourceSeq := batchCallSeqs[message.Call]
			if callID == "" {
				// A bare position inside the batch is a convenience spelling; accept it
				// only when it cannot be confused with a committed seq.
				if candidate, ok := batchCalls[head+message.Call]; ok && message.Call < int64(len(messages)) {
					callID = candidate
					sourceSeq = head + message.Call
				}
			}
			if callID == "" {
				callID = knownCalls[message.Call]
				sourceSeq = message.Call
			}
			if callID == "" {
				return nil, fail(CodeBadRequest,
					fmt.Sprintf("injected tool-result cites call %d, which is not in this batch or the session", message.Call))
			}
			data := map[string]any{
				"turn": turn,
				"step": 1,
				"message": map[string]any{
					"role":       "tool",
					"id":         session.NewID(),
					"toolCallId": callID,
					"content":    []any{map[string]any{"type": "text", "text": message.Text}},
					"source":     map[string]any{"kind": "tool", "callId": callID},
				},
			}
			if message.IsError {
				data["message"].(map[string]any)["isError"] = true
				data["error"] = map[string]any{"name": "ToolError", "code": "TOOL_ERROR", "reason": message.Text}
			}
			events = append(events, session.Event{
				Type:            session.EventToolResult,
				Data:            mustRaw(data),
				SurfaceOp:       session.SplitSurface,
				SourceEventSeqs: []int64{sourceSeq},
			})
		default:
			return nil, fail(CodeBadRequest,
				fmt.Sprintf("injected message kind must be user, thinking, assistant, tool-call or tool-result; got %q", message.Kind))
		}
	}
	return events, nil
}

// existingCallIDs maps a committed tool/call seq to its call id.
func existingCallIDs(events []session.Event) map[int64]string {
	out := map[int64]string{}
	for _, event := range events {
		if event.Type != session.EventToolCall {
			continue
		}
		var data struct {
			CallID string `json:"callId"`
		}
		if err := json.Unmarshal(event.Data, &data); err == nil && data.CallID != "" {
			out[event.Seq] = data.CallID
		}
	}
	return out
}

// editChange is one rewrite or removal of an existing message.
type editChange struct {
	Seq     int64  `json:"seq"`
	Text    string `json:"text"`
	Deleted bool   `json:"deleted"`
}

// edit answers session/edit: one message/edit record carries the whole batch, so
// a rejected batch leaves no trace and a committed one replays identically.
func (s *Service) edit(args json.RawMessage) (any, error) {
	var request struct {
		RequestID string       `json:"requestId"`
		SessionID string       `json:"sessionId"`
		Edits     []editChange `json:"edits"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	if receipt, ok := s.receipts.get("edit\x00" + request.RequestID); ok {
		return receipt, nil
	}
	if len(request.Edits) == 0 {
		return nil, fail(CodeBadRequest, "an edit batch needs at least one change")
	}
	opened, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	events := opened.Events()
	messageSeqs := map[int64]string{}
	for _, event := range events {
		if event.SurfaceOp != nil && session.SurfaceEventTypes[event.Type] {
			messageSeqs[event.Seq] = event.Type
		}
	}
	seen := map[int64]bool{}
	edits := make([]map[string]any, 0, len(request.Edits))
	for _, change := range request.Edits {
		if change.Seq < 0 {
			return nil, fail(CodeBadRequest, "edit seq must not be negative")
		}
		if seen[change.Seq] {
			return nil, fail(CodeBadRequest, fmt.Sprintf("edit names seq %d twice", change.Seq))
		}
		seen[change.Seq] = true
		if change.Deleted {
			edits = append(edits, map[string]any{"seq": change.Seq, "deleted": true})
			continue
		}
		if strings.TrimSpace(change.Text) == "" {
			return nil, fail(CodeBadRequest, fmt.Sprintf("edit of seq %d has no text", change.Seq))
		}
		if _, ok := messageSeqs[change.Seq]; !ok {
			// A message that a later compaction shadowed is skipped rather than
			// reported: a former legitimate edit must not become an error.
			continue
		}
		edits = append(edits, map[string]any{"seq": change.Seq, "text": change.Text})
	}
	if len(edits) == 0 {
		result := map[string]any{"seq": opened.LastSeq(), "started": false}
		s.receipts.put("edit\x00"+request.RequestID, result)
		return result, nil
	}
	event, err := opened.Append(session.EventMessageEdit, map[string]any{"edits": edits})
	if err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	started := s.actOnHistory(opened, request.RequestID)
	result := map[string]any{"seq": event.Seq, "started": started}
	s.receipts.put("edit\x00"+request.RequestID, result)
	return result, nil
}

// fork answers session/fork: the child inherits an exact source prefix, is
// marked seeded, and closes any turn the cut left open.
func (s *Service) fork(args json.RawMessage) (any, error) {
	var request struct {
		SessionID      string `json:"sessionId"`
		AtSeq          *int64 `json:"atSeq"`
		Cwd            string `json:"cwd"`
		AllowMigration *bool  `json:"allowMigration"`
	}
	if err := decodeRequest(args, &request); err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	source, err := s.openSession(request.SessionID)
	if err != nil {
		return nil, err
	}
	events := source.Events()
	if len(events) == 0 {
		return nil, &mux.Failure{
			Code:    CodeForkUnavailable,
			Message: "the session has no committed turn to fork",
			Details: map[string]any{"sessionId": source.ID()},
		}
	}
	boundary := int64(-1)
	if request.AtSeq != nil {
		if *request.AtSeq < 0 || *request.AtSeq >= int64(len(events)) {
			return nil, fail(CodeBadRequest, "fork cut is outside the stored log")
		}
		boundary = *request.AtSeq
	} else {
		// Default: the latest completed turn.
		for index := len(events) - 1; index >= 0; index-- {
			if events[index].Type == session.EventTurnEnd {
				boundary = events[index].Seq
				break
			}
		}
		if boundary < 0 {
			return nil, &mux.Failure{
				Code:    CodeForkUnavailable,
				Message: "the session has no completed turn to fork",
				Details: map[string]any{"sessionId": source.ID()},
			}
		}
	}
	cwd := strings.TrimSpace(request.Cwd)
	if cwd == "" {
		cwd = source.Cwd()
	} else if !strings.HasPrefix(cwd, "/") {
		return nil, &mux.Failure{
			Code:    CodeForkUnavailable,
			Message: "fork cwd must be an absolute directory",
			Details: map[string]any{"cwd": cwd},
		}
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, &mux.Failure{
			Code:    CodeForkUnavailable,
			Message: fmt.Sprintf("fork cwd %q is not a directory", cwd),
			Details: map[string]any{"cwd": cwd},
		}
	}
	seed := forkedPrefix(events, boundary)
	child, err := s.cfg.Store.CreateSeeded(cwd, source.Header(), seed)
	if err != nil {
		if errors.Is(err, session.ErrAlreadyOwned) {
			return nil, fail(CodeAlreadyOwned, err.Error())
		}
		return nil, fail(CodeInternal, err.Error())
	}
	s.Workspaces.Add(cwd)
	return map[string]any{"sessionId": child.ID()}, nil
}

// forkedPrefix copies the inclusive prefix through boundary and closes it the
// way DSH builds a fork seed: an inherited marker, then forked closers.
func forkedPrefix(events []session.Event, boundary int64) []session.Event {
	prefix := make([]session.Event, 0, boundary+2)
	openCalls := map[string]int64{}
	openStep := ""
	openTurn := int64(-1)
	for index := 0; index <= int(boundary) && index < len(events); index++ {
		event := events[index]
		prefix = append(prefix, event)
		switch event.Type {
		case session.EventTurnStart:
			openTurn = event.Seq
		case session.EventTurnEnd:
			openTurn = -1
			openStep = ""
			openCalls = map[string]int64{}
		case session.EventStepStart:
			openStep = event.Type
		case session.EventStepEnd:
			openStep = ""
		case session.EventToolCall:
			var data struct {
				CallID string `json:"callId"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil && data.CallID != "" {
				openCalls[data.CallID] = event.Seq
			}
		case session.EventToolResult:
			var data struct {
				Message struct {
					ToolCallID string `json:"toolCallId"`
				} `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &data); err == nil {
				delete(openCalls, data.Message.ToolCallID)
			}
		}
	}
	// A call without its result would make the child's history non-replayable,
	// so the seed closes it with a forked error result.
	for callID, seq := range openCalls {
		prefix = append(prefix, session.Event{
			Type: session.EventToolResult,
			Data: mustRaw(map[string]any{
				"turn": 0,
				"step": 1,
				"message": map[string]any{
					"role":       "tool",
					"id":         session.NewID(),
					"toolCallId": callID,
					"content":    []any{map[string]any{"type": "text", "text": "forked before this tool returned"}},
					"source":     map[string]any{"kind": "tool", "callId": callID},
					"isError":    true,
				},
				"error": map[string]any{
					"name":   "ToolError",
					"code":   "FORKED",
					"reason": "the fork boundary cut this call before it returned",
				},
			}),
			SurfaceOp:       session.SplitSurface,
			SourceEventSeqs: []int64{seq},
		})
	}
	prefix = append(prefix, session.Event{
		Type: session.EventSessionEndSeed,
		Data: mustRaw(map[string]any{"inherited": true}),
	})
	if openStep != "" {
		prefix = append(prefix, session.Event{
			Type: session.EventStepEnd,
			Data: mustRaw(map[string]any{"turn": 0, "step": 1}),
		})
	}
	if openTurn >= 0 {
		prefix = append(prefix, session.Event{
			Type: session.EventTurnEnd,
			Data: mustRaw(map[string]any{"turn": 0, "reason": map[string]any{"kind": session.TurnForked}}),
		})
	}
	return prefix
}

// startWithModel answers the create path with an initial selection: the route is
// validated before anything exists, recorded on the session, and echoed.
func (s *Service) applyInitialSelection(opened *session.Session, selection *session.ModelSelection) (
	*session.ModelSelection, error,
) {
	if selection == nil {
		return nil, nil
	}
	route, err := s.catalog().Resolve(modelconfig.Selection{
		Provider:        selection.Provider,
		Model:           selection.Model,
		ReasoningEffort: selection.ReasoningEffort,
	})
	if err != nil {
		return nil, fail(CodeBadRequest, err.Error())
	}
	accepted := sessionSelection(route.Selection)
	for _, event := range opened.Events() {
		if event.Type != session.EventModelSelection {
			continue
		}
		var stored session.ModelSelection
		if err := json.Unmarshal(event.Data, &stored); err != nil {
			continue
		}
		if stored == accepted {
			return &accepted, nil
		}
		return nil, &mux.Failure{
			Code:    CodeModelConflict,
			Message: "the session already recorded a different model selection",
			Details: map[string]any{"sessionId": opened.ID(), "modelSelection": stored},
		}
	}
	if _, err := opened.Append(session.EventModelSelection, accepted); err != nil {
		return nil, fail(CodeInternal, err.Error())
	}
	return &accepted, nil
}

// mustRaw marshals an event payload that was built by this package.
func mustRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// nextTurn returns the next turn number for a log.
func nextTurn(events []session.Event) int {
	turn := 0
	for _, event := range events {
		if event.Type != session.EventTurnStart {
			continue
		}
		var data struct {
			Turn int `json:"turn"`
		}
		if err := json.Unmarshal(event.Data, &data); err == nil && data.Turn > turn {
			turn = data.Turn
		}
	}
	return turn + 1
}

func nextTurnOf(events []session.Event) int { return nextTurn(events) - 1 }
