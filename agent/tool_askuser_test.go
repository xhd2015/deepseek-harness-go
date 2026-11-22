package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAskUserQuestionBlocksThenReturnsAnswers(t *testing.T) {
	e := NewToolExecutor(t.TempDir())
	inv := ToolInvocation{SessionID: "session-test", CallID: "call-1", Name: askUserQuestionToolName,
		Arguments: `{"questions":[{"id":"q1","question":"Which mode?","header":"Choose","options":[{"label":"embed"},{"label":"standalone","description":"separate process"}],"multi_select":false}]}`}

	type result struct{ out ToolOutcome }
	done := make(chan result, 1)
	go func() { done <- result{e.ExecuteOutcome(context.Background(), inv)} }()

	deadline := time.Now().Add(2 * time.Second)
	for len(PendingQuestions()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(PendingQuestions()) != 1 {
		t.Fatalf("pending = %v", PendingQuestions())
	}
	if _, err := AnswerQuestion("session-test", "call-1", QuestionAnswerBatch{Answers: []QuestionAnswer{{ID: "q1", Selected: []string{"standalone"}}}}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	select {
	case r := <-done:
		if r.out.IsError {
			t.Fatalf("error: %s", r.out.Text)
		}
		var got QuestionAnswerBatch
		if err := json.Unmarshal([]byte(r.out.Text), &got); err != nil {
			t.Fatalf("decode %s: %v", r.out.Text, err)
		}
		if len(got.Answers) != 1 || got.Answers[0].Selected[0] != "standalone" {
			t.Fatalf("answers = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not return after the answer")
	}
	if len(PendingQuestions()) != 0 {
		t.Fatalf("still pending: %v", PendingQuestions())
	}
}

func TestAskUserQuestionReleasesOnCancel(t *testing.T) {
	e := NewToolExecutor(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ToolOutcome, 1)
	go func() {
		done <- e.ExecuteOutcome(ctx, ToolInvocation{SessionID: "s2", CallID: "c2", Name: askUserQuestionToolName,
			Arguments: `{"questions":[{"id":"q","question":"hi"}]}`})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case out := <-done:
		if !out.IsError || !strings.Contains(out.Text, "turn ended") {
			t.Fatalf("outcome = %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not release the tool")
	}
	if len(PendingQuestions()) != 0 {
		t.Fatalf("leaked: %v", PendingQuestions())
	}
}

func TestAnswerQuestionRejectsBadBatches(t *testing.T) {
	e := NewToolExecutor(t.TempDir())
	done := make(chan ToolOutcome, 1)
	go func() {
		done <- e.ExecuteOutcome(context.Background(), ToolInvocation{SessionID: "s3", CallID: "c3", Name: askUserQuestionToolName,
			Arguments: `{"questions":[{"id":"a","question":"A"},{"id":"b","question":"B"}]}`})
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := AnswerQuestion("s3", "c3", QuestionAnswerBatch{Answers: []QuestionAnswer{{ID: "a"}}}); err != ErrIncompleteAnswers {
		t.Fatalf("partial batch err = %v", err)
	}
	if _, err := AnswerQuestion("s3", "c3", QuestionAnswerBatch{Answers: []QuestionAnswer{{ID: "a"}, {ID: "a"}}}); err != ErrIncompleteAnswers {
		t.Fatalf("duplicate err = %v", err)
	}
	if _, err := AnswerQuestion("s3", "c3", QuestionAnswerBatch{Answers: []QuestionAnswer{{ID: "a"}, {ID: "b"}}}); err != nil {
		t.Fatalf("good batch: %v", err)
	}
	<-done
	if _, err := AnswerQuestion("s3", "c3", QuestionAnswerBatch{Answers: []QuestionAnswer{{ID: "a"}, {ID: "b"}}}); err != ErrNoSuchQuestion {
		t.Fatalf("second answer err = %v", err)
	}
}
