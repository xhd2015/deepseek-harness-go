package agent

import (
	"sync"
)

// A question outlives the call that asked it.
//
// The tool returns as soon as the question is recorded, so nothing is waiting when
// the answer arrives: the registry below is only the set of questions this process
// still owes an answer to, kept so a reply can be validated against what was
// actually asked. The page learns that the question is answerable from the
// session's userQuestions projection, not from this map.
//
// Shapes follow DSH: the call is an ordinary tool/call, its result says the
// question was asked, and a reply is a user/message whose source names the call.

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

// AskedQuestionResult is the tool result of a question that was recorded.
//
// It says what happened rather than what was decided: the question was asked, and
// the answer will arrive later as a message. The model must not read this as an
// answer, a skip, or a failure.
type AskedQuestionResult struct {
	Asked   bool   `json:"asked"`
	CallID  string `json:"callId"`
	Message string `json:"message"`
}

// AskedNotice tells the model how to continue after asking.
const AskedNotice = "The question was shown to the user and this turn continues without its answer. " +
	"Continue with what you can do without it. The answer will arrive as a user message identifying this call; " +
	"do not ask it again and do not treat the silence as permission."

// Errors the answer path reports. They are distinct values because the page words
// each one differently: a stale answer is not the user's mistake.
var (
	// ErrNoSuchQuestion reports an answer for a call this process never asked, or
	// one already answered.
	ErrNoSuchQuestion = errorString("this question is no longer waiting for an answer")
	// ErrIncompleteAnswers reports a batch that does not name each question once.
	ErrIncompleteAnswers = errorString("the answer batch must name each question exactly once")
)

type errorString string

func (e errorString) Error() string { return string(e) }

// openQuestion is one ask still owed an answer.
type openQuestion struct {
	items []QuestionItem
}

// questionRegistry holds the asks this process has recorded and not settled.
//
// It is process-scoped because the request that answers one has no handle on the
// session's runner. It is also deliberately small: entries are dropped when
// answered, and a restart simply loses them — the projection folded from the log
// is what still shows the question, and answering it then takes the same path.
var questionRegistry = struct {
	mu   sync.Mutex
	open map[string]openQuestion
}{open: map[string]openQuestion{}}

// questionKey identifies one ask. The session is part of the key because call ids
// are unique only within a session.
func questionKey(sessionID, callID string) string {
	return sessionID + "\x00" + callID
}

// registerQuestion records that a question was asked.
func registerQuestion(sessionID, callID string, items []QuestionItem) {
	questionRegistry.mu.Lock()
	questionRegistry.open[questionKey(sessionID, callID)] = openQuestion{items: items}
	questionRegistry.mu.Unlock()
}

// AnswerQuestion accepts one answer batch for the ask it names.
//
// The batch is validated against the questions actually asked: a reply that names
// a question twice, or misses one, would leave the model unable to tell which
// answer belongs where, so it is refused instead of forwarded.
func AnswerQuestion(sessionID, callID string, batch QuestionAnswerBatch) error {
	if sessionID == "" || callID == "" {
		return ErrNoSuchQuestion
	}
	key := questionKey(sessionID, callID)
	questionRegistry.mu.Lock()
	question, ok := questionRegistry.open[key]
	if !ok {
		questionRegistry.mu.Unlock()
		return ErrNoSuchQuestion
	}
	if err := validateAnswerBatch(question.items, batch); err != nil {
		questionRegistry.mu.Unlock()
		return err
	}
	delete(questionRegistry.open, key)
	questionRegistry.mu.Unlock()
	return nil
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

// PendingQuestions reports the asks still owed an answer, for diagnostics and for
// tests that must not depend on timing.
func PendingQuestions() []string {
	questionRegistry.mu.Lock()
	defer questionRegistry.mu.Unlock()
	keys := make([]string, 0, len(questionRegistry.open))
	for key := range questionRegistry.open {
		keys = append(keys, key)
	}
	return keys
}
