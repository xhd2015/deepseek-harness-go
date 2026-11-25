package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
)

// TestApprovalFrameCarriesEscalationFields pins the contract a page relies on to
// say what a widening request would actually grant.
//
// The frame already carried a sentence (`reason`). A sentence is for a reader: a
// consumer that wants to draw "workspace-write -> full access" and show the command
// would have to parse prose that is free to change. These three fields are the same
// facts without the parsing, and a consumer must be able to trust them.
func TestApprovalFrameCarriesEscalationFields(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	stream, err := f.service.Open(ctx, EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer stream.Close()
	// The first frame is the readiness announcement.
	if _, err := stream.Next(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}

	done := make(chan agent.ApprovalOutcome, 1)
	go func() {
		done <- f.runner.RequestEscalationForTest(opened, "bash", "call-9",
			"danger-full-access", "workspace-write", "rm -rf node_modules")
	}()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := stream.Next(ctx)
		if err != nil {
			break
		}
		var frame struct {
			Type    string `json:"type"`
			Event   string `json:"event"`
			EventID string `json:"eventId"`
			Request struct {
				ToolName         string `json:"toolName"`
				RequestedSandbox string `json:"requestedSandbox"`
				EffectiveSandbox string `json:"effectiveSandbox"`
				Command          string `json:"command"`
			} `json:"request"`
		}
		if json.Unmarshal(raw, &frame) != nil || frame.Type != "waterfall" || frame.Event != "approval/request" {
			continue
		}
		if frame.Request.ToolName != "bash" {
			t.Fatalf("toolName = %q", frame.Request.ToolName)
		}
		if frame.Request.RequestedSandbox != "danger-full-access" {
			t.Fatalf("requestedSandbox = %q", frame.Request.RequestedSandbox)
		}
		if frame.Request.EffectiveSandbox != "workspace-write" {
			t.Fatalf("effectiveSandbox = %q", frame.Request.EffectiveSandbox)
		}
		if frame.Request.Command != "rm -rf node_modules" {
			t.Fatalf("command = %q", frame.Request.Command)
		}
		return
	}
	t.Fatal("no approval/request frame arrived")
}
