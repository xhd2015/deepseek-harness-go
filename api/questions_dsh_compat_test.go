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
		Message   struct {
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

// TestQuestionRecordMatchesRealDSH compares this backend's record of an ask round
// against one Node DSH wrote, field by field.
//
// Three things must line up or the two backends cannot read each other's logs:
// the event type names, the call identity the result is keyed by, and the answer
// batch inside the result text — including the option structure, since the UI
// renders the card from the call's arguments and the reply from the batch.
func TestQuestionRecordMatchesRealDSH(t *testing.T) {
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
	// agree": if a field name differed, the ask would arrive without it.
	f := newAskFixture(t, dshCall.Data.Arguments)
	opened := f.createSession(t)
	promptTurn(t, f, opened, "ask me")
	pending := waitForPendingQuestion(t)

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

	// Answer with a selection and custom text, exactly as the DSH session did.
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
	// The id in the batch must be the id from the question, or the browser and the
	// model would disagree about which answer belongs where.
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
		"callId":    strings.SplitN(pending, "\x00", 2)[1],
		"answer":    request,
	}).mustSucceed(t)
	waitForTurnEnd(t, opened, 10*time.Second)

	// The result must be the same event type, keyed the same way, with the same
	// text payload.
	result := findEvent(t, opened, session.EventToolResult)
	var wroteResult dshQuestionRecord
	if err := json.Unmarshal([]byte(callRawJSON(t, opened, result)), &wroteResult); err != nil {
		t.Fatalf("decode own result: %v", err)
	}
	if wroteResult.Type != dshResult.Type {
		t.Fatalf("result type = %q, want %q", wroteResult.Type, dshResult.Type)
	}
	if wroteResult.Data.Message.Source.Kind != dshResult.Data.Message.Source.Kind {
		t.Fatalf("result source kind = %q, want %q",
			wroteResult.Data.Message.Source.Kind, dshResult.Data.Message.Source.Kind)
	}
	if len(wroteResult.Data.Message.Content) != 1 || wroteResult.Data.Message.Content[0].Type != "text" {
		t.Fatalf("result content = %+v", wroteResult.Data.Message.Content)
	}
	// The batch must decode the same way on both sides: same keys, same shape.
	var ownBatch, dshBatch map[string]any
	if err := json.Unmarshal([]byte(wroteResult.Data.Message.Content[0].Text), &ownBatch); err != nil {
		t.Fatalf("decode own batch: %v", err)
	}
	if err := json.Unmarshal([]byte(dshResult.Data.Message.Content[0].Text), &dshBatch); err != nil {
		t.Fatalf("decode DSH batch: %v", err)
	}
	if got, want := sortedKeys(ownBatch), sortedKeys(dshBatch); got != want {
		t.Fatalf("batch keys = %s, want %s", got, want)
	}
	ownAnswers := ownBatch["answers"].([]any)
	if len(ownAnswers) != len(dshBatch["answers"].([]any)) {
		t.Fatalf("answer count = %d, want %d", len(ownAnswers), len(dshBatch["answers"].([]any)))
	}
	for _, answer := range ownAnswers {
		if got, want := sortedKeys(answer.(map[string]any)), sortedKeys(dshBatch["answers"].([]any)[0].(map[string]any)); got != want {
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

// TestRequestHeaderToolSchemasMatchDSH keeps the logged schemas readable by DSH.
//
// The session page decides which calls it can answer by looking for a `timeout`
// parameter in the header's tool schemas. A schema whose fields serialize as
// "Name"/"Parameters" is invisible to that lookup, so the question renders but can
// never be submitted — which is exactly how a browser-only gap survives a
// backend-only test. The comparison is against a real DSH schema list.
func TestRequestHeaderToolSchemasMatchDSH(t *testing.T) {
	root := dshSnapshotRoot()
	if root == "" {
		t.Skip("no DeepSeek Harness checkout with tool schema snapshots; set DSH_GO_DSH_ROOT")
	}
	path := filepath.Join(root, "snapshots/web/creator-persona/tool-schemas.expected.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no tool schema fixture: %v", err)
	}
	var fixture struct {
		Initial []map[string]any `json:"initial"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var dshAsk map[string]any
	for _, schema := range fixture.Initial {
		if schema["name"] == "ask_user_question" {
			dshAsk = schema
			break
		}
	}
	if dshAsk == nil {
		t.Skip("the fixture has no ask_user_question schema")
	}
	dshKeys := sortedKeys(dshAsk)

	// This backend's own logged schema, encoded the way the session log encodes it.
	executor := agent.NewToolExecutor(t.TempDir())
	var own map[string]any
	for _, def := range executor.Definitions() {
		if def.Name != "ask_user_question" {
			continue
		}
		encoded, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("encode schema: %v", err)
		}
		if err := json.Unmarshal(encoded, &own); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
	}
	if own == nil {
		t.Fatal("ask_user_question is not offered")
	}
	if got := sortedKeys(own); got != dshKeys {
		t.Fatalf("schema keys = %s, want %s", got, dshKeys)
	}
	// The parameter names under `parameters.properties` must be DSH's too, and this
	// backend's timed form must additionally declare the wait window.
	ownProps := sortedKeys(own["parameters"].(map[string]any)["properties"].(map[string]any))
	dshProps := sortedKeys(dshAsk["parameters"].(map[string]any)["properties"].(map[string]any))
	if dshProps != "questions" {
		t.Fatalf("the fixture's parameters changed: %s", dshProps)
	}
	if !strings.Contains(ownProps, "questions") {
		t.Fatalf("parameters = %s, want questions among them", ownProps)
	}
	// The window is what lets a page answer the question at all.
	if !strings.Contains(ownProps, "timeout") {
		t.Fatalf("parameters = %s, want the wait window", ownProps)
	}
}
