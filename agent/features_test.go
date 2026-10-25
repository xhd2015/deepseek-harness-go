package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/session"
)

type eventRecorder struct {
	mu     sync.Mutex
	events []session.Event
}

func (r *eventRecorder) emit(typ string, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, session.Event{Type: typ, Data: encoded})
	return nil
}

func (r *eventRecorder) byType(typ string) []session.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []session.Event
	for _, event := range r.events {
		if event.Type == typ {
			out = append(out, event)
		}
	}
	return out
}

func TestTodoWriteReplacesListAndAllowsParallelActiveItems(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	recorder := &eventRecorder{}
	inv := ToolInvocation{SessionID: "todo", Emit: recorder.emit}
	args := map[string]any{"todos": []any{
		map[string]any{"content": "one", "status": "in_progress"},
		map[string]any{"content": "two", "status": "in_progress"},
		map[string]any{"content": "three", "status": "completed"},
	}}
	asset, isError := callTool(t, executor, inv, "todo_write", args)
	if isError {
		t.Fatalf("todo_write failed: %s", asset)
	}
	if want := "Updated todo list: 0 pending, 2 in progress, 1 completed."; asset != want {
		t.Fatalf("output = %q, want %q", asset, want)
	}
	events := recorder.byType(session.EventTodoWrite)
	if len(events) != 1 {
		t.Fatalf("emitted %d todo events, want 1", len(events))
	}
	var payload struct {
		Todos []todoItem `json:"todos"`
	}
	if err := json.Unmarshal(events[0].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Todos) != 3 || payload.Todos[0].Content != "one" {
		t.Fatalf("todo payload = %+v", payload.Todos)
	}

	bad := map[string]any{"todos": []any{
		map[string]any{"content": "same", "status": "pending"},
		map[string]any{"content": "same", "status": "pending"},
	}}
	asset, isError = callTool(t, executor, inv, "todo_write", bad)
	if !isError || !strings.Contains(asset, "duplicate content") {
		t.Fatalf("duplicate todo = %q, %v", asset, isError)
	}
}

func TestBackgroundBashJobsLifecycle(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	executor.dshHome = t.TempDir()
	recorder := &eventRecorder{}
	var notices []string
	var noticeMu sync.Mutex
	inv := ToolInvocation{
		SessionID: "jobs", CallID: "call-1", RootCallID: "call-1", Emit: recorder.emit,
		Notify: func(text string) {
			noticeMu.Lock()
			notices = append(notices, text)
			noticeMu.Unlock()
		},
	}
	out, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "print in background", "command": "printf background-output", "run_in_background": true,
	})
	if isError || !strings.HasPrefix(out, "started background job ") {
		t.Fatalf("background bash = %q, %v", out, isError)
	}
	jobID := strings.TrimPrefix(out, "started background job ")
	deadline := time.Now().Add(5 * time.Second)
	for {
		asset, _ := callTool(t, executor, inv, "job_output", map[string]any{"job_id": jobID, "wait": true, "timeout_ms": 5000})
		if strings.Contains(asset, "[status: completed, exit code: 0]") {
			if !strings.Contains(asset, "background-output") {
				t.Fatalf("job output missing stdout: %q", asset)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: %q", asset)
		}
	}
	list, isError := callTool(t, executor, inv, "job_list", map[string]any{})
	if isError || !strings.Contains(list, jobID+" [bash] completed") {
		t.Fatalf("job_list = %q, %v", list, isError)
	}
	registered := recorder.byType(session.EventJobRegistered)
	if len(registered) != 1 || len(recorder.byType(session.EventJobSettled)) != 1 {
		raw, _ := json.Marshal(recorder.events)
		t.Fatalf("job events = %s", raw)
	}
	var registration struct {
		JobID    string `json:"jobId"`
		Owner    string `json:"owner"`
		Kind     string `json:"kind"`
		ToolCall struct {
			CallID     string `json:"callId"`
			RootCallID string `json:"rootCallId"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(registered[0].Data, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.JobID != jobID || registration.Owner != "session-jobs" || registration.Kind != "bash" ||
		registration.ToolCall.CallID != "call-1" || registration.ToolCall.RootCallID != "call-1" {
		t.Fatalf("job/registered = %+v", registration)
	}
	noticeMu.Lock()
	joined := strings.Join(notices, "\n")
	noticeMu.Unlock()
	if joined != "" {
		t.Fatalf("a wait that observed settlement must not also notify: %q", joined)
	}
}

func TestJobSettlementNotifiesWhenNotAwaited(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	executor.dshHome = t.TempDir()
	notices := make(chan string, 4)
	inv := ToolInvocation{SessionID: "notify", Notify: func(text string) { notices <- text }}
	out, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "finish soon", "command": "printf done", "run_in_background": true,
	})
	if isError {
		t.Fatalf("background bash failed: %s", out)
	}
	jobID := strings.TrimPrefix(out, "started background job ")
	select {
	case notice := <-notices:
		if !strings.Contains(notice, jobID) || !strings.Contains(notice, "finished [status: completed, exit code: 0]") {
			t.Fatalf("notice = %q", notice)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for settlement notice")
	}
}

func TestTimedOutWaitDoesNotSwallowSettlementNotice(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	executor.dshHome = t.TempDir()
	notices := make(chan string, 4)
	inv := ToolInvocation{SessionID: "notify-timeout", Notify: func(text string) { notices <- text }}
	out, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "finish after wait", "command": "sleep 0.2; printf done", "run_in_background": true,
	})
	if isError {
		t.Fatalf("background bash failed: %s", out)
	}
	jobID := strings.TrimPrefix(out, "started background job ")
	out, isError = callTool(t, executor, inv, "job_output", map[string]any{
		"job_id": jobID, "wait": true, "timeout_ms": 20,
	})
	if isError || !strings.Contains(out, "[status: running]") {
		t.Fatalf("timed-out wait = %q, %v", out, isError)
	}
	select {
	case notice := <-notices:
		if !strings.Contains(notice, jobID) {
			t.Fatalf("notice = %q", notice)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed-out wait suppressed the settlement notice")
	}
}

func TestJobsAreIsolatedByOwningSession(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	executor.dshHome = t.TempDir()
	owner := ToolInvocation{SessionID: "owner"}
	other := ToolInvocation{SessionID: "other"}
	first, isError := callTool(t, executor, owner, "bash", map[string]any{
		"description": "owned", "command": "printf owned; sleep 0.2", "run_in_background": true,
	})
	if isError {
		t.Fatalf("background bash failed: %s", first)
	}
	firstID := strings.TrimPrefix(first, "started background job ")
	second, isError := callTool(t, executor, owner, "bash", map[string]any{
		"description": "owned second", "command": "sleep 0.2", "run_in_background": true,
	})
	if isError {
		t.Fatalf("second background bash failed: %s", second)
	}
	secondID := strings.TrimPrefix(second, "started background job ")

	out, isError := callTool(t, executor, other, "job_output", map[string]any{"job_id": firstID})
	if !isError || out != "job "+firstID+" belongs to another session" {
		t.Fatalf("cross-session output = %q, %v", out, isError)
	}
	out, isError = callTool(t, executor, other, "job_kill", map[string]any{"job_id": firstID})
	if !isError || out != "job "+firstID+" belongs to another session" {
		t.Fatalf("cross-session kill = %q, %v", out, isError)
	}
	out, isError = callTool(t, executor, other, "job_list", map[string]any{})
	if isError || out != "(no background jobs)" {
		t.Fatalf("cross-session list = %q, %v", out, isError)
	}
	out, isError = callTool(t, executor, owner, "job_list", map[string]any{})
	if isError || !strings.HasPrefix(out, firstID+" [bash]") || !strings.Contains(out, "\n"+secondID+" [bash]") {
		t.Fatalf("owner list order = %q, %v", out, isError)
	}
	// Avoid leaving sleeping producers behind when the test ends.
	_, _ = callTool(t, executor, owner, "job_kill", map[string]any{"job_id": firstID})
	_, _ = callTool(t, executor, owner, "job_kill", map[string]any{"job_id": secondID})
}

func TestJobKillRequestsCancellation(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	executor.dshHome = t.TempDir()
	recorder := &eventRecorder{}
	inv := ToolInvocation{SessionID: "kill", CallID: "call-kill", RootCallID: "call-kill", Emit: recorder.emit}
	out, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "sleep", "command": "sleep 30", "run_in_background": true,
	})
	if isError {
		t.Fatalf("background bash failed: %s", out)
	}
	jobID := strings.TrimPrefix(out, "started background job ")
	out, isError = callTool(t, executor, inv, "job_kill", map[string]any{"job_id": jobID, "reason": "test"})
	if isError || out != "requested cancellation of job "+jobID {
		t.Fatalf("job_kill = %q, %v", out, isError)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ = callTool(t, executor, inv, "job_output", map[string]any{"job_id": jobID, "wait": true, "timeout_ms": 1000})
		if strings.Contains(out, "[status: killed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job was not killed: %q", out)
		}
	}
	var settled struct {
		Cause    string `json:"cause"`
		Detail   string `json:"detail"`
		ToolCall struct {
			CallID     string `json:"callId"`
			RootCallID string `json:"rootCallId"`
		} `json:"toolCall"`
	}
	events := recorder.byType(session.EventJobSettled)
	if len(events) != 1 {
		t.Fatalf("settled events = %d, want 1", len(events))
	}
	if err := json.Unmarshal(events[0].Data, &settled); err != nil {
		t.Fatal(err)
	}
	if settled.Cause != "kill" || !strings.Contains(settled.Detail, "test") ||
		settled.ToolCall.CallID != "call-kill" || settled.ToolCall.RootCallID != "call-kill" {
		t.Fatalf("job/settled = %+v", settled)
	}
	out, _ = callTool(t, executor, inv, "job_output", map[string]any{"job_id": "missing"})
	if out != "unknown job missing" {
		t.Fatalf("unknown job = %q", out)
	}
}

func TestStopBeforeSpawnCancelsWhenAttached(t *testing.T) {
	manager := newJobManager()
	job, err := manager.register(ToolInvocation{SessionID: "race"}, "bash", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if !job.stopByModel("cancelled before spawn") {
		t.Fatal("stopByModel returned false for a running job")
	}
	cancelled := make(chan struct{}, 1)
	job.attach(func() { cancelled <- struct{}{} })
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("attach did not deliver the pending cancellation")
	}
}

func TestReadImageStoresAttachmentAndReturnsBlocks(t *testing.T) {
	executor, root := newToolTestExecutor(t)
	executor.dshHome = t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(20 * x), G: uint8(40 * y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "pixel.png"), string(buf.Bytes()))
	out := executor.readImage(context.Background(), ToolInvocation{SessionID: "image"}, map[string]any{"file_path": "pixel.png"})
	if out.IsError {
		t.Fatalf("read_image failed: %s", out.Text)
	}
	if !strings.Contains(out.Text, "<type>image</type>") || !strings.Contains(out.Text, "image/png image, 3x2 px") {
		t.Fatalf("image envelope = %q", out.Text)
	}
	if len(out.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(out.Content))
	}
	block, ok := out.Content[1].(map[string]any)
	if !ok || block["type"] != "image" {
		t.Fatalf("image block = %#v", out.Content[1])
	}
	ref := block["attachment"].(map[string]any)
	id := ref["attachmentId"].(string)
	data, err := loadAttachment(executor.dshHome, id)
	if err != nil || len(data) == 0 {
		t.Fatalf("stored attachment = %d bytes, %v", len(data), err)
	}
	if got := base64.StdEncoding.EncodeToString(data); got == "" {
		t.Fatal("empty base64")
	}
}

func TestJobTeardownCancelsOwnedJobsWithoutNotice(t *testing.T) {
	executor, _ := newToolTestExecutor(t)
	recorder := &eventRecorder{}
	notices := make(chan string, 4)
	inv := ToolInvocation{
		SessionID: "teardown", Emit: recorder.emit,
		Notify: func(text string) { notices <- text },
	}
	out, isError := callTool(t, executor, inv, "bash", map[string]any{
		"description": "wait for teardown", "command": "sleep 30", "run_in_background": true,
	})
	if isError {
		t.Fatalf("background bash failed: %s", out)
	}
	jobID := strings.TrimPrefix(out, "started background job ")
	executor.closeJobs()

	settled := recorder.byType(session.EventJobSettled)
	if len(settled) != 1 {
		t.Fatalf("settled events = %d, want 1", len(settled))
	}
	var payload struct {
		JobID  string `json:"jobId"`
		Status string `json:"status"`
		Cause  string `json:"cause"`
	}
	if err := json.Unmarshal(settled[0].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.JobID != jobID || payload.Status != "killed" || payload.Cause != "teardown" {
		t.Fatalf("settled payload = %+v", payload)
	}
	select {
	case notice := <-notices:
		t.Fatalf("teardown must not notify: %q", notice)
	default:
	}
	listed, isError := callTool(t, executor, inv, "job_list", map[string]any{})
	if isError || !strings.Contains(listed, jobID+" [bash] killed") {
		t.Fatalf("job_list after teardown = %q, %v", listed, isError)
	}
}
