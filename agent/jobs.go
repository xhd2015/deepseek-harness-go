package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xhd2015/deepseek-harness-go/session"
)

const (
	jobLiveRetention    = 256 * 1024
	jobSettledRetention = 16 * 1024
	jobOwnerLimit       = 10
	jobWaitDefault      = 30 * time.Second
	jobWaitMax          = 600 * time.Second
)

type jobManager struct {
	mu   sync.Mutex
	seq  uint64
	jobs map[string]*backgroundJob
}

type backgroundJob struct {
	mu sync.Mutex

	id         string
	kind       string
	label      string
	owner      string
	seq        uint64
	status     string
	detail     string
	startedAt  int64
	finishedAt int64

	stdout        []byte
	stderr        []byte
	stdoutRead    int
	stderrRead    int
	stdoutDropped bool
	stderrDropped bool

	cancel          func()
	cancelRequested bool
	done            chan struct{}
	notify          func(string)
	emit            func(string, any) error

	suppressNotice bool
	settleCause    string
	killReason     string
	toolCall       map[string]any
	waiters        map[chan struct{}]struct{}
}

func newJobManager() *jobManager {
	return &jobManager{jobs: map[string]*backgroundJob{}}
}

func randomJobID(kind string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", kind, time.Now().UnixNano())
	}
	return kind + "-" + hex.EncodeToString(buf)
}

func (m *jobManager) register(inv ToolInvocation, kind, label string) (*backgroundJob, error) {
	owner := ""
	if inv.SessionID != "" {
		owner = "session-" + inv.SessionID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	seq := m.seq
	live := 0
	for _, job := range m.jobs {
		job.mu.Lock()
		active := job.status == "running" || job.status == "stopping"
		job.mu.Unlock()
		if job.owner == owner && active {
			live++
		}
	}
	if live >= jobOwnerLimit {
		return nil, fmt.Errorf("background job limit reached for this owner (limit: %d); use job_kill to stop an unneeded job, wait for it to finish, then retry", jobOwnerLimit)
	}
	id := randomJobID(kind)
	job := &backgroundJob{
		id:        id,
		kind:      kind,
		label:     label,
		owner:     owner,
		seq:       seq,
		status:    "running",
		startedAt: time.Now().UnixMilli(),
		done:      make(chan struct{}),
		waiters:   map[chan struct{}]struct{}{},
		notify:    inv.Notify,
		emit:      inv.Emit,
	}
	if inv.CallID != "" {
		job.toolCall = map[string]any{"callId": inv.CallID, "rootCallId": inv.RootCallID}
	}
	m.jobs[id] = job
	if inv.Emit != nil {
		data := map[string]any{
			"jobId":     id,
			"owner":     owner,
			"kind":      kind,
			"label":     label,
			"startedAt": job.startedAt,
		}
		if job.toolCall != nil {
			data["toolCall"] = job.toolCall
		}
		if err := inv.Emit(session.EventJobRegistered, data); err != nil {
			delete(m.jobs, id)
			return nil, fmt.Errorf("failed to register background job: %v", err)
		}
	}
	return job, nil
}

func jobOwnerForSession(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return "session-" + sessionID
}

// lookup enforces the same isolation fence as DSH: a job is reachable only by
// its owner, while unowned jobs are open to every caller.
func (m *jobManager) lookup(id, owner string) (*backgroundJob, error) {
	m.mu.Lock()
	job := m.jobs[id]
	m.mu.Unlock()
	if job == nil {
		return nil, fmt.Errorf("unknown job %s", id)
	}
	if job.owner != "" && job.owner != owner {
		return nil, fmt.Errorf("job %s belongs to another session", id)
	}
	return job, nil
}

func (m *jobManager) list(owner string) []*backgroundJob {
	m.mu.Lock()
	jobs := make([]*backgroundJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		if job.owner == "" || job.owner == owner {
			jobs = append(jobs, job)
		}
	}
	m.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].seq < jobs[j].seq })
	return jobs
}

// listAll returns every job regardless of owner. Teardown must cancel the jobs
// owned by the agent sessions too; list("") intentionally only exposes unowned
// jobs to a caller without a session.
func (m *jobManager) listAll() []*backgroundJob {
	m.mu.Lock()
	jobs := make([]*backgroundJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].seq < jobs[j].seq })
	return jobs
}

func (m *jobManager) closeAll() {
	jobs := m.listAll()
	for _, job := range jobs {
		job.mu.Lock()
		if job.status == "running" || job.status == "stopping" {
			job.settleCause = "teardown"
			job.suppressNotice = true
			cancel := job.cancel
			job.cancelRequested = true
			job.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		} else {
			job.mu.Unlock()
		}
	}
	// Cancellation is asynchronous. Give producers a bounded chance to settle
	// so teardown does not return while their child processes are still dying.
	deadline := time.After(3 * time.Second)
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-deadline:
			return
		}
	}
}

type jobWriter struct {
	job     *backgroundJob
	channel string
}

func (w jobWriter) Write(p []byte) (int, error) {
	w.job.appendOutput(w.channel, p)
	return len(p), nil
}

func (j *backgroundJob) appendOutput(channel string, data []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if channel == "stderr" {
		j.stderr = append(j.stderr, data...)
		if len(j.stderr) > jobLiveRetention {
			drop := len(j.stderr) - jobLiveRetention
			j.stderr = append([]byte(nil), j.stderr[drop:]...)
			if j.stderrRead >= drop {
				j.stderrRead -= drop
			} else {
				j.stderrRead = 0
				j.stderrDropped = true
			}
		}
		return
	}
	j.stdout = append(j.stdout, data...)
	if len(j.stdout) > jobLiveRetention {
		drop := len(j.stdout) - jobLiveRetention
		j.stdout = append([]byte(nil), j.stdout[drop:]...)
		if j.stdoutRead >= drop {
			j.stdoutRead -= drop
		} else {
			j.stdoutRead = 0
			j.stdoutDropped = true
		}
	}
}

// attach arms cancellation for a started process. A stop request may arrive
// between registration and a successful spawn, so attach must honor the
// already-recorded request instead of losing it.
func (j *backgroundJob) attach(cancel func()) {
	j.mu.Lock()
	j.cancel = cancel
	stop := j.cancelRequested
	j.mu.Unlock()
	if stop && cancel != nil {
		cancel()
	}
}

// settle records the terminal outcome once. The durable job/settled event is
// appended before a completion notice is delivered.
func (j *backgroundJob) settle(status, detail string, producerReported bool) {
	j.mu.Lock()
	if j.status == "completed" || j.status == "killed" || j.status == "failed" {
		j.mu.Unlock()
		return
	}
	cause := "producer"
	if j.settleCause == "teardown" {
		// Teardown announced its cause before cancelling; the producer's Wait
		// reports the signal, but the settlement still belongs to teardown.
		cause = "teardown"
	} else if j.status == "stopping" {
		// A model kill sets the cause before the producer reports the signal.
		cause = "kill"
		if status == "killed" && j.killReason != "" {
			detail += "; " + j.killReason
		}
	}
	j.status = status
	j.detail = detail
	j.finishedAt = time.Now().UnixMilli()
	j.settleCause = cause
	suppress := j.suppressNotice
	notify := j.notify
	waiters := make([]chan struct{}, 0, len(j.waiters))
	for waiter := range j.waiters {
		waiters = append(waiters, waiter)
	}
	j.waiters = map[chan struct{}]struct{}{}
	j.trimSettledLocked()
	data := map[string]any{
		"jobId":            j.id,
		"owner":            j.owner,
		"kind":             j.kind,
		"label":            j.label,
		"startedAt":        j.startedAt,
		"finishedAt":       j.finishedAt,
		"status":           status,
		"cause":            cause,
		"producerReported": producerReported,
	}
	if j.toolCall != nil {
		data["toolCall"] = j.toolCall
	}
	if detail != "" {
		data["detail"] = detail
	}
	j.mu.Unlock()
	// Commit the durable settlement before releasing waits so a job_output that
	// observes the terminal status can rely on the event already being logged.
	if j.emit != nil {
		_ = j.emit(session.EventJobSettled, data)
	}
	close(j.done)
	for _, waiter := range waiters {
		close(waiter)
	}
	if notify != nil && !suppress && len(waiters) == 0 && cause != "teardown" {
		notify(fmt.Sprintf("background job %s (%s: %s) finished %s. Read its output with job_output.",
			j.id, j.kind, j.label, j.statusLine()))
	}
}

func (j *backgroundJob) trimSettledLocked() {
	trim := func(data []byte, read int) ([]byte, int) {
		keep := len(data) - read
		if keep < jobSettledRetention {
			keep = jobSettledRetention
		}
		if keep >= len(data) {
			return data, read
		}
		drop := len(data) - keep
		next := read - drop
		if next < 0 {
			next = 0
		}
		return append([]byte(nil), data[drop:]...), next
	}
	j.stdout, j.stdoutRead = trim(j.stdout, j.stdoutRead)
	j.stderr, j.stderrRead = trim(j.stderr, j.stderrRead)
}

func (j *backgroundJob) statusLine() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.detail != "" {
		return fmt.Sprintf("[status: %s, %s]", j.status, j.detail)
	}
	return fmt.Sprintf("[status: %s]", j.status)
}

func (j *backgroundJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	value := map[string]any{
		"id":        j.id,
		"kind":      j.kind,
		"label":     j.label,
		"status":    j.status,
		"startedAt": j.startedAt,
	}
	if j.detail != "" {
		value["detail"] = j.detail
	}
	if j.finishedAt != 0 {
		value["finishedAt"] = j.finishedAt
	}
	return value
}

// registerWaiter adds a live wait. A settlement that releases it is reported as
// awaited, so the tool result is the model's only delivery of the completion.
func (j *backgroundJob) registerWaiter() chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == "completed" || j.status == "killed" || j.status == "failed" {
		return nil
	}
	waiter := make(chan struct{})
	j.waiters[waiter] = struct{}{}
	return waiter
}

func (j *backgroundJob) removeWaiter(waiter chan struct{}) {
	if waiter == nil {
		return
	}
	j.mu.Lock()
	delete(j.waiters, waiter)
	j.mu.Unlock()
}

// read returns the output since the previous consuming read.
func (j *backgroundJob) read() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := string(j.stdout[j.stdoutRead:])
	errText := string(j.stderr[j.stderrRead:])
	j.stdoutRead = len(j.stdout)
	j.stderrRead = len(j.stderr)
	lossy := j.stdoutDropped || j.stderrDropped
	j.stdoutDropped = false
	j.stderrDropped = false
	body := out
	if body != "" && errText != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if errText != "" {
		body += "[stderr]\n" + errText
	}
	if lossy {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "[some output was dropped from memory; full output: (unavailable)]"
	}
	return body
}

func (j *backgroundJob) finished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == "completed" || j.status == "killed" || j.status == "failed"
}

func (j *backgroundJob) stopByModel(reason string) bool {
	j.mu.Lock()
	if j.status == "completed" || j.status == "killed" || j.status == "failed" {
		j.mu.Unlock()
		return false
	}
	j.suppressNotice = true
	j.status = "stopping"
	j.killReason = reason
	j.cancelRequested = true
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (j *backgroundJob) startProcess(cmd *exec.Cmd) error {
	cmd.Stdout = jobWriter{job: j, channel: "stdout"}
	cmd.Stderr = jobWriter{job: j, channel: "stderr"}
	if err := cmd.Start(); err != nil {
		j.settle("failed", err.Error(), false)
		return err
	}
	j.attach(func() {
		_ = terminateProcessGroup(cmd)
	})
	go func() {
		waitErr := cmd.Wait()
		exitCode := 0
		signalName := ""
		if waitErr != nil {
			var exitErr *exec.ExitError
			if !isExitError(waitErr, &exitErr) {
				j.settle("failed", waitErr.Error(), true)
				return
			}
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signalName = signalText(status.Signal())
			} else {
				exitCode = exitErr.ExitCode()
			}
		}
		if signalName != "" {
			j.settle("killed", "signal: "+signalName, true)
			return
		}
		j.settle("completed", fmt.Sprintf("exit code: %d", exitCode), true)
	}()
	return nil
}

func (e *ToolExecutor) jobOutput(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	id, ok := stringArg(args, "job_id")
	if !ok || strings.TrimSpace(id) == "" {
		return ToolOutcome{Text: "invalid job_id: expected a non-empty string", IsError: true}
	}
	owner := jobOwnerForSession(inv.SessionID)
	job, err := e.jobs.lookup(id, owner)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	wait := boolArg(args, "wait")
	timeout := jobWaitDefault
	if raw, present := args["timeout_ms"]; present {
		value, ok := numberArg(args, "timeout_ms")
		if !ok || value <= 0 {
			return ToolOutcome{Text: fmt.Sprintf("invalid timeout_ms: expected a positive number, got %v", raw), IsError: true}
		}
		if value > float64(jobWaitMax/time.Millisecond) {
			value = float64(jobWaitMax / time.Millisecond)
		}
		timeout = time.Duration(value * float64(time.Millisecond))
	}
	if wait {
		if waiter := job.registerWaiter(); waiter != nil {
			timer := time.NewTimer(timeout)
			select {
			case <-waiter:
			case <-timer.C:
				job.removeWaiter(waiter)
			case <-ctx.Done():
				job.removeWaiter(waiter)
				timer.Stop()
				return ToolOutcome{Text: "[stopped: cancelled]", IsError: true}
			}
			timer.Stop()
		}
	}
	body := job.read()
	if body == "" {
		body = "(no new output)"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += job.statusLine()
	return ToolOutcome{Text: body}
}

func (e *ToolExecutor) jobList(inv ToolInvocation) ToolOutcome {
	jobs := e.jobs.list(jobOwnerForSession(inv.SessionID))
	if len(jobs) == 0 {
		return ToolOutcome{Text: "(no background jobs)"}
	}
	lines := make([]string, 0, len(jobs))
	for _, job := range jobs {
		job.mu.Lock()
		lines = append(lines, fmt.Sprintf("%s [%s] %s — %s", job.id, job.kind, job.status, job.label))
		job.mu.Unlock()
	}
	return ToolOutcome{Text: strings.Join(lines, "\n")}
}

func (e *ToolExecutor) jobKill(inv ToolInvocation, args map[string]any) ToolOutcome {
	id, ok := stringArg(args, "job_id")
	if !ok || strings.TrimSpace(id) == "" {
		return ToolOutcome{Text: "invalid job_id: expected a non-empty string", IsError: true}
	}
	reason, _ := stringArg(args, "reason")
	owner := jobOwnerForSession(inv.SessionID)
	job, err := e.jobs.lookup(id, owner)
	if err != nil {
		return ToolOutcome{Text: err.Error(), IsError: true}
	}
	if job.stopByModel(reason) {
		return ToolOutcome{Text: "requested cancellation of job " + id}
	}
	return ToolOutcome{Text: fmt.Sprintf("job %s had already finished %s", id, job.statusLine())}
}

func (e *ToolExecutor) closeJobs() {
	if e != nil && e.jobs != nil {
		e.jobs.closeAll()
	}
}
