package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// goalOf decodes a goal tool result.
func goalOf(t *testing.T, output string) GoalValue {
	t.Helper()
	var value GoalValue
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	return value
}

// TestGoalLifecycleWritesGoalChange is the acceptance criterion: the three tools
// move one goal and each move is recorded on the session.
func TestGoalLifecycleWritesGoalChange(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	opened := newOpenSession(t, root)
	inv := ToolInvocation{SessionID: opened.ID(), Session: opened}

	// No goal yet.
	output, isError := callTool(t, executor, inv, getGoalToolName, map[string]any{})
	if isError {
		t.Fatalf("get_goal: %s", output)
	}
	if value := goalOf(t, output); value.Goal != nil {
		t.Fatalf("a fresh session reports a goal: %+v", value.Goal)
	}

	output, isError = callTool(t, executor, inv, createGoalToolName, map[string]any{
		"objective":       "Ship the compaction work",
		"max_goal_rounds": 4,
	})
	if isError {
		t.Fatalf("create_goal: %s", output)
	}
	created := goalOf(t, output)
	if created.Goal == nil || created.Goal.Phase != GoalActive {
		t.Fatalf("created = %+v", created)
	}
	if created.Goal.MaxGoalRounds != 4 || created.Activation != "armed" {
		t.Fatalf("created = %+v", created)
	}

	// A second active goal would compete for the same continuation.
	if _, isError := callTool(t, executor, inv, createGoalToolName, map[string]any{"objective": "another"}); !isError {
		t.Fatal("a second active goal was accepted")
	}

	// A stale revision must not apply: it would overwrite a decision made between.
	output, isError = callTool(t, executor, inv, updateGoalToolName, map[string]any{
		"goal_id":  created.Goal.ID,
		"revision": 99,
		"action":   "complete",
	})
	if !isError {
		t.Fatalf("a stale revision was accepted: %s", output)
	}

	output, isError = callTool(t, executor, inv, updateGoalToolName, map[string]any{
		"goal_id":  created.Goal.ID,
		"revision": created.Goal.Revision,
		"action":   "complete",
	})
	if isError {
		t.Fatalf("update_goal complete: %s", output)
	}
	completed := goalOf(t, output)
	if completed.Goal == nil || completed.Goal.Phase != GoalComplete {
		t.Fatalf("completed = %+v", completed)
	}
	if completed.Goal.Revision != created.Goal.Revision+1 {
		t.Fatalf("revision = %d, want %d", completed.Goal.Revision, created.Goal.Revision+1)
	}
	if completed.Activation != "disarmed" {
		t.Fatalf("activation = %q, want disarmed", completed.Activation)
	}

	// Every move is durable, which is what a reload reads.
	var actions []string
	for _, event := range opened.Events() {
		if event.Type != session.EventGoalChange {
			continue
		}
		var payload struct {
			Action string `json:"action"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			actions = append(actions, payload.Action)
		}
	}
	if strings.Join(actions, ",") != "create,complete" {
		t.Fatalf("recorded actions = %v", actions)
	}
}

// TestGoalBlockedNeedsRoundsAndReason pins the two guards on reporting blocked.
func TestGoalBlockedNeedsRoundsAndReason(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	opened := newOpenSession(t, root)
	inv := ToolInvocation{SessionID: opened.ID(), Session: opened}

	output, isError := callTool(t, executor, inv, createGoalToolName, map[string]any{"objective": "hard thing"})
	if isError {
		t.Fatalf("create: %s", output)
	}
	goal := goalOf(t, output).Goal
	if goal == nil {
		t.Fatal("no goal")
	}

	// No reason.
	if _, isError := callTool(t, executor, inv, updateGoalToolName, map[string]any{
		"goal_id": goal.ID, "revision": goal.Revision, "action": "blocked",
	}); !isError {
		t.Fatal("blocked without a reason was accepted")
	}
	// Too early: difficulty is not a block.
	output, isError = callTool(t, executor, inv, updateGoalToolName, map[string]any{
		"goal_id": goal.ID, "revision": goal.Revision, "action": "blocked",
		"blocked_reason": "still stuck",
	})
	if !isError {
		t.Fatalf("blocked before the round threshold was accepted: %s", output)
	}
	if !strings.Contains(output, "rounds") {
		t.Fatalf("the error does not explain the threshold: %s", output)
	}
}
