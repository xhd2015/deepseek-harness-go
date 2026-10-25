package session

// Durable event types this host writes. The vocabulary is a subset of the
// Harness event list; the chat page only needs these.
const (
	EventPermissionPreset = "permission/preset"
	EventSandboxMode      = "sandbox/mode"
	EventApprovalPolicy   = "approval/policy"
	EventModelSelection   = "model/selection"
	EventTurnStart        = "turn/start"
	EventTurnEnd          = "turn/end"
	EventStepStart        = "step/start"
	EventStepEnd          = "step/end"
	EventUserMessage      = "user/message"
	EventAssistantMessage = "assistant/message"
	EventAssistantAttempt = "assistant/attempt"
	EventToolCall         = "tool/call"
	EventToolResult       = "tool/result"
	EventSessionTitle     = "session/title"
	EventWorkingDirectory = "working-directory/change"
	EventTodoWrite        = "todo/write"
	EventJobRegistered    = "job/registered"
	EventJobSettled       = "job/settled"
)

// Turn end reasons.
const (
	TurnCompleted = "completed"
	TurnAborted   = "aborted"
	TurnError     = "error"
)
