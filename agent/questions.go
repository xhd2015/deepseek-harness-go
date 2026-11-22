package agent

import (
	"context"
	"sync"
	"time"
)

// The ask_user_question tool stops a turn until a person answers.
//
// Unlike every other tool, its result does not come from the machine: it comes
// from the browser. Two halves therefore live here — the registry a call waits
// on, and the answer entry point the API calls — because both must agree on one
// key or the question waits forever.
//
// # Why there is a wait window
//
// The session page decides a question is answerable from the `userQuestions`
// projection, and that projection only tracks calls whose schema declares a
// `timeout` parameter: a call without one belongs to a client that answers
// through an in-process waterfall, which a browser connected over the network
// does not have. Offering the window is therefore what makes a question
// answerable from the page at all, not a convenience.
//
// The window closes into one of two outcomes, and they are different records:
//
//	answered in time   the batch is this call's tool result; nothing else is written
//	window expired     the result says pending, the questions stay answerable,
//	                   and a later answer becomes a user message that continues
//	                   the turn
//
// Node DSH records exactly these two shapes, which is what keeps a session
// written here readable there.

// QuestionOption is one choice the user may pick.
type QuestionOption struct {
	Label string `json:"label"`
	// Description explains the tradeoff in one sentence; optional.
	Description string `json:"description,omitempty"`
}

// QuestionItem is one question inside an ask.
type QuestionItem struct {
	// ID is stable for the call and echoed in the answer, so a model that asks
	// several questions at once can tell which answer belongs where.
	ID       string           `json:"id"`
	Question string           `json:"question"`
	Header   string           `json:"header,omitempty"`
	Options  []QuestionOption `json:"options,omitempty"`
	// MultiSelect lets the user pick more than one option.
	MultiSelect bool `json:"multi_select,omitempty"`
}

// QuestionAnswer is one answer: what the user selected, plus anything written
// instead of or in addition to the options.
type QuestionAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// QuestionAnswerBatch is the whole reply to one ask.
type QuestionAnswerBatch struct {
	Answers []QuestionAnswer `json:"answers"`
}

// PendingQuestionResult is the tool result of a question whose window expired.
//
// The model is told to continue with independent work and that the question
// remains answerable — never that it was skipped, because a skipped answer is a
// decision the user made and a pending one is a decision still to come.
type PendingQuestionResult struct {
	Pending bool   `json:"pending"`
	CallID  string `json:"callId"`
	Message string `json:"message"`
}

// PendingNotice is the text a pending result carries. It is a field of the result
// value rather than prose the model has to interpret, because the page reads the
// recorded result back to tell the timeout from an answer batch.
const PendingNotice = "No answer batch arrived before the timeout. This is pending, not a skipped answer. " +
	"Continue useful independent work. The user can still answer; their reply will arrive as a user message " +
	"identifying this call and the original questions. Do not treat this as permission."

// Errors the answer path reports. They are distinct values because the page words
// each one differently: a stale answer is not the user's mistake.
var (
	// ErrNoSuchQuestion reports an answer for an ask that is not waiting.
	ErrNoSuchQuestion = errorString("this question is no longer waiting for an answer")
	// ErrAlreadyAnswered reports a second answer to one ask.
	ErrAlreadyAnswered = errorString("this question was already answered")
	// ErrIncompleteAnswers reports a batch that does not name each question once.
	ErrIncompleteAnswers = errorString("the answer batch must name each question exactly once")
)

type errorString string

func (e errorString) Error() string { return string(e) }

// DefaultQuestionWait is the foreground window when the model names none. It is
// long enough to answer a question from a phone and short enough that a model
// called away from an empty room is not stuck for the rest of the session.
const DefaultQuestionWait = 120 * time.Second

// waitingQuestion is one open ask.
type waitingQuestion struct {
	sessionID string
	callID    string
	items     []QuestionItem
	// reply carries the batch. Buffered so answering never blocks on a waiter
	// already on its way out.
	reply chan *QuestionAnswerBatch
	// blocked is true while a tool call is still inside its window. False means
	// the window expired and a late answer must continue the turn instead of
	// resolving a call that already returned.
	blocked bool
	// answered stops a second batch from being accepted in the window between the
	// first one being sent and the waiter removing the entry.
	answered bool
}

// questionRegistry holds the asks still waiting for a person in this process.
//
// It is process-scoped because the request that answers one has no handle on the
// runner that started the call, and rebuilding that handle per request would make
// the answer depend on unrelated state.
var questionRegistry = struct {
	mu      sync.Mutex
	waiting map[string]*waitingQuestion
}{waiting: map[string]*waitingQuestion{}}

// questionKey identifies one ask. The session is part of the key because call
// ids are unique only within a session.
func questionKey(sessionID, callID string) string {
	return sessionID + "\x00" + callID
}

// awaitQuestion registers one ask and waits for its window to close.
//
// It returns the batch when the user answered in time, timedOut when the window
// expired (the ask stays answerable), or ctx.Err() when the turn went away.
func awaitQuestion(ctx context.Context, sessionID, callID string, items []QuestionItem, wait time.Duration) (*QuestionAnswerBatch, bool, error) {
	waiting := &waitingQuestion{
		sessionID: sessionID,
		callID:    callID,
		items:     items,
		reply:     make(chan *QuestionAnswerBatch, 1),
		blocked:   true,
	}
	key := questionKey(sessionID, callID)
	questionRegistry.mu.Lock()
	questionRegistry.waiting[key] = waiting
	questionRegistry.mu.Unlock()

	var expired <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expired = timer.C
	}

	select {
	case batch := <-waiting.reply:
		questionRegistry.mu.Lock()
		delete(questionRegistry.waiting, key)
		questionRegistry.mu.Unlock()
		return batch, false, nil
	case <-expired:
		// The ask stays in the registry: the user can still answer it, and that
		// answer must reach the model as a continuation rather than vanish.
		questionRegistry.mu.Lock()
		waiting.blocked = false
		questionRegistry.mu.Unlock()
		return nil, true, nil
	case <-ctx.Done():
		questionRegistry.mu.Lock()
		delete(questionRegistry.waiting, key)
		questionRegistry.mu.Unlock()
		return nil, false, ctx.Err()
	}
}

// AnswerQuestion delivers one answer batch to the ask it names, and reports
// whether a waiting call took it.
//
// A batch nobody took is not an error the user should see: the question was still
// open, their answer is valid, and it becomes the message that continues the
// turn. Only a batch for a question that is gone entirely is refused.
func AnswerQuestion(sessionID, callID string, batch QuestionAnswerBatch) (delivered bool, err error) {
	if sessionID == "" || callID == "" {
		return false, ErrNoSuchQuestion
	}
	key := questionKey(sessionID, callID)
	questionRegistry.mu.Lock()
	waiting, ok := questionRegistry.waiting[key]
	if !ok {
		questionRegistry.mu.Unlock()
		return false, ErrNoSuchQuestion
	}
	if waiting.answered {
		questionRegistry.mu.Unlock()
		return false, ErrAlreadyAnswered
	}
	if err := validateAnswerBatch(waiting.items, batch); err != nil {
		questionRegistry.mu.Unlock()
		return false, err
	}
	waiting.answered = true
	blocked := waiting.blocked
	questionRegistry.mu.Unlock()

	if !blocked {
		// The window expired: no call is waiting, so the answer continues the turn
		// through a user message. The entry is done either way.
		questionRegistry.mu.Lock()
		delete(questionRegistry.waiting, key)
		questionRegistry.mu.Unlock()
		return false, nil
	}
	waiting.reply <- &batch
	return true, nil
}

// validateAnswerBatch checks the batch names each question of the ask once.
func validateAnswerBatch(items []QuestionItem, batch QuestionAnswerBatch) error {
	if len(batch.Answers) != len(items) {
		return ErrIncompleteAnswers
	}
	seen := make(map[string]bool, len(batch.Answers))
	for _, answer := range batch.Answers {
		if seen[answer.ID] {
			return ErrIncompleteAnswers
		}
		seen[answer.ID] = true
	}
	for _, item := range items {
		if !seen[item.ID] {
			return ErrIncompleteAnswers
		}
	}
	return nil
}

// PendingQuestions reports the asks still waiting, for diagnostics and for tests
// that must not depend on timing.
func PendingQuestions() []string {
	questionRegistry.mu.Lock()
	defer questionRegistry.mu.Unlock()
	keys := make([]string, 0, len(questionRegistry.waiting))
	for key := range questionRegistry.waiting {
		keys = append(keys, key)
	}
	return keys
}
