package session

// Durable event types this host writes. The vocabulary is a subset of the
// Harness event list; the chat page and the SDK consumers need these.
const (
	EventPermissionPreset = "permission/preset"
	EventSandboxMode      = "sandbox/mode"
	EventApprovalPolicy   = "approval/policy"
	EventApprovalAsked    = "approval/asked"
	EventApprovalDecided  = "approval/decided"
	EventModelSelection   = "model/selection"
	EventRequestHeader    = "request/header"
	EventRequestContext   = "request/context"
	EventTurnStart        = "turn/start"
	EventTurnEnd          = "turn/end"
	EventStepStart        = "step/start"
	EventStepEnd          = "step/end"
	EventUserMessage      = "user/message"
	EventSystemMessage    = "system/message"
	EventAssistantMessage = "assistant/message"
	EventAssistantAttempt = "assistant/attempt"
	EventToolCall         = "tool/call"
	// EventWorkingDirectoryChange records a directory the model moved the session
	// into. Later calls resolve relative paths against it.
	EventWorkingDirectoryChange = "working-directory/change"
	EventToolResult             = "tool/result"
	EventMessageEdit            = "message/edit"
	EventSessionTitle           = "session/title"
	EventSessionEndSeed         = "session/end-seed"
	EventAgentInboxSpliced      = "agent/inbox/spliced"
	EventWorkingDirectory       = "working-directory/change"
	EventTodoWrite              = "todo/write"
	// EventCommandRun and EventCommandDone bracket one human slash command.
	EventCommandRun              = "command/run"
	EventCommandDone             = "command/done"
	EventJobRegistered           = "job/registered"
	EventJobSettled              = "job/settled"
	EventLlmRetry                = "llm/retry"
	EventLlmRetryStarted         = "llm/retry-started"
	EventDeliverablesPresented   = "deliverables/presented"
	EventImageOffload            = "image/offload"
	EventSourceDeliveryAccepted  = "session-log-deepseek/delivery-accepted"
	EventSessionTitleLlmRequest  = "session/title-llm-request"
	EventSessionTitleLlmResponse = "session/title-llm-request/response"
	EventWebSearchLlmRequest     = "web/deepseek-search-llm-request"
)

// KnownEventTypes is the vocabulary this build interprets. A stored row this
// build does not know is refused unless its writer marked it ignorable.
var KnownEventTypes = map[string]bool{
	EventPermissionPreset: true,
	EventSandboxMode:      true,

	EventApprovalPolicy:   true,
	EventApprovalAsked:    true,
	EventApprovalDecided:  true,
	EventModelSelection:   true,
	EventRequestHeader:    true,
	EventRequestContext:   true,
	EventTurnStart:        true,
	EventTurnEnd:          true,
	EventStepStart:        true,
	EventStepEnd:          true,
	EventUserMessage:      true,
	EventSystemMessage:    true,
	EventAssistantMessage: true,
	EventAssistantAttempt: true,
	EventToolCall:         true,

	EventToolResult:              true,
	EventMessageEdit:             true,
	EventSessionTitle:            true,
	EventSessionEndSeed:          true,
	EventAgentInboxSpliced:       true,
	EventWorkingDirectory:        true,
	EventTodoWrite:               true,
	EventJobRegistered:           true,
	EventJobSettled:              true,
	EventLlmRetry:                true,
	EventLlmRetryStarted:         true,
	EventDeliverablesPresented:   true,
	EventImageOffload:            true,
	EventSourceDeliveryAccepted:  true,
	EventSessionTitleLlmRequest:  true,
	EventSessionTitleLlmResponse: true,
	EventWebSearchLlmRequest:     true,
	// Boundary and plugin-owned records a compatible reader passes through.
	"command/run":                     true,
	"command/done":                    true,
	"compaction/start":                true,
	"compaction/summary":              true,
	"compaction/prune":                true,
	"compaction/end":                  true,
	"developer/message":               true,
	"feedback/message-put":            true,
	"feedback/message-delete":         true,
	"feedback/record":                 true,
	"goal/change":                     true,
	"hook/invoked":                    true,
	"hook/result":                     true,
	"plan/mode":                       true,
	"schedule/change":                 true,
	ThinkingCapAutoContinueEvent:      true,
	"subagent/catalog":                true,
	"subagent/descriptor":             true,
	"subagent/model-selection-policy": true,
	"team/member":                     true,
	"team/message/delivered":          true,
	"team/message/queued":             true,
	"team/mode":                       true,
	"team/task":                       true,
	"tool-workflow/agent-end":         true,
	"tool-workflow/agent-start":       true,
	"tool-workflow/run-end":           true,
	"tool-workflow/run-start":         true,
	"tool/ptc-dispatch":               true,
	"tool/ptc-dispatch-start":         true,
	"workspace/changes":               true,
}

// SurfaceEventTypes are the event types DSH treats as model-visible surface
// nodes and that may carry a surfaceOp.
var SurfaceEventTypes = map[string]bool{
	EventSystemMessage:    true,
	"developer/message":   true,
	EventUserMessage:      true,
	EventAssistantMessage: true,
	EventToolResult:       true,
}

// Turn end reasons this host emits. The full DSH set also carries blocked,
// max-tokens, interrupted and forked; they are accepted when read.
const (
	TurnCompleted = "completed"
	TurnAborted   = "aborted"
	TurnError     = "error"
	TurnForked    = "forked"
	TurnMaxTokens = "max-tokens"
)
