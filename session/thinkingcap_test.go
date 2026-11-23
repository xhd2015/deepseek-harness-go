package session

import "testing"

// capStop builds an assistant message that is reasoning-only.
func capStop(t *testing.T, opened *Session, reasoning string) {
	t.Helper()
	if _, err := opened.AppendSurface(EventAssistantMessage, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"id": NewID(), "role": "assistant",
			"content": []any{map[string]any{"type": "reasoning", "text": reasoning}},
		},
	}); err != nil {
		t.Fatalf("assistant: %v", err)
	}
}

// endTurnOnCap closes a turn as an output-cap stop.
func endTurnOnCap(t *testing.T, opened *Session, turn int) {
	t.Helper()
	if _, err := opened.Append(EventTurnEnd, map[string]any{
		"turn": turn, "reason": map[string]any{"kind": TurnMaxTokens},
	}); err != nil {
		t.Fatalf("turn end: %v", err)
	}
}

// appendContinue writes the follow-up the policy emits.
func appendContinue(t *testing.T, opened *Session) {
	t.Helper()
	if _, err := opened.AppendSurface(EventUserMessage, map[string]any{
		"role": "user", "id": NewID(),
		"content": []any{map[string]any{"type": "text", "text": ThinkingCapAutoContinueText}},
		"source":  map[string]any{"kind": "thinking-cap-auto-continue"},
	}); err != nil {
		t.Fatalf("continue: %v", err)
	}
}

// TestThinkingCapFoldTracksPolicyAndStreak is the state the behaviour reads.
func TestThinkingCapFoldTracksPolicyAndStreak(t *testing.T) {
	opened := newProjectionSession(t)
	if FoldThinkingCapAutoContinue(opened.Events()).Enabled {
		t.Fatal("a fresh session reports the policy on")
	}
	if _, err := opened.AppendIgnorable(ThinkingCapAutoContinueEvent, map[string]any{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	state := FoldThinkingCapAutoContinue(opened.Events())
	if !state.Enabled {
		t.Fatal("the policy did not fold")
	}
	if state.ConsecutiveAutoContinues != 0 || state.PendingMaxTokens {
		t.Fatalf("state = %+v", state)
	}

	capStop(t, opened, "thinking hard")
	endTurnOnCap(t, opened, 1)
	state = FoldThinkingCapAutoContinue(opened.Events())
	if !state.LastAssistantIsThinkingCapStop {
		t.Fatal("a reasoning-only message was not recognized")
	}
	if !state.PendingMaxTokens {
		t.Fatal("the cap stop is not pending")
	}
	if !ShouldFollowupThinkingCap(state) {
		t.Fatal("the first cap stop should be followed up")
	}

	appendContinue(t, opened)
	state = FoldThinkingCapAutoContinue(opened.Events())
	if state.ConsecutiveAutoContinues != 1 || state.PendingMaxTokens {
		t.Fatalf("state after one continue = %+v", state)
	}
	// A second cap stop is still followed up: the limit is two.
	capStop(t, opened, "still thinking")
	endTurnOnCap(t, opened, 2)
	if !ShouldFollowupThinkingCap(FoldThinkingCapAutoContinue(opened.Events())) {
		t.Fatal("the second cap stop should be followed up")
	}
	appendContinue(t, opened)
	// The third is not: continuing forever would spend the user's budget on a loop.
	capStop(t, opened, "thinking again")
	endTurnOnCap(t, opened, 3)
	state = FoldThinkingCapAutoContinue(opened.Events())
	if state.ConsecutiveAutoContinues != 2 {
		t.Fatalf("streak = %d, want 2", state.ConsecutiveAutoContinues)
	}
	if ShouldFollowupThinkingCap(state) {
		t.Fatal("the third consecutive cap stop was followed up")
	}
}

// TestThinkingCapNeedsReasoningOnly keeps a visible answer from triggering it.
func TestThinkingCapNeedsReasoningOnly(t *testing.T) {
	opened := newProjectionSession(t)
	if _, err := opened.AppendIgnorable(ThinkingCapAutoContinueEvent, map[string]any{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.AppendSurface(EventAssistantMessage, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{
			"id": NewID(), "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "here is the answer"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	endTurnOnCap(t, opened, 1)
	if ShouldFollowupThinkingCap(FoldThinkingCapAutoContinue(opened.Events())) {
		t.Fatal("a message with visible text was treated as a thinking-cap stop")
	}
}

// TestThinkingCapOffDoesNothing is the default: no policy, no extra turns.
func TestThinkingCapOffDoesNothing(t *testing.T) {
	opened := newProjectionSession(t)
	capStop(t, opened, "thinking")
	endTurnOnCap(t, opened, 1)
	if ShouldFollowupThinkingCap(FoldThinkingCapAutoContinue(opened.Events())) {
		t.Fatal("the policy is off and a follow-up was still queued")
	}
}

// TestThinkingCapStreakResetsOnANewQuestion keeps an unrelated prompt from
// inheriting an old streak.
func TestThinkingCapStreakResetsOnANewQuestion(t *testing.T) {
	opened := newProjectionSession(t)
	if _, err := opened.AppendIgnorable(ThinkingCapAutoContinueEvent, map[string]any{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	capStop(t, opened, "thinking")
	endTurnOnCap(t, opened, 1)
	appendContinue(t, opened)
	if _, err := opened.AppendSurface(EventUserMessage, map[string]any{
		"role": "user", "id": NewID(),
		"content": []any{map[string]any{"type": "text", "text": "never mind, do this instead"}},
		"source":  map[string]any{"kind": "user"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := FoldThinkingCapAutoContinue(opened.Events()).ConsecutiveAutoContinues; got != 0 {
		t.Fatalf("streak = %d after a new question, want 0", got)
	}
}
