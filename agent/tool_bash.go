package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func (e *ToolExecutor) bash(ctx context.Context, inv ToolInvocation, args map[string]any) (string, bool) {
	command, _ := stringArg(args, "command")
	if strings.TrimSpace(command) == "" {
		return "invalid command: expected a non-empty string", true
	}
	description, _ := stringArg(args, "description")
	if strings.TrimSpace(description) == "" {
		return "invalid description: expected a non-empty string", true
	}

	timeout := bashDefaultTimeout
	if raw, present := args["timeoutMs"]; present {
		value, ok := numberArg(args, "timeoutMs")
		if !ok || value <= 0 || value != value {
			return fmt.Sprintf("invalid timeoutMs: expected a positive number, got %v", raw), true
		}
		if value > float64(bashMaxTimeout/time.Millisecond) {
			value = float64(bashMaxTimeout / time.Millisecond)
		}
		timeout = time.Duration(value * float64(time.Millisecond))
	}

	base, err := e.baseDir(inv)
	if err != nil {
		return err.Error(), true
	}
	workdir := base
	if raw, present := args["workdir"]; present {
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "invalid workdir: expected a non-empty string", true
		}
		workdir = text
		if !filepath.IsAbs(workdir) {
			workdir = filepath.Join(base, workdir)
		}
		workdir, err = canonicalDir(workdir)
		if err != nil {
			return fmt.Sprintf("invalid workdir: %v", err), true
		}
	}

	shell, err := bashShell()
	if err != nil {
		return err.Error(), true
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)

	stdout := &tailBuffer{limit: bashMaxOutputBytes}
	stderr := &tailBuffer{limit: bashMaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("failed to spawn bash: %v", err), true
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	timedOut := false
	cancelled := false
	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer.C:
		timedOut = true
		_ = terminateProcessGroup(cmd)
		waitErr = <-done
	case <-ctx.Done():
		cancelled = true
		_ = terminateProcessGroup(cmd)
		waitErr = <-done
	}

	exitCode := 0
	signalName := ""
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !isExitError(waitErr, &exitErr) {
			return fmt.Sprintf("bash failed: %v", waitErr), true
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signalName = signalText(status.Signal())
		} else {
			exitCode = exitErr.ExitCode()
		}
	}
	if cancelled {
		return appendStatus(stdout.String(), "[stopped: cancelled]"), true
	}

	body := composeBashOutput(stdout, stderr)
	markers := []string{}
	if timedOut {
		markers = append(markers, fmt.Sprintf("[timed out after %dms]", timeout.Milliseconds()))
	}
	if signalName != "" {
		markers = append(markers, fmt.Sprintf("[killed by signal: %s]", signalName))
	} else if exitCode != 0 {
		markers = append(markers, fmt.Sprintf("[exit code: %d]", exitCode))
	}
	if len(markers) == 0 {
		return body, false
	}
	return appendStatus(body, strings.Join(markers, "\n")), false
}

func composeBashOutput(stdout, stderr *tailBuffer) string {
	out := stdout.String()
	errText := stderr.String()
	body := out
	if out != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if errText != "" {
		body += "[stderr]\n"
		body += errText
	}
	if body == "" {
		body = "(no output)"
	}
	return body
}

func appendStatus(body, markers string) string {
	if markers == "" {
		return body
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + markers
}

func bashShell() (string, error) {
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	path, err := exec.LookPath("bash")
	if err != nil {
		return "", fmt.Errorf("bash is not available: %v", err)
	}
	return path, nil
}

func isExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

func signalText(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGHUP:
		return "SIGHUP"
	default:
		return strings.ToUpper(signal.String())
	}
}

// tailBuffer retains the last limit bytes written to it while reporting byte
// counts to the child process as if every byte had been consumed.
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		b.truncated = true
		return len(p), nil
	}
	overflow := len(b.data) + len(p) - b.limit
	if overflow > 0 {
		b.data = append(b.data[:0], b.data[overflow:]...)
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := string(b.data)
	if b.truncated {
		text += "\n[output truncated; full output: (unavailable)]"
	}
	return text
}

func (b *tailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data)
}
