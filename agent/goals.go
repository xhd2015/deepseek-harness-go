package agent

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// A goal is the objective a session keeps working toward across turns.
//
// It exists because a long request is not one turn: the model finishes a step,
// the turn ends, and something must remember what is still owed. The goal is that
// memory, and it is recorded on the session so a reload shows why the session is
// still active.

// GoalPhase is where a goal stands.
type GoalPhase string

// The four phases a goal can be in. `blocked` is not a failure: it says the same
// obstacle persisted, which is different from the session having finished.
const (
	GoalActive   GoalPhase = "active"
	GoalPaused   GoalPhase = "paused"
	GoalBlocked  GoalPhase = "blocked"
	GoalComplete GoalPhase = "complete"
)

// GoalBlockedReason records why a goal stopped making progress.
type GoalBlockedReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Goal is one session's objective and its progress.
type Goal struct {
	ID            string             `json:"id"`
	Revision      int64              `json:"revision"`
	Objective     string             `json:"objective"`
	Phase         GoalPhase          `json:"phase"`
	RoundsStarted int64              `json:"roundsStarted"`
	MaxGoalRounds int64              `json:"maxGoalRounds"`
	BlockedReason *GoalBlockedReason `json:"blockedReason,omitempty"`
	CreatedAt     time.Time          `json:"createdAt"`
	UpdatedAt     time.Time          `json:"updatedAt"`
}

// GoalValue is the tool result: either no goal, or the goal and whether the
// automatic continuation is armed.
type GoalValue struct {
	Goal       *Goal  `json:"goal"`
	Activation string `json:"activation,omitempty"`
}

// defaultMaxGoalRounds bounds automatic continuation when the caller names none.
const defaultMaxGoalRounds int64 = 8

// minBlockedRounds is how many rounds must persist before blocked is allowed.
//
// The threshold is what keeps "this is hard" from being reported as "this cannot
// be done": difficulty is not blocked, a condition that survived several attempts
// is.
const minBlockedRounds int64 = 3

// goalStore holds one goal per session.
//
// It is process-scoped like the question registry, and for the same reason: the
// tool call and the API that reads a session's goal have no other shared handle.
// The durable record is the session log; this map is what keeps the fold from
// being recomputed on every tool call.
var goalStore = struct {
	mu    sync.Mutex
	goals map[string]*Goal
}{goals: map[string]*Goal{}}

// goalKey is the session a goal belongs to.
func goalKey(sessionID string) string { return sessionID }

// CurrentGoal reports the session's goal, or nil.
func CurrentGoal(sessionID string) *Goal {
	goalStore.mu.Lock()
	defer goalStore.mu.Unlock()
	goal := goalStore.goals[goalKey(sessionID)]
	if goal == nil {
		return nil
	}
	clone := *goal
	return &clone
}

// createGoal records a new objective for the session.
func createGoal(sessionID, objective string, maxRounds int64) (*Goal, error) {
	if maxRounds <= 0 {
		maxRounds = defaultMaxGoalRounds
	}
	goalStore.mu.Lock()
	defer goalStore.mu.Unlock()
	if existing := goalStore.goals[goalKey(sessionID)]; existing != nil && existing.Phase == GoalActive {
		// A second active goal would leave two objectives competing for the same
		// continuation, and neither would be the one the model is following.
		return nil, errString("this session already has an active goal")
	}
	now := time.Now().UTC()
	goal := &Goal{
		ID:            newGoalID(now),
		Revision:      1,
		Objective:     objective,
		Phase:         GoalActive,
		MaxGoalRounds: maxRounds,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	goalStore.goals[goalKey(sessionID)] = goal
	clone := *goal
	return &clone, nil
}

// updateGoal applies one action to the session's goal.
func updateGoal(sessionID string, id string, revision int64, action string, objective string, maxRounds int64, blockedReason string) (*Goal, error) {
	goalStore.mu.Lock()
	defer goalStore.mu.Unlock()
	goal := goalStore.goals[goalKey(sessionID)]
	if goal == nil {
		return nil, errString("this session has no goal")
	}
	if goal.ID != id {
		return nil, errString("goal_id does not match the current goal; read it again")
	}
	if goal.Revision != revision {
		// A stale revision means the caller acted on a goal that has moved. Applying
		// it anyway would overwrite a decision made in between.
		return nil, errString("revision is stale; read the goal again")
	}
	switch action {
	case "edit":
		if objective != "" {
			goal.Objective = objective
		}
		if maxRounds > 0 {
			goal.MaxGoalRounds = maxRounds
		}
	case "pause":
		if goal.Phase != GoalActive {
			return nil, errString("only an active goal can be paused")
		}
		goal.Phase = GoalPaused
	case "resume":
		if goal.Phase != GoalPaused {
			return nil, errString("only a paused goal can be resumed")
		}
		goal.Phase = GoalActive
	case "complete":
		goal.Phase = GoalComplete
	case "blocked":
		if blockedReason == "" {
			return nil, errString("blocked_reason is required with action blocked")
		}
		if goal.RoundsStarted < minBlockedRounds {
			return nil, errString("blocked requires at least " + itoa(minBlockedRounds) +
				" consecutive goal rounds; current round is " + itoa(goal.RoundsStarted))
		}
		goal.Phase = GoalBlocked
		goal.BlockedReason = &GoalBlockedReason{Code: "model-reported", Message: blockedReason}
	default:
		return nil, errString("unknown goal action " + action)
	}
	goal.Revision++
	goal.UpdatedAt = time.Now().UTC()
	clone := *goal
	return &clone, nil
}

// recordGoalEvent appends the durable change to the session.
func recordGoalEvent(opened *session.Session, goal *Goal, action string) error {
	if opened == nil {
		return nil
	}
	_, err := opened.AppendSurface("goal/change", map[string]any{
		"action": action,
		"goal":   goal,
	})
	return err
}

// goalValueOf renders a goal the way the tool output declares it.
func goalValueOf(goal *Goal) GoalValue {
	if goal == nil {
		return GoalValue{Goal: nil}
	}
	activation := "armed"
	if goal.Phase == GoalPaused || goal.Phase == GoalComplete {
		activation = "disarmed"
	}
	return GoalValue{Goal: goal, Activation: activation}
}

// newGoalID mints a goal identity that carries its creation time, so an id can be
// read back as "when" without another field.
func newGoalID(now time.Time) string {
	return "goal-" + now.Format("20060102T150405") + "-" + session.NewID()
}

// itoa renders an integer for a message.
func itoa(value int64) string {
	return json.Number(jsonInt(value)).String()
}

// jsonInt formats an integer as JSON does.
func jsonInt(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// errString is a constant error a caller can compare, for the failures a tool
// reports verbatim.
type errString string

func (e errString) Error() string { return string(e) }

// toInt reads an integer argument the way a model emits one.
func toInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		converted := int64(typed)
		return converted, typed == float64(converted)
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	}
	return 0, false
}
