package api

import (
	"encoding/json"
	"testing"

	"github.com/xhd2015/deepseek-harness-go/session"
)

// TestSetThinkingCapAutoContinueWritesTheEvent is the RPC contract: the policy is
// durable and the reply says where it landed.
func TestSetThinkingCapAutoContinueWritesTheEvent(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	var value struct {
		Enabled bool  `json:"enabled"`
		Seq     int64 `json:"seq"`
	}
	decode(t, f.invoke(t, "session/setThinkingCapAutoContinue", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "enabled": true},
	}).mustSucceed(t), &value)
	if !value.Enabled {
		t.Fatal("enabled = false")
	}

	var found bool
	for _, event := range opened.Events() {
		if event.Type == session.ThinkingCapAutoContinueEvent {
			found = true
			if event.Seq != value.Seq {
				t.Fatalf("reply seq %d, event seq %d", value.Seq, event.Seq)
			}
			// The record must be one a build without this feature can skip.
			if !event.Ignorable {
				t.Fatal("the policy event is not marked ignorable")
			}
		}
	}
	if !found {
		t.Fatal("no policy event was written")
	}

	// It survives a reopen: the choice was made once.
	reopened, err := f.store.Open(opened.ID())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !session.FoldThinkingCapAutoContinue(reopened.Events()).Enabled {
		t.Fatal("the policy did not survive the reopen")
	}
}

// TestSetThinkingCapAutoContinueIsIdempotent avoids growing the log on a no-op.
func TestSetThinkingCapAutoContinueIsIdempotent(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "session/setThinkingCapAutoContinue", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "enabled": true},
	}).mustSucceed(t)
	before := len(opened.Events())
	value := f.invoke(t, "session/setThinkingCapAutoContinue", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "enabled": true},
	}).mustSucceed(t)
	if len(opened.Events()) != before {
		t.Fatalf("a repeated setting wrote %d events", len(opened.Events())-before)
	}
	var reply struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(value, &reply); err != nil || !reply.Enabled {
		t.Fatalf("reply = %s", value)
	}
}

// TestThinkingCapProjectsAsABoolean is what the switch in the page reads.
func TestThinkingCapProjectsAsABoolean(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	value := f.invoke(t, "session/projections", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	if !projectionBool(t, value, "thinkingCapAutoContinue", false) {
		t.Fatalf("a fresh session projects the policy on: %s", value)
	}

	f.invoke(t, "session/setThinkingCapAutoContinue", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "enabled": true},
	}).mustSucceed(t)
	value = f.invoke(t, "session/projections", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	if !projectionBool(t, value, "thinkingCapAutoContinue", true) {
		t.Fatalf("the policy did not project as true: %s", value)
	}
}

// projectionBool reads one boolean out of a projection reply, requiring the key.
func projectionBool(t *testing.T, value []byte, key string, want bool) bool {
	t.Helper()
	var reply struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(value, &reply); err != nil {
		t.Fatalf("decode projections: %v", err)
	}
	raw, ok := reply.Values[key]
	if !ok {
		t.Fatalf("projection %q is missing from %s", key, value)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("projection %q is not a boolean: %s", key, raw)
	}
	return got == want
}
