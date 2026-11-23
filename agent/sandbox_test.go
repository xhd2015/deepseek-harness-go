package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadOnlyRefusesEveryWrite covers all three mutation paths: the refusal must
// not be a filesystem-tool-only check, because a shell can write too.
func TestReadOnlyRefusesEveryWrite(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("read-only", "read-only")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inv := ToolInvocation{SessionID: "ro", Cwd: workspace}

	if output, isError := callTool(t, executor, inv, "write", map[string]any{
		"file_path": "a.txt", "content": "x",
	}); !isError || !strings.Contains(output, "[sandbox: file access denied under read-only mode]") {
		t.Fatalf("write under read-only = %q (error=%v)", output, isError)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, isError := callTool(t, executor, inv, "edit", map[string]any{
		"file_path": "b.txt", "old_string": "old", "new_string": "new",
	}); !isError || !strings.Contains(output, "read-only mode") {
		t.Fatalf("edit under read-only = %q (error=%v)", output, isError)
	}
	// The shell is the case that used to slip through entirely.
	if output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "write outside", "command": "echo x > " + filepath.Join(workspace, "c.txt"),
	}); !isError || !strings.Contains(output, "sandbox: file access denied") {
		t.Fatalf("bash write under read-only = %q (error=%v)", output, isError)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "c.txt")); statErr == nil {
		t.Fatal("the refused command still created the file")
	}
}

// TestWorkspaceWriteConfinesTheShell is the acceptance criterion for bash: a write
// outside the workspace fails with DSH's marker, and one inside succeeds.
func TestWorkspaceWriteConfinesTheShell(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("workspace-write", "workspace-write")
	executor.SetApproval("never")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inv := ToolInvocation{SessionID: "ww", Cwd: workspace}

	inside := filepath.Join(workspace, "inside.txt")
	output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "write inside", "command": "echo x > " + inside,
	})
	if isError {
		t.Fatalf("inside write failed: %s", output)
	}
	body, err := os.ReadFile(inside)
	if err != nil || !strings.Contains(string(body), "x") {
		t.Fatalf("inside write did not land: %v %q", err, body)
	}

	outside := filepath.Join(userHomeForTest(t), "eluc-audit-outside.txt")
	output, isError = callTool(t, executor, inv, "bash", map[string]any{
		"description": "write outside", "command": "echo x > " + outside,
	})
	if !isError {
		t.Fatalf("outside write was allowed: %s", output)
	}
	if !strings.Contains(output, "[sandbox: file access denied under workspace-write mode]") {
		t.Fatalf("the refusal is not in DSH's vocabulary: %s", output)
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatal("the refused command still wrote outside the workspace")
	}
}

// TestEscalationWithoutJustificationIsRefused keeps a widened retry from being
// granted on nothing but a mode name.
func TestEscalationWithoutJustificationIsRefused(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("workspace-write", "workspace-write")
	executor.SetApproval("ask")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inv := ToolInvocation{SessionID: "esc", Cwd: workspace}
	output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description":         "escalate",
		"command":             "echo x > " + filepath.Join(userHomeForTest(t), "nope.txt"),
		"sandbox_permissions": "danger-full-access",
	})
	if !isError || !strings.Contains(output, "justification") {
		t.Fatalf("a justification-less escalation was accepted: %q (error=%v)", output, isError)
	}
}

// TestEscalationUnderNeverIsDeterministic states the policy instead of asking.
func TestEscalationUnderNeverIsDeterministic(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("workspace-write", "workspace-write")
	executor.SetApproval("never")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inv := ToolInvocation{SessionID: "never", Cwd: workspace}
	output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description":         "escalate",
		"command":             "echo x > " + filepath.Join(userHomeForTest(t), "nope.txt"),
		"sandbox_permissions": "danger-full-access",
		"justification":       "needed for the task",
	})
	if !isError {
		t.Fatalf("an escalation under `never` was granted: %s", output)
	}
	if !strings.Contains(output, "do not request sandbox escalation") {
		t.Fatalf("the refusal does not carry DSH's never sentence: %s", output)
	}
}

// TestEscalationAsksAndGrantsOnce proves the approved retry runs, and that the next
// call is checked again rather than inheriting the wider mode.
func TestEscalationAsksAndGrantsOnce(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("workspace-write", "workspace-write")
	executor.SetApproval("ask")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	asked := 0
	executor.askApproval = func(inv ToolInvocation, requested, effective, command string) ApprovalOutcome {
		asked++
		return ApprovalAllowedOnce
	}
	target := filepath.Join(userHomeForTest(t), "escalated-once.txt")
	t.Cleanup(func() { _ = os.Remove(target) })
	inv := ToolInvocation{SessionID: "grant", Cwd: workspace}
	output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description":         "escalate",
		"command":             "echo y > " + target,
		"sandbox_permissions": "danger-full-access",
		"justification":       "the task needs a path outside the workspace",
	})
	if isError {
		t.Fatalf("the approved retry failed: %s", output)
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want once", asked)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("the approved write did not land: %v", statErr)
	}
	// The grant was for that call: an identical retry asks again.
	if _, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description":         "escalate again",
		"command":             "echo z > " + target,
		"sandbox_permissions": "danger-full-access",
		"justification":       "same reason",
	}); isError {
		t.Fatal("the second approved retry failed")
	}
	if asked != 2 {
		t.Fatalf("asked %d times total, want 2: the grant was not one-shot", asked)
	}
}

// TestEscalationRejectionStopsTheCall is the other half of the gate.
func TestEscalationRejectionStopsTheCall(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("workspace-write", "workspace-write")
	executor.SetApproval("ask")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	executor.askApproval = func(ToolInvocation, string, string, string) ApprovalOutcome {
		return ApprovalRejected
	}
	target := filepath.Join(userHomeForTest(t), "rejected.txt")
	inv := ToolInvocation{SessionID: "reject", Cwd: workspace}
	output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description":         "escalate",
		"command":             "echo n > " + target,
		"sandbox_permissions": "danger-full-access",
		"justification":       "because",
	})
	if !isError || !strings.Contains(output, "rejected") {
		t.Fatalf("a rejected escalation ran: %q (error=%v)", output, isError)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("a rejected escalation still wrote the file")
	}
}

// TestNarrowerEscalationIsIgnoredRatherThanGranted keeps a no-op request from
// spending a user prompt.
func TestNarrowerEscalationIsIgnoredRatherThanGranted(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.SetPolicy("danger-full-access", "danger-full-access")
	executor.SetApproval("ask")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	asked := 0
	executor.askApproval = func(ToolInvocation, string, string, string) ApprovalOutcome {
		asked++
		return ApprovalAllowedOnce
	}
	inv := ToolInvocation{SessionID: "narrow", Cwd: workspace}
	output, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description":         "noop",
		"command":             "true",
		"sandbox_permissions": "read-only",
		"justification":       "unnecessary",
	})
	if isError {
		t.Fatalf("a no-op escalation failed the call: %s", output)
	}
	if asked != 0 {
		t.Fatalf("asked %d times for a narrower mode, want 0", asked)
	}
}
