package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/session"
)

// The comparison below is against a session Node DSH itself wrote, kept in the
// Harness checkout. Reading it rather than restating it means the test fails when
// DSH changes its record shape, which is the only time this backend's shape is
// wrong.
//
// The fixture is optional: a machine without a Harness checkout skips the
// comparison instead of failing, because the checkout is a development aid, not a
// build input. Set DSH_GO_DSH_ROOT to point at one explicitly.
const dshQuestionSnapshot = "snapshots/web/question-composer/session.v4.jsonl"

// dshSnapshotRoot finds a Harness checkout holding the fixture.
func dshSnapshotRoot() string {
	candidates := []string{os.Getenv("DSH_GO_DSH_ROOT")}
	if local, err := os.ReadFile("../.deepseek-harness.json"); err == nil {
		var cfg struct {
			Local string `json:"local"`
		}
		if json.Unmarshal(local, &cfg) == nil && cfg.Local != "" {
			candidates = append(candidates, filepath.Join("..", cfg.Local))
		}
	}
	candidates = append(candidates, "../deepseek-harness", "../../deepseek-harness")
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate, dshQuestionSnapshot)); err == nil {
			return candidate
		}
	}
	return ""
}

// dshQuestionRecord is the part of a DSH session row this test compares.
type dshQuestionRecord struct {
	Type string `json:"type"`
	Data struct {
		CallID    string `json:"callId"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		// Source is where a user/message names the call it replies to; a tool
		// result carries the same link under its message instead.
		Source struct {
			Kind   string `json:"kind"`
			CallID string `json:"callId"`
		} `json:"source"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Message struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			IsError    bool   `json:"isError"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Source struct {
				Kind   string `json:"kind"`
				CallID string `json:"callId"`
			} `json:"source"`
		} `json:"message"`
	} `json:"data"`
}

// readDSHQuestionRecords returns the call and result rows of the fixture session.
func readDSHQuestionRecords(t *testing.T, root string) (call, result dshQuestionRecord) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, dshQuestionSnapshot))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var sawCall, sawResult bool
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row dshQuestionRecord
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		switch row.Type {
		case session.EventToolCall:
			if row.Data.Name != "ask_user_question" || sawCall {
				continue
			}
			call, sawCall = row, true
		case session.EventToolResult:
			if !sawCall || sawResult {
				continue
			}
			result, sawResult = row, true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("%s has no ask_user_question call/result pair", dshQuestionSnapshot)
	}
	return call, result
}

// TestQuestionCallMatchesRealDSH compares this backend's record of an ask against
// one Node DSH wrote.
//
// What must line up is the call and the answer vocabulary: the event types, the
// name, the call identity the reply is keyed by, and the shape of each answer
// object. The envelope around the answers is deliberately not compared: this
// backend records the answer after the call returned, as the late reply DSH
// defines, while the fixture answered while the call was still open.
func TestQuestionCallMatchesRealDSH(t *testing.T) {
	root := dshSnapshotRoot()
	if root == "" {
		t.Skip("no DeepSeek Harness checkout with the question snapshot; set DSH_GO_DSH_ROOT")
	}
	dshCall, dshResult := readDSHQuestionRecords(t, root)

	// The fixture's arguments are the shape this backend must accept.
	var fixtureArgs struct {
		Questions []struct {
			ID          string `json:"id"`
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multi_select"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(dshCall.Data.Arguments), &fixtureArgs); err != nil {
		t.Fatalf("decode DSH arguments: %v", err)
	}
	if len(fixtureArgs.Questions) == 0 {
		t.Fatal("the DSH fixture has no questions")
	}
	// Replaying the fixture's own arguments is the strictest form of "the schemas
	// agree": if a field name differed, the question would arrive without it.
	f := newAskFixture(t, dshCall.Data.Arguments)
	opened := f.createSession(t)
	promptTurn(t, f, opened, "ask me")
	waitForTurnEnd(t, opened, 5*time.Second)
	waitForPendingQuestion(t, opened.ID())

	// The call this backend wrote must carry the same shape as DSH's.
	call := findEvent(t, opened, session.EventToolCall)
	var wroteCall dshQuestionRecord
	if err := json.Unmarshal([]byte(callRawJSON(t, opened, call)), &wroteCall); err != nil {
		t.Fatalf("decode own call: %v", err)
	}
	if wroteCall.Type != dshCall.Type {
		t.Fatalf("call type = %q, want %q", wroteCall.Type, dshCall.Type)
	}
	if wroteCall.Data.Name != dshCall.Data.Name {
		t.Fatalf("tool name = %q, want %q", wroteCall.Data.Name, dshCall.Data.Name)
	}
	if wroteCall.Data.Arguments != dshCall.Data.Arguments {
		t.Fatalf("arguments were rewritten\n got: %s\nwant: %s", wroteCall.Data.Arguments, dshCall.Data.Arguments)
	}

	// Answer with the same batch the DSH session recorded.
	var answered struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
			Custom   string   `json:"custom"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(dshResult.Data.Message.Content[0].Text), &answered); err != nil {
		t.Fatalf("decode DSH answers: %v", err)
	}
	if len(answered.Answers) == 0 {
		t.Fatal("the DSH fixture recorded no answers")
	}
	if answered.Answers[0].ID != fixtureArgs.Questions[0].ID {
		t.Fatalf("DSH answer id %q does not match its question %q",
			answered.Answers[0].ID, fixtureArgs.Questions[0].ID)
	}
	request := map[string]any{"answers": []map[string]any{}}
	for _, answer := range answered.Answers {
		request["answers"] = append(request["answers"].([]map[string]any), map[string]any{
			"id": answer.ID, "selected": answer.Selected, "custom": answer.Custom,
		})
	}
	f.invoke(t, "userQuestions/answer", map[string]any{
		"sessionId": opened.ID(),
		"callId":    strings.SplitN(waitForPendingQuestion(t, opened.ID()), "\x00", 2)[1],
		"answer":    request,
	}).mustSucceed(t)

	// The reply must be the event DSH uses for a late answer, and its answers must
	// carry the same keys as the batch DSH recorded.
	reply := waitForQuestionReply(t, opened, 5*time.Second)
	var wroteReply dshQuestionRecord
	if err := json.Unmarshal([]byte(callRawJSON(t, opened, *reply)), &wroteReply); err != nil {
		t.Fatalf("decode own reply: %v", err)
	}
	if wroteReply.Type != session.EventUserMessage {
		t.Fatalf("reply type = %q, want %q", wroteReply.Type, session.EventUserMessage)
	}
	if wroteReply.Data.Source.Kind != "user-question-reply" {
		t.Fatalf("reply source kind = %q", wroteReply.Data.Source.Kind)
	}
	var ownBatch, dshBatch map[string]any
	if err := json.Unmarshal([]byte(wroteReply.Data.Content[0].Text), &ownBatch); err != nil {
		t.Fatalf("decode own batch: %v", err)
	}
	if err := json.Unmarshal([]byte(dshResult.Data.Message.Content[0].Text), &dshBatch); err != nil {
		t.Fatalf("decode DSH batch: %v", err)
	}
	ownAnswers := ownBatch["answers"].([]any)
	if len(ownAnswers) != len(dshBatch["answers"].([]any)) {
		t.Fatalf("answer count = %d, want %d", len(ownAnswers), len(dshBatch["answers"].([]any)))
	}
	for _, answer := range ownAnswers {
		got := sortedKeys(answer.(map[string]any))
		want := sortedKeys(dshBatch["answers"].([]any)[0].(map[string]any))
		if got != want {
			t.Fatalf("answer keys = %s, want %s", got, want)
		}
	}
}

// callRawJSON renders one event as the wire row a reader sees.
func callRawJSON(t *testing.T, _ *session.Session, event session.Event) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"type": event.Type, "data": json.RawMessage(event.Data)})
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return string(payload)
}

// sortedKeys renders an object's key set for comparison.
func sortedKeys(value map[string]any) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ",")
}

// TestAskSchemaMatchesTheWebSnapshot is the strictest form of "same tool": the
// whole schema object must equal the one the web preset hands the model.
//
// Field-level checking is not enough here. The corrected contract is the absence of
// a wait parameter as much as the presence of the question fields, and a schema
// that quietly grows a `timeout` changes when the model thinks it will get an
// answer.
func TestAskSchemaMatchesTheWebSnapshot(t *testing.T) {
	root := dshSnapshotRoot()
	if root == "" {
		t.Skip("no DeepSeek Harness checkout with tool schema snapshots; set DSH_GO_DSH_ROOT")
	}
	body, err := os.ReadFile(filepath.Join(root, "snapshots/web/creator-persona/tool-schemas.expected.json"))
	if err != nil {
		t.Skipf("no tool schema fixture: %v", err)
	}
	var fixture struct {
		Initial []json.RawMessage `json:"initial"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var want map[string]any
	for _, raw := range fixture.Initial {
		var schema map[string]any
		if json.Unmarshal(raw, &schema) != nil {
			continue
		}
		if schema["name"] == "ask_user_question" {
			want = schema
			break
		}
	}
	if want == nil {
		t.Skip("the fixture has no ask_user_question schema")
	}

	executor := agent.NewToolExecutor(t.TempDir())
	var got map[string]any
	for _, def := range executor.Definitions() {
		if def.Name != "ask_user_question" {
			continue
		}
		encoded, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("encode schema: %v", err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
	}
	if got == nil {
		t.Fatal("ask_user_question is not offered to the model")
	}

	// The wait parameter must not exist, in either shape.
	if props, ok := got["parameters"].(map[string]any)["properties"].(map[string]any); ok {
		if _, found := props["timeout"]; found {
			t.Fatal("the schema declares a timeout; the web preset has none")
		}
	}

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode own schema: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode fixture schema: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("schema differs from the web snapshot\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}
