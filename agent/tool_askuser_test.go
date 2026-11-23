package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// timeoutSecond is one second, named so the deadline assertions read as budgets
// rather than magic numbers.
const timeoutSecond = time.Second

// TestAskUserQuestionReturnsImmediately is the corrected contract: asking is an
// ordinary tool call. It must not wait for a person, because nobody answers in
// seconds and a waiting call leaves the whole session looking busy.
func TestAskUserQuestionReturnsImmediately(t *testing.T) {
	executor := NewToolExecutor(t.TempDir())
	inv := ToolInvocation{
		SessionID: "session-fast",
		CallID:    "call-fast",
		Name:      askUserQuestionToolName,
		Arguments: `{"questions":[{"id":"q1","question":"Which mode?","options":[{"label":"embed"}]}]}`,
	}
	done := make(chan ToolOutcome, 1)
	go func() { done <- executor.ExecuteOutcome(context.Background(), inv) }()

	select {
	case outcome := <-done:
		if outcome.IsError {
			t.Fatalf("asking failed: %s", outcome.Text)
		}
		var asked AskedQuestionResult
		if err := json.Unmarshal([]byte(outcome.Text), &asked); err != nil {
			t.Fatalf("decode %q: %v", outcome.Text, err)
		}
		if !asked.Asked || asked.CallID != "call-fast" || asked.Message == "" {
			t.Fatalf("result = %+v", asked)
		}
	case <-time.After(2 * timeoutSecond):
		t.Fatal("ask_user_question waited for an answer instead of returning")
	}

	// The question is remembered so a later answer can be validated.
	if pending := PendingQuestions(); len(pending) != 1 {
		t.Fatalf("pending = %v", pending)
	}
}

// TestAnswerQuestionValidatesTheBatch keeps a malformed reply from reaching the
// model, where it would be indistinguishable from a real answer.
func TestAnswerQuestionValidatesTheBatch(t *testing.T) {
	executor := NewToolExecutor(t.TempDir())
	executor.ExecuteOutcome(context.Background(), ToolInvocation{
		SessionID: "session-batch",
		CallID:    "call-batch",
		Name:      askUserQuestionToolName,
		Arguments: `{"questions":[{"id":"a","question":"A"},{"id":"b","question":"B"}]}`,
	})

	if err := AnswerQuestion("session-batch", "call-batch", QuestionAnswerBatch{
		Answers: []QuestionAnswer{{ID: "a"}},
	}); err != ErrIncompleteAnswers {
		t.Fatalf("partial batch err = %v", err)
	}
	if err := AnswerQuestion("session-batch", "call-batch", QuestionAnswerBatch{
		Answers: []QuestionAnswer{{ID: "a"}, {ID: "a"}},
	}); err != ErrIncompleteAnswers {
		t.Fatalf("duplicate err = %v", err)
	}
	if err := AnswerQuestion("session-batch", "call-batch", QuestionAnswerBatch{
		Answers: []QuestionAnswer{{ID: "a"}, {ID: "b"}},
	}); err != nil {
		t.Fatalf("valid batch: %v", err)
	}
	// An answered question is gone, so a second submit cannot reach the model twice.
	if err := AnswerQuestion("session-batch", "call-batch", QuestionAnswerBatch{
		Answers: []QuestionAnswer{{ID: "a"}, {ID: "b"}},
	}); err != ErrNoSuchQuestion {
		t.Fatalf("second answer err = %v", err)
	}
}

// TestAskUserQuestionRejectsBadQuestions reports the model's mistake instead of
// showing the user a card that cannot be answered.
func TestAskUserQuestionRejectsBadQuestions(t *testing.T) {
	executor := NewToolExecutor(t.TempDir())
	cases := map[string]string{
		"no questions":  `{}`,
		"empty list":    `{"questions":[]}`,
		"missing id":    `{"questions":[{"question":"Q"}]}`,
		"missing text":  `{"questions":[{"id":"a"}]}`,
		"duplicate ids": `{"questions":[{"id":"a","question":"A"},{"id":"a","question":"B"}]}`,
	}
	for name, arguments := range cases {
		t.Run(name, func(t *testing.T) {
			outcome := executor.ExecuteOutcome(context.Background(), ToolInvocation{
				SessionID: "session-bad", CallID: "call-bad", Name: askUserQuestionToolName, Arguments: arguments,
			})
			if !outcome.IsError {
				t.Fatalf("accepted %s: %s", arguments, outcome.Text)
			}
		})
	}
}
