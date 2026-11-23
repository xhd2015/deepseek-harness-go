package agent

import (
	"context"
	"encoding/json"
	"strings"
)

// The goal tools are how a session keeps one objective across turns.
//
// get_goal reads it, create_goal opens one, update_goal moves it. They are three
// tools rather than one because each answers a different question, and the model
// must be able to ask "what am I working on" without proposing a change.

// Goal tool names, spelled as DSH spells them.
const (
	getGoalToolName    = "get_goal"
	createGoalToolName = "create_goal"
	updateGoalToolName = "update_goal"
)

const getGoalDescription = "Read the current session goal, including the id and revision that update_goal requires."

const createGoalDescription = "Create a persisted goal that keeps this session working across automatic continuation rounds. " +
	`Use it when the direct human request is a long-running objective, even if the user did not say "goal"; not for single-turn work.`

const updateGoalDescription = "Update the current goal."

// getGoal answers with the session's goal, or null.
func (e *ToolExecutor) getGoal(ctx context.Context, inv ToolInvocation) ToolOutcome {
	_ = ctx
	sessionID := inv.SessionID
	if sessionID == "" && inv.Session != nil {
		sessionID = inv.Session.ID()
	}
	return goalOutcome(goalValueOf(CurrentGoal(sessionID)), nil)
}

// createGoal opens a goal for this session.
func (e *ToolExecutor) createGoal(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	_ = ctx
	objective, _ := args["objective"].(string)
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return ToolOutcome{Text: "objective is required", IsError: true}
	}
	maxRounds := int64(0)
	if raw, ok := args["max_goal_rounds"]; ok && raw != nil {
		value, isInt := toInt(raw)
		if !isInt || value <= 0 {
			return ToolOutcome{Text: "max_goal_rounds must be a positive integer", IsError: true}
		}
		maxRounds = value
	}
	sessionID := sessionIDOf(inv)
	goal, err := createGoal(sessionID, objective, maxRounds)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	if appendErr := recordGoalEvent(inv.Session, goal, "create"); appendErr != nil {
		return ToolOutcome{Text: "record goal: " + appendErr.Error(), IsError: true}
	}
	return goalOutcome(goalValueOf(goal), nil)
}

// updateGoal moves the session's goal.
func (e *ToolExecutor) updateGoal(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	_ = ctx
	id, _ := args["goal_id"].(string)
	action, _ := args["action"].(string)
	id = strings.TrimSpace(id)
	action = strings.TrimSpace(action)
	if id == "" {
		return ToolOutcome{Text: "goal_id is required", IsError: true}
	}
	if action == "" {
		return ToolOutcome{Text: "action is required", IsError: true}
	}
	revision, isInt := toInt(args["revision"])
	if !isInt {
		return ToolOutcome{Text: "revision is required and must be an integer", IsError: true}
	}
	objective, _ := args["objective"].(string)
	blockedReason, _ := args["blocked_reason"].(string)
	maxRounds := int64(0)
	if raw, ok := args["max_goal_rounds"]; ok && raw != nil {
		value, intOK := toInt(raw)
		if !intOK || value <= 0 {
			return ToolOutcome{Text: "max_goal_rounds must be a positive integer", IsError: true}
		}
		maxRounds = value
	}
	goal, err := updateGoal(sessionIDOf(inv), id, revision, action, strings.TrimSpace(objective), maxRounds, strings.TrimSpace(blockedReason))
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	if appendErr := recordGoalEvent(inv.Session, goal, action); appendErr != nil {
		return ToolOutcome{Text: "record goal: " + appendErr.Error(), IsError: true}
	}
	return goalOutcome(goalValueOf(goal), nil)
}

// goalOutcome renders one goal value as the tool result.
func goalOutcome(value GoalValue, _ error) ToolOutcome {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ToolOutcome{Text: "encode goal: " + err.Error(), IsError: true}
	}
	return ToolOutcome{Text: string(encoded), Meta: map[string]any{"goal": value.Goal, "activation": value.Activation}}
}

// sessionIDOf reports the session a tool invocation belongs to.
func sessionIDOf(inv ToolInvocation) string {
	if inv.SessionID != "" {
		return inv.SessionID
	}
	if inv.Session != nil {
		return inv.Session.ID()
	}
	return ""
}
