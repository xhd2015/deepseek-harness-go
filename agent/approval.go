package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// Approval is the one gate the model cannot pass on its own.
//
// DSH writes an `approval/asked` record, waits for an answerer, then writes the
// matching `approval/decided`. The pair is the audit trail: a decision with no
// question, or a question with no decision, is a log a reader cannot trust. The
// wait happens between the two writes, which is why the ask must be inside an
// open turn — a bare pair between turns is crash-tail garbage on reload.

// ApprovalOutcome is the closed vocabulary, identical to DSH's.
type ApprovalOutcome string

const (
	// ApprovalAllowedOnce is the only grant: one call, no standing permission.
	ApprovalAllowedOnce ApprovalOutcome = "allowed-once"
	ApprovalRejected    ApprovalOutcome = "rejected"
	ApprovalCancelled   ApprovalOutcome = "cancelled"
	// ApprovalUnavailable is the fail-closed answer when nobody can decide.
	ApprovalUnavailable ApprovalOutcome = "unavailable"
)

// ApprovalRequest is what the page is shown.
type ApprovalRequest struct {
	ID       string `json:"id"`
	Session  string `json:"sessionId"`
	ToolName string `json:"toolName"`
	CallID   string `json:"callId,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// approvalGate carries pending approvals between the runner and the page.
type approvalGate struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

// pendingApproval is one question waiting for an answer.
type pendingApproval struct {
	request ApprovalRequest
	answer  chan ApprovalOutcome
}

// newApprovalGate builds an empty gate.
func newApprovalGate() *approvalGate {
	return &approvalGate{pending: map[string]*pendingApproval{}}
}

// ask registers one question and returns the channel its answer arrives on.
func (g *approvalGate) ask(request ApprovalRequest) chan ApprovalOutcome {
	answer := make(chan ApprovalOutcome, 1)
	g.mu.Lock()
	g.pending[request.ID] = &pendingApproval{request: request, answer: answer}
	g.mu.Unlock()
	return answer
}

// settle delivers one answer, reporting whether the question was still open.
func (g *approvalGate) settle(id string, outcome ApprovalOutcome) bool {
	g.mu.Lock()
	entry, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	entry.answer <- outcome
	return true
}

// PendingApprovals lists the open questions, for a page that just reconnected.
func (r *Runner) PendingApprovals() []ApprovalRequest {
	r.approvals.mu.Lock()
	defer r.approvals.mu.Unlock()
	requests := make([]ApprovalRequest, 0, len(r.approvals.pending))
	for _, entry := range r.approvals.pending {
		requests = append(requests, entry.request)
	}
	return requests
}

// AnswerApproval delivers a user's decision. It reports whether a question was
// waiting: answering nothing must not read as success.
func (r *Runner) AnswerApproval(id string, outcome ApprovalOutcome) bool {
	switch outcome {
	case ApprovalAllowedOnce, ApprovalRejected, ApprovalCancelled, ApprovalUnavailable:
	default:
		return false
	}
	return r.approvals.settle(id, outcome)
}

// requestApproval writes the audit pair around one wait.
//
// The `never` policy short-circuits before any wait: it is a decision the log can
// state without asking, and pretending to ask would make the audit trail lie about
// whether the user was consulted.
func (r *Runner) requestApproval(sess *session.Session, requested, effective string, inv ToolInvocation, command string) ApprovalOutcome {
	toolName := inv.Name
	if toolName == "" {
		toolName = "bash"
	}
	reason := approvalReason(command, requested, effective)
	return r.askApproval(sess, toolName, inv.CallID, reason)
}

// askApproval writes the audit pair around one wait.
func (r *Runner) askApproval(sess *session.Session, toolName, callID, reason string) ApprovalOutcome {
	policy := e_policyOf(r, sess)
	if policy.Approval == session.ApprovalNever {
		// No prompt is shown under `never`; the log states the refusal without
		// pretending a question was ever put to the user.
		return ApprovalRejected
	}
	id := "approval-" + session.NewID()
	if _, err := sess.Append(session.EventApprovalAsked, map[string]any{
		"id":       id,
		"toolName": toolName,
		"callId":   callID,
		"reason":   reason,
	}); err != nil {
		// Without the question the pair cannot be honest; fail closed. The failure
		// is logged because the alternative is a silent `unavailable` that looks
		// identical to "nobody was listening" from the outside.
		r.logf("session %s: cannot record approval/asked: %v", sess.ID(), err)
		return ApprovalUnavailable
	}
	answer := r.approvals.ask(ApprovalRequest{
		ID: id, Session: sess.ID(), ToolName: toolName, CallID: callID, Reason: reason,
	})
	outcome := ApprovalUnavailable
	request := ApprovalRequest{
		ID: id, Session: sess.ID(), ToolName: toolName, CallID: callID, Reason: reason,
	}
	if r.answerer != nil {
		outcome = r.answerer(request)
	} else {
		// The page decides. With no listener installed, nobody can answer, so the
		// request fails closed immediately instead of waiting out a window that has
		// no one on the other end.
		if r.notifyApproval == nil {
			// No page hook: this deployment cannot ask. Logged so the difference
			// between "not wired" and "asked but unanswered" is visible.
			r.logf("session %s: approval %s has no notifier installed", sess.ID(), id)
			outcome = ApprovalUnavailable
		} else {
			// The notifier must only DELIVER the question. Running the wait inside it
			// made the answer undeliverable: the notifier blocked on the waterfall
			// until its own timeout, so a decision arriving on `answer` had nothing
			// reading it and the caller waited out the whole window while the page
			// showed no pending item. Publishing asynchronously and waiting here
			// keeps one wait, owned by the caller.
			go r.notifyApproval(request)
			// The wait is bounded by the same window the page is given: a decision
			// that never arrives must release the call, not hold the turn open.
			select {
			case outcome = <-answer:
			case <-time.After(r.approvalWindow()):
				outcome = ApprovalUnavailable
			}
			// Whatever ends the wait — a timeout, a cancellation, another answerer —
			// the question is over. Withdrawing it is what keeps a consumer from
			// offering a button for a decision that can no longer take effect.
			if r.withdrawApproval != nil {
				r.withdrawApproval(request, outcome)
			}
		}
	}
	if outcome == "" {
		outcome = ApprovalUnavailable
	}
	if _, err := sess.Append(session.EventApprovalDecided, map[string]any{
		"id":      id,
		"outcome": string(outcome),
	}); err != nil {
		return ApprovalUnavailable
	}
	return outcome
}

// e_policyOf reads one session's folded permission state.
func e_policyOf(r *Runner, sess *session.Session) session.PermissionState {
	return session.FoldPermission(sess.Events())
}

// neverSentenceFor exposes the policy statement the model is shown.
func neverSentenceFor() string { return neverSentence }

// approvalReason renders the escalation a caller is asking for.
func approvalReason(command, requested, effective string) string {
	return fmt.Sprintf("Retry under %s (currently %s): %s", requested, effective, command)
}

// SetApprovalNotifier installs the callback that tells the page a question is
// waiting. Without it the question is recorded and fails closed on timeout.
func (r *Runner) SetApprovalNotifier(notify func(ApprovalRequest)) {
	r.mu.Lock()
	r.notifyApproval = notify
	r.mu.Unlock()
}

// RequestApprovalForTest asks one approval by hand, for tests and diagnostics.
func (r *Runner) RequestApprovalForTest(sess *session.Session, toolName, callID, reason string) ApprovalOutcome {
	return r.askApproval(sess, toolName, callID, reason)
}

// SetApprovalAnswerer installs an in-process decision, used by tests and by a
// deployment that answers approvals itself instead of through the page.
func (r *Runner) SetApprovalAnswerer(answer func(ApprovalRequest) ApprovalOutcome) {
	r.mu.Lock()
	r.answerer = answer
	r.mu.Unlock()
}

// ToolsExecuteForTest exposes the raw outcome of the real dispatch path.
//
// It returns the outcome rather than a summary: an earlier version guessed the
// result from the message text and reported success for a call that had failed for
// an unrelated reason, which is exactly the kind of helper that hides bugs.
func (r *Runner) ToolsExecuteForTest(ctx context.Context, sess *session.Session, name, arguments string) ToolOutcome {
	return r.tools.ExecuteOutcome(ctx, ToolInvocation{
		SessionID: sess.ID(),
		Session:   sess,
		Cwd:       sess.Cwd(),
		Name:      name,
		CallID:    "test-call",
		Arguments: arguments,
	})
}

// ClearApprovalNotifier removes the page hook, so an ask fails closed with no one
// to consult. Tests use it to exercise the no-listener path.
func (r *Runner) ClearApprovalNotifier() {
	r.mu.Lock()
	r.notifyApproval = nil
	r.mu.Unlock()
}

// approvalWindow bounds how long a call waits for a decision.
func (r *Runner) approvalWindow() time.Duration {
	r.mu.Lock()
	window := r.approvalWait
	r.mu.Unlock()
	if window <= 0 {
		return 5 * time.Minute
	}
	return window
}

// SetApprovalWindow changes how long a call waits for a decision.
//
// A test shortens it rather than waiting out the production default; a deployment
// may lengthen it when its operator steps away.
func (r *Runner) SetApprovalWindow(window time.Duration) {
	r.mu.Lock()
	r.approvalWait = window
	r.mu.Unlock()
}

// SetApprovalWithdrawer installs the callback that tells the page a question is
// over, whatever ended it.
//
// A consumer that only learns about questions would keep a card on screen after the
// window expired, and the user would answer a question whose answer is already
// recorded — the button would appear to do nothing.
func (r *Runner) SetApprovalWithdrawer(withdraw func(ApprovalRequest, ApprovalOutcome)) {
	r.mu.Lock()
	r.withdrawApproval = withdraw
	r.mu.Unlock()
}
