package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/mux"
	"github.com/xhd2015/deepseek-harness-go/session"
)

type fakeModel struct {
	answer string
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	answer, _ := json.Marshal(f.answer)
	_, _ = w.Write([]byte(`data: {"model":"fake-model","choices":[{"delta":{"content":` + string(answer) + `}}]}` + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

type fixture struct {
	service *Service
	store   *session.Store
	runner  *agent.Runner
	root    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	model := httptest.NewServer(&fakeModel{answer: "hello from the fake model"})
	t.Cleanup(model.Close)
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runner := agent.New(agent.Config{
		Client:   llm.New(llm.Config{BaseURL: model.URL, Model: "fake-model", APIKey: "k"}),
		Provider: "fake-provider",
		Model:    "fake-model",
	})
	t.Cleanup(runner.Close)
	cwd := filepath.Join(root, "project")
	service := New(Config{
		Store:       store,
		Runner:      runner,
		DefaultCwd:  cwd,
		AgentPreset: "std-no-subagent",
		HomeDir:     root,
	})
	return &fixture{service: service, store: store, runner: runner, root: root}
}

func (f *fixture) createSession(t *testing.T) *session.Session {
	t.Helper()
	opened, err := f.store.Create(filepath.Join(f.root, "project"), "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

// invoke drives one unary call the way the browser does: a `$invoke` stream
// whose inner payload wraps the method args in `{"args": ...}`.
func (f *fixture) invoke(t *testing.T, endpoint string, args any) result {
	t.Helper()
	ap, ok := args.(actionPayload)
	if !ok {
		ap = actionPayload{Args: args}
	}
	inner, err := json.Marshal(ap.Args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"args": map[string]any{
			"endpoint": endpoint,
			"payload":  map[string]any{"args": json.RawMessage(inner)},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	stream, err := f.service.Open(context.Background(), EndpointInvoke, payload, nil)
	if err != nil {
		var failure *mux.Failure
		if !asFailure(err, &failure) {
			t.Fatalf("%s: %v", endpoint, err)
		}
		return result{Error: failure}
	}
	values := drain(t, stream)
	if len(values) != 1 {
		t.Fatalf("%s emitted %d values, want 1: %s", endpoint, len(values), values)
	}
	var decoded result
	if err := json.Unmarshal(values[0], &decoded); err != nil {
		t.Fatalf("%s: decode %s: %v", endpoint, values[0], err)
	}
	return decoded
}

type actionPayload struct{ Args any }

type result struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error *mux.Failure    `json:"error"`
}

func (r result) mustSucceed(t *testing.T) json.RawMessage {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("unexpected failure: %s: %s", r.Error.Code, r.Error.Message)
	}
	if !r.OK {
		t.Fatalf("response is not ok: %+v", r)
	}
	return r.Value
}

func (r result) mustFail(t *testing.T, code string) *mux.Failure {
	t.Helper()
	if r.Error == nil {
		t.Fatalf("expected failure %q, got success: %s", code, r.Value)
	}
	if r.Error.Code != code {
		t.Fatalf("code = %q, want %q (%s)", r.Error.Code, code, r.Error.Message)
	}
	return r.Error
}

func TestSessionListAndSummary(t *testing.T) {
	f := newFixture(t)
	empty := f.invoke(t, "session/list", map[string]any{"_request": map[string]any{}}).mustSucceed(t)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	decode(t, empty, &list)
	if list.Items == nil || len(list.Items) != 0 {
		t.Fatalf("items = %v", list.Items)
	}

	opened := f.createSession(t)
	if _, err := opened.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := opened.Append(session.EventSessionTitle, map[string]any{"title": "title"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	value := f.invoke(t, "session/list", map[string]any{"_request": map[string]any{}}).mustSucceed(t)
	decode(t, value, &list)
	if len(list.Items) != 1 {
		t.Fatalf("items = %v", list.Items)
	}
	item := list.Items[0]
	if item["sessionId"] != opened.ID() {
		t.Fatalf("sessionId = %v", item["sessionId"])
	}
	if item["agentAvailable"] != true {
		t.Fatalf("agentAvailable = %v", item["agentAvailable"])
	}
	if item["blank"] != false || item["running"] != false {
		t.Fatalf("blank/running = %v/%v", item["blank"], item["running"])
	}
	if item["updatedAt"] == nil {
		t.Fatalf("updatedAt is missing: %v", item)
	}
}

func TestSessionCreateUsesWorkspaceOrCwd(t *testing.T) {
	f := newFixture(t)
	workspaces := f.service.Workspaces.List()
	if len(workspaces) != 1 {
		t.Fatalf("default workspaces = %d", len(workspaces))
	}

	created := f.invoke(t, "session/create", map[string]any{
		"request": map[string]any{"workspaceId": workspaces[0].ID},
	}).mustSucceed(t)
	var value struct {
		SessionID   string `json:"sessionId"`
		AgentPreset string `json:"agentPreset"`
	}
	decode(t, created, &value)
	if value.SessionID == "" || value.AgentPreset != "std-no-subagent" {
		t.Fatalf("create = %+v", value)
	}
	opened, err := f.store.Open(value.SessionID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.Header().Cwd != workspaces[0].Path {
		t.Fatalf("cwd = %q", opened.Header().Cwd)
	}

	// The client demands a workspace it can show for the new session, so the
	// create call must have updated the membership list.
	after := f.service.workspaceViews()
	if len(after) != 1 {
		t.Fatalf("workspaces = %v", after)
	}
	sessionIDs, ok := after[0]["sessionIds"].([]string)
	if !ok || len(sessionIDs) != 1 || sessionIDs[0] != value.SessionID {
		t.Fatalf("sessionIds = %v", after[0]["sessionIds"])
	}

	// An explicit cwd is accepted when it is not also a workspaceId.
	second := f.invoke(t, "session/create", map[string]any{
		"request": map[string]any{"cwd": filepath.Join(f.root, "other")},
	}).mustSucceed(t)
	var other struct {
		SessionID string `json:"sessionId"`
	}
	decode(t, second, &other)
	openedOther, err := f.store.Open(other.SessionID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if openedOther.Header().Cwd != filepath.Join(f.root, "other") {
		t.Fatalf("cwd = %q", openedOther.Header().Cwd)
	}

	f.invoke(t, "session/create", map[string]any{
		"request": map[string]any{"workspaceId": workspaces[0].ID, "cwd": "/tmp"},
	}).mustFail(t, CodeBadRequest)
	f.invoke(t, "session/create", map[string]any{
		"request": map[string]any{"workspaceId": "ws-missing"},
	}).mustFail(t, CodeWorkspaceNotFound)
}

func TestSessionCreateWithIDRetriesBlankSession(t *testing.T) {
	f := newFixture(t)
	request := map[string]any{
		"cwd":       filepath.Join(f.root, "retry-project"),
		"sessionId": "api-retry-id",
	}
	var first, second struct {
		SessionID string `json:"sessionId"`
	}
	decode(t, f.invoke(t, "session/create", map[string]any{"request": request}).mustSucceed(t), &first)
	decode(t, f.invoke(t, "session/create", map[string]any{"request": request}).mustSucceed(t), &second)
	if first.SessionID != "api-retry-id" || second.SessionID != first.SessionID {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
	entries, err := f.store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Header.ID != "api-retry-id" {
		t.Fatalf("sessions = %+v", entries)
	}
	if entries[0].LastSeq >= 0 {
		t.Fatalf("retried session has events: %+v", entries[0])
	}
}

func TestUnaryFailuresAreSingleItems(t *testing.T) {
	f := newFixture(t)
	failure := f.invoke(t, "session/page", map[string]any{
		"request": map[string]any{"address": map[string]any{"kind": "session", "sessionId": "missing"}},
	}).mustFail(t, CodeNotFound)
	if failure.Details["sessionId"] != "missing" {
		t.Fatalf("details = %v", failure.Details)
	}
	if failure.Message == "" {
		t.Fatalf("message is empty: %+v", failure)
	}
}

func TestUnimplementedAndMalformedRequests(t *testing.T) {
	f := newFixture(t)
	f.invoke(t, "session/fork", map[string]any{"request": map[string]any{}}).mustFail(t, CodeUnimplemented)

	// The typed client always nests args under the single parameter name.
	stream, err := f.service.Open(context.Background(), EndpointInvoke, []byte(`{"args":{"endpoint":"session/page","payload":{"args":{"address":{"sessionId":"x"}}}}}`), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	decoded := decodeItem(t, drain(t, stream)[0])
	if decoded.Error == nil || decoded.Error.Code != CodeBadRequest {
		t.Fatalf("response = %+v", decoded)
	}

	if _, err := f.service.Open(context.Background(), EndpointInvoke, []byte(`{"args":{}}`), nil); err == nil {
		t.Fatal("expected a failure for an invoke without endpoint")
	}
	if _, err := f.service.Open(context.Background(), "session/nope", []byte(`{"args":{}}`), nil); err == nil {
		t.Fatal("expected a failure for an unknown endpoint")
	}
}

func TestFollowServesSnapshotThenLiveEvents(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	if _, err := opened.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	stream, err := f.service.Open(context.Background(), "session/follow", []byte(
		`{"args":{"request":{"address":{"kind":"session","sessionId":"`+opened.ID()+`"},"maxMessages":500,"turnWindow":{"minMessages":50,"minTurns":2},"assistantStream":true}}}`,
	), nil)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer stream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snapshot struct {
		Type    string          `json:"type"`
		Header  json.RawMessage `json:"header"`
		Cursor  int64           `json:"cursor"`
		Records []struct {
			Type  string        `json:"type"`
			Event session.Event `json:"event"`
		} `json:"records"`
		HasMore         bool                        `json:"hasMore"`
		Projections     *session.ProjectionBaseline `json:"projections"`
		AssistantStream map[string]any              `json:"assistantStream"`
	}
	decode(t, first, &snapshot)
	if snapshot.Type != "snapshot" {
		t.Fatalf("type = %q", snapshot.Type)
	}
	if snapshot.Cursor != 0 || len(snapshot.Records) != 1 {
		t.Fatalf("cursor/records = %d/%d", snapshot.Cursor, len(snapshot.Records))
	}
	if snapshot.Records[0].Type != "event" || snapshot.Records[0].Event.Type != session.EventTurnStart {
		t.Fatalf("record = %+v", snapshot.Records[0])
	}
	if snapshot.AssistantStream == nil || snapshot.AssistantStream["revision"] != float64(0) {
		t.Fatalf("assistantStream = %v", snapshot.AssistantStream)
	}
	if snapshot.Projections == nil || snapshot.Projections.AsOfSeq != 0 {
		t.Fatalf("projections = %+v", snapshot.Projections)
	}
	var header map[string]any
	if err := json.Unmarshal(snapshot.Header, &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	if header["id"] != opened.ID() || header["version"] != float64(session.FormatVersion) {
		t.Fatalf("header = %v", header)
	}
	if _, ok := header["type"]; ok {
		t.Fatalf("wire header must not carry a type field: %v", header)
	}

	if _, err := opened.Append(session.EventStepStart, map[string]any{"turn": 1, "step": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	second, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("live event: %v", err)
	}
	var record struct {
		Type  string        `json:"type"`
		Event session.Event `json:"event"`
	}
	decode(t, second, &record)
	if record.Type != "event" || record.Event.Type != session.EventStepStart || record.Event.Seq != 1 {
		t.Fatalf("live record = %+v", record)
	}
}

func TestFollowSnapshotCarriesActiveAssistantStream(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	if _, err := opened.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	attempt := opened.BeginAssistantAttempt(1, 1)
	if err := attempt.Push(map[string]any{"type": "reasoning-delta", "index": 0, "text": "thinking"}); err != nil {
		t.Fatalf("Push reasoning: %v", err)
	}
	if err := attempt.Push(map[string]any{"type": "text-delta", "index": 1, "text": "Reading"}); err != nil {
		t.Fatalf("Push text: %v", err)
	}

	stream, err := f.service.Open(context.Background(), "session/follow", []byte(
		`{"args":{"request":{"address":{"kind":"session","sessionId":"`+opened.ID()+`"},"maxMessages":500,"turnWindow":{"minMessages":50,"minTurns":2},"assistantStream":true}}}`,
	), nil)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer stream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snapshot struct {
		Type            string `json:"type"`
		AssistantStream struct {
			Revision      int64 `json:"revision"`
			ActiveAttempt *struct {
				AttemptID       string            `json:"attemptId"`
				StartedAfterSeq int64             `json:"startedAfterSeq"`
				Turn            int               `json:"turn"`
				Step            int               `json:"step"`
				NextIndex       int               `json:"nextIndex"`
				Stream          []json.RawMessage `json:"stream"`
			} `json:"activeAttempt"`
		} `json:"assistantStream"`
	}
	decode(t, first, &snapshot)
	if snapshot.Type != "snapshot" {
		t.Fatalf("type = %q", snapshot.Type)
	}
	active := snapshot.AssistantStream.ActiveAttempt
	if active == nil {
		t.Fatalf("assistantStream missing activeAttempt: %s", first)
	}
	// start, two chunks -> revision 3; both chunks are retained for a client
	// that reconnects mid-attempt.
	if snapshot.AssistantStream.Revision != 3 {
		t.Fatalf("revision = %d, want 3", snapshot.AssistantStream.Revision)
	}
	if active.AttemptID != opened.ID()+":1" || active.Turn != 1 || active.Step != 1 {
		t.Fatalf("activeAttempt = %+v", active)
	}
	if active.NextIndex != 2 || len(active.Stream) != 2 {
		t.Fatalf("nextIndex/stream = %d/%d, want 2/2", active.NextIndex, len(active.Stream))
	}
	for i, raw := range active.Stream {
		var record struct {
			Type  string `json:"type"`
			Time  int64  `json:"time"`
			Chunk struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"chunk"`
		}
		decode(t, raw, &record)
		if record.Type != "chunk" || record.Time == 0 || record.Chunk.Text == "" {
			t.Fatalf("stream[%d] = %s", i, raw)
		}
	}
}

func TestFollowRejectsUnknownSessionAndFlatArgs(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Open(context.Background(), "session/follow", []byte(`{"args":{"address":{"sessionId":"missing"}}}`), nil); err == nil {
		t.Fatal("expected a failure for a flat (unwrapped) request")
	}
	_, err := f.service.Open(context.Background(), "session/follow", []byte(`{"args":{"request":{"address":{"kind":"session","sessionId":"missing"}}}}`), nil)
	var failure *mux.Failure
	if !asFailure(err, &failure) || failure.Code != CodeNotFound {
		t.Fatalf("error = %v", err)
	}
}

func TestSessionPageAndProjections(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	seedTurn(t, opened, 1)
	seedTurn(t, opened, 2)

	value := f.invoke(t, "session/page", map[string]any{
		"request": map[string]any{
			"address":     map[string]any{"kind": "session", "sessionId": opened.ID()},
			"maxMessages": 1,
		},
	}).mustSucceed(t)
	var page struct {
		Records []struct {
			Event session.Event `json:"event"`
		} `json:"records"`
		HasMore bool `json:"hasMore"`
	}
	decode(t, value, &page)
	if len(page.Records) == 0 || !page.HasMore {
		t.Fatalf("page = %+v", page)
	}
	if page.Records[0].Event.Seq != 6 {
		t.Fatalf("page starts at seq %d, want the second turn's boundary", page.Records[0].Event.Seq)
	}

	projections := f.invoke(t, "session/projections", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	var baseline struct {
		Kind    string         `json:"kind"`
		AsOfSeq int64          `json:"asOfSeq"`
		Values  map[string]any `json:"values"`
	}
	decode(t, projections, &baseline)
	if baseline.Kind != "sequenced" || baseline.AsOfSeq != 11 {
		t.Fatalf("projections = %+v", baseline)
	}
	if _, ok := baseline.Values["sessionListMetadata"]; !ok {
		t.Fatalf("projection values = %v", baseline.Values)
	}
}

func TestPromptCancelRename(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"requestId": "rpc-1",
			"sessionId": opened.ID(),
			"mode":      "queue",
			"content":   []map[string]any{{"type": "text", "text": "hi"}},
		},
	}).mustSucceed(t)
	deadline := time.Now().Add(3 * time.Second)
	for f.runner.Busy(opened.ID()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.runner.Busy(opened.ID()) {
		t.Fatal("turn did not finish")
	}

	events := opened.Events()
	var last session.Event = events[len(events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %s", last.Type)
	}

	renamed := f.invoke(t, "session/rename", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "title": "Renamed"},
	}).mustSucceed(t)
	var renameValue struct {
		Title string `json:"title"`
		Seq   int64  `json:"seq"`
	}
	decode(t, renamed, &renameValue)
	if renameValue.Title != "Renamed" || renameValue.Seq != last.Seq+1 {
		t.Fatalf("rename = %+v", renameValue)
	}

	cancelled := f.invoke(t, "session/cancel", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	var cancelValue struct {
		Accepted bool `json:"accepted"`
	}
	decode(t, cancelled, &cancelValue)
	if !cancelValue.Accepted {
		t.Fatalf("cancel = %+v", cancelValue)
	}
}

func TestPromptRejectsUnsupportedContent(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"requestId": "rpc-2",
			"sessionId": opened.ID(),
			"mode":      "queue",
			"content":   []map[string]any{{"type": "image", "data": "…"}},
		},
	}).mustFail(t, CodeUnimplemented)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{"sessionId": "missing", "content": []map[string]any{{"type": "text", "text": "hi"}}},
	}).mustFail(t, CodeNotFound)
}

func TestDraftRoundTrip(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	get := func() string {
		value := f.invoke(t, "session/getDraft", map[string]any{
			"request": map[string]any{"sessionId": opened.ID()},
		}).mustSucceed(t)
		var draft struct {
			Text string `json:"text"`
		}
		decode(t, value, &draft)
		return draft.Text
	}
	if got := get(); got != "" {
		t.Fatalf("initial draft = %q", got)
	}
	f.invoke(t, "session/setDraft", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "text": "half typed"},
	}).mustSucceed(t)
	if got := get(); got != "half typed" {
		t.Fatalf("draft = %q", got)
	}
	f.invoke(t, "session/setDraft", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "text": ""},
	}).mustSucceed(t)
	if got := get(); got != "" {
		t.Fatalf("draft after clear = %q", got)
	}

	f.invoke(t, "session/getDraft", map[string]any{
		"request": map[string]any{"sessionId": "missing"},
	}).mustFail(t, CodeNotFound)
}

func TestModelCatalogAndSelection(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)

	value := f.invoke(t, "session/modelCatalog", map[string]any{}).mustSucceed(t)
	var catalog struct {
		Default struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"default"`
		RoutableProviders []string `json:"routableProviders"`
		Groups            []struct {
			ID     string `json:"id"`
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"groups"`
		Failures []any `json:"failures"`
	}
	decode(t, value, &catalog)
	if catalog.Default.Provider != "fake-provider" || catalog.Default.Model != "fake-model" {
		t.Fatalf("default = %+v", catalog.Default)
	}
	if len(catalog.RoutableProviders) != 1 || catalog.RoutableProviders[0] != "fake-provider" {
		t.Fatalf("providers = %v", catalog.RoutableProviders)
	}
	if len(catalog.Groups) != 1 || catalog.Groups[0].Models[0].ID != "fake-model" {
		t.Fatalf("groups = %+v", catalog.Groups)
	}
	if catalog.Failures == nil {
		t.Fatal("failures must be an empty array, not null")
	}

	f.invoke(t, "session/selectModel", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "provider": "other", "model": "fake-model"},
	}).mustFail(t, CodeBadRequest)
	f.invoke(t, "session/selectModel", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "provider": "fake-provider", "model": "other"},
	}).mustFail(t, CodeBadRequest)

	selected := f.invoke(t, "session/selectModel", map[string]any{
		"request": map[string]any{"sessionId": opened.ID(), "provider": "fake-provider", "model": "fake-model", "reasoningEffort": "high"},
	}).mustSucceed(t)
	var selection struct {
		Selected struct {
			Provider        string `json:"provider"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"selected"`
	}
	decode(t, selected, &selection)
	if selection.Selected.ReasoningEffort != "high" {
		t.Fatalf("selected = %+v", selection.Selected)
	}

	projections := f.invoke(t, "session/projections", map[string]any{
		"request": map[string]any{"sessionId": opened.ID()},
	}).mustSucceed(t)
	var baseline struct {
		Values map[string]struct {
			Next map[string]any `json:"next"`
		} `json:"values"`
	}
	decode(t, projections, &baseline)
	if baseline.Values["modelSelection"].Next["reasoningEffort"] != "high" {
		t.Fatalf("modelSelection = %+v", baseline.Values["modelSelection"])
	}
}

func TestWorkspaceFollowBaselineAndReplace(t *testing.T) {
	f := newFixture(t)
	stream, err := f.service.Open(context.Background(), "workspace/follow", []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("workspace/follow: %v", err)
	}
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	var frame struct {
		Type  string `json:"type"`
		Value struct {
			Items              []map[string]any `json:"items"`
			ArchivedSessionIDs []string         `json:"archivedSessionIds"`
			PinnedSessionIDs   []string         `json:"pinnedSessionIds"`
		} `json:"value"`
	}
	decode(t, first, &frame)
	if frame.Type != "baseline" || len(frame.Value.Items) != 1 {
		t.Fatalf("baseline = %+v", frame)
	}
	if frame.Value.ArchivedSessionIDs == nil || frame.Value.PinnedSessionIDs == nil {
		t.Fatalf("baseline arrays = %+v", frame.Value)
	}
	defaultPath := f.service.Workspaces.List()[0].Path
	if frame.Value.Items[0]["path"] != defaultPath {
		t.Fatalf("default workspace = %+v", frame.Value.Items[0])
	}

	opened, err := f.store.Create(filepath.Join(f.root, "second"), "std-no-subagent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	f.service.Workspaces.Add(filepath.Join(f.root, "second"))

	next, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	decode(t, next, &frame)
	if frame.Type != "replace" || len(frame.Value.Items) != 2 {
		t.Fatalf("replace = %+v", frame)
	}
	second := frame.Value.Items[1]
	if second["title"] != "second" {
		t.Fatalf("second workspace = %+v", second)
	}
	ids, _ := second["sessionIds"].([]any)
	if len(ids) != 1 || ids[0] != opened.ID() {
		t.Fatalf("second sessionIds = %v", second["sessionIds"])
	}
}

func TestWorkspaceCreateAndInitialize(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.root, "created")
	value := f.invoke(t, "workspace/create", map[string]any{
		"request": map[string]any{"path": path},
	}).mustSucceed(t)
	var created struct {
		Workspace map[string]any `json:"workspace"`
		Created   bool           `json:"created"`
	}
	decode(t, value, &created)
	if !created.Created || created.Workspace["path"] != path {
		t.Fatalf("create = %+v", created)
	}

	again := f.invoke(t, "workspace/create", map[string]any{
		"request": map[string]any{"path": path},
	}).mustSucceed(t)
	decode(t, again, &created)
	if created.Created {
		t.Fatal("create reported a new workspace for a known path")
	}
	f.invoke(t, "workspace/create", map[string]any{
		"request": map[string]any{"path": "relative/path"},
	}).mustFail(t, CodeWorkspaceInvalidPath)

	initialized := f.invoke(t, "workspace/initializeDefault", map[string]any{}).mustSucceed(t)
	var initValue struct {
		Workspace map[string]any `json:"workspace"`
	}
	decode(t, initialized, &initValue)
	if initValue.Workspace["path"] != f.service.Workspaces.List()[0].Path {
		t.Fatalf("initializeDefault = %+v", initValue)
	}
}

func TestEventsEndpointAnnouncesReadyThenWaits(t *testing.T) {
	f := newFixture(t)
	stream, err := f.service.Open(context.Background(), EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("$events: %v", err)
	}
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	frame, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	var ready struct {
		Type     string `json:"type"`
		ClientID string `json:"clientId"`
		Host     struct {
			Home string `json:"home"`
		} `json:"host"`
	}
	decode(t, frame, &ready)
	if ready.Type != "ready" || ready.ClientID == "" || ready.Host.Home != f.root {
		t.Fatalf("ready frame = %+v", ready)
	}

	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if _, err := stream.Next(short); err == nil {
		t.Fatal("$events stream must stay idle instead of ending")
	}
}

func TestEventsEndpointForwardsSessionStatus(t *testing.T) {
	f := newFixture(t)
	stream, err := f.service.Open(context.Background(), EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("$events: %v", err)
	}
	defer stream.Close()
	drainOne(t, stream) // ready

	f.service.Emit(EventSessionStatus, "session-1", true)
	frame := drainOne(t, stream)
	var event struct {
		Type  string `json:"type"`
		Event string `json:"event"`
		Args  []any  `json:"args"`
	}
	decode(t, frame, &event)
	if event.Type != "emit" || event.Event != EventSessionStatus {
		t.Fatalf("event frame = %s", frame)
	}
	if len(event.Args) != 2 || event.Args[0] != "session-1" || event.Args[1] != true {
		t.Fatalf("event args = %#v", event.Args)
	}
}

func TestPromptForwardsRunningStatus(t *testing.T) {
	f := newFixture(t)
	stream, err := f.service.Open(context.Background(), EndpointEvents, []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("$events: %v", err)
	}
	defer stream.Close()
	drainOne(t, stream) // ready

	opened := f.createSession(t)
	f.invoke(t, "session/prompt", map[string]any{
		"request": map[string]any{
			"requestId": "rpc-status",
			"sessionId": opened.ID(),
			"mode":      "queue",
			"content":   []map[string]any{{"type": "text", "text": "status"}},
		},
	}).mustSucceed(t)

	statuses := make([]bool, 0, 2)
	for len(statuses) < 2 {
		var event struct {
			Type  string `json:"type"`
			Event string `json:"event"`
			Args  []any  `json:"args"`
		}
		decode(t, drainOne(t, stream), &event)
		if event.Type != "emit" || event.Event != EventSessionStatus || len(event.Args) != 2 {
			t.Fatalf("unexpected event frame: %+v", event)
		}
		if event.Args[0] != opened.ID() {
			t.Fatalf("status session = %v, want %s", event.Args[0], opened.ID())
		}
		running, ok := event.Args[1].(bool)
		if !ok {
			t.Fatalf("status value = %#v", event.Args[1])
		}
		statuses = append(statuses, running)
	}
	if statuses[0] != true || statuses[1] != false {
		t.Fatalf("statuses = %v, want [true false]", statuses)
	}
}

func TestSessionControlBaseline(t *testing.T) {
	f := newFixture(t)
	stream, err := f.service.Open(context.Background(), "session/control", []byte(`{"args":{}}`), nil)
	if err != nil {
		t.Fatalf("session/control: %v", err)
	}
	defer stream.Close()
	frame := drainOne(t, stream)
	var baseline struct {
		Type  string         `json:"type"`
		Value map[string]any `json:"value"`
	}
	decode(t, frame, &baseline)
	if baseline.Type != "baseline" {
		t.Fatalf("frame = %+v", baseline)
	}
	if _, ok := baseline.Value["projections"]; !ok {
		t.Fatalf("value = %v", baseline.Value)
	}
}

func seedTurn(t *testing.T, opened *session.Session, turn int) {
	t.Helper()
	if _, err := opened.Append(session.EventTurnStart, map[string]any{"turn": turn}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := opened.Append(session.EventStepStart, map[string]any{"turn": turn, "step": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := opened.AppendSurface(session.EventUserMessage, map[string]any{
		"role":    "user",
		"id":      session.NewID(),
		"content": []map[string]any{{"type": "text", "text": "hello"}},
		"source":  map[string]any{"kind": "user", "rpcId": "rpc"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := opened.AppendSurface(session.EventAssistantMessage, map[string]any{
		"turn": turn,
		"step": 1,
		"message": map[string]any{
			"role":    "assistant",
			"id":      session.NewID(),
			"content": []map[string]any{{"type": "text", "text": "hi"}},
			"source":  map[string]any{"kind": "model", "provider": "fake-provider", "model": "fake-model"},
		},
		"stream": []any{},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := opened.Append(session.EventStepEnd, map[string]any{"turn": turn, "step": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := opened.Append(session.EventTurnEnd, map[string]any{"turn": turn, "reason": map[string]any{"kind": "completed"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func drain(t *testing.T, stream mux.Stream) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var values [][]byte
	for {
		value, err := stream.Next(ctx)
		if err == io.EOF {
			return values
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		values = append(values, value)
	}
}

func drainOne(t *testing.T, stream mux.Stream) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	value, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return value
}

func decode(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func decodeItem(t *testing.T, raw []byte) result {
	t.Helper()
	var decoded result
	decode(t, raw, &decoded)
	return decoded
}

func asFailure(err error, target **mux.Failure) bool {
	return errors.As(err, target)
}

func TestSessionAttachmentServesOnlyReferencedImages(t *testing.T) {
	f := newFixture(t)
	opened := f.createSession(t)
	data := []byte("\x89PNG\r\n\x1a\npayload")
	sum := sha256.Sum256(data)
	id := "sha256:" + hex.EncodeToString(sum[:])
	path := filepath.Join(f.root, ".dsh-go", "attachments", "v1", "objects", hex.EncodeToString(sum[:])[:2], hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ref := map[string]any{"attachmentId": id, "mediaType": "image/png", "bytes": len(data), "width": 1, "height": 1}
	if _, err := opened.AppendSurface(session.EventToolResult, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"role": "tool", "content": []any{map[string]any{
			"type": "image", "attachment": ref,
		}}},
	}); err != nil {
		t.Fatal(err)
	}

	value := f.invoke(t, "session/attachment", map[string]any{"sessionId": opened.ID(), "attachmentId": id}).mustSucceed(t)
	var attachment struct {
		Attachment map[string]any `json:"attachment"`
		Data       string         `json:"data"`
	}
	decode(t, value, &attachment)
	if attachment.Attachment["attachmentId"] != id || attachment.Attachment["mediaType"] != "image/png" {
		t.Fatalf("attachment = %+v", attachment.Attachment)
	}
	if attachment.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("data = %q", attachment.Data)
	}

	other := f.createSession(t)
	f.invoke(t, "session/attachment", map[string]any{"sessionId": other.ID(), "attachmentId": id}).
		mustFail(t, "ATTACHMENT_NOT_REFERENCED")
	f.invoke(t, "session/attachment", map[string]any{"sessionId": "missing", "attachmentId": id}).
		mustFail(t, "session/not-found")
}
