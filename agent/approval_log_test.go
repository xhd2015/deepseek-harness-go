package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestApprovalFailuresAreLogged keeps the two silent paths from returning to
// silence.
//
// A deployment with no notifier and a failed audit append both used to answer
// `unavailable` without a word, which is indistinguishable from "asked, nobody
// answered" — the one ambiguity that makes this feature impossible to diagnose
// from the outside.
func TestApprovalFailuresAreLogged(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	runner := New(Config{Provider: "p", Model: "m",
		Logf: func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
		}})
	opened := newOpenSession(t, t.TempDir())
	if got := runner.RequestApprovalForTest(opened, "bash", "c1", "reason"); got != ApprovalUnavailable {
		t.Fatalf("outcome = %q", got)
	}
	mu.Lock()
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "no notifier installed") {
		t.Fatalf("the missing-notifier path did not log: %q", joined)
	}
	if !strings.Contains(joined, opened.ID()) {
		t.Fatalf("the log line does not carry %s: %q", opened.ID(), joined)
	}
}
