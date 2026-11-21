package compat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentprodsh "github.com/xhd2015/agent-pro/agent/dsh/web"
	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/api"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/server"
	"github.com/xhd2015/deepseek-harness-go/session"
	"github.com/xhd2015/deepseek-harness-go/workspace"
)

// TestAgentProClientDrivesOurServer is the SDK contract: agent-pro's own client
// must create, prompt, and query a session against this backend without knowing
// it is not the Node Host. It discovers the backend through the same
// web-listen.json record the Host writes.
func TestAgentProClientDrivesOurServer(t *testing.T) {
	dataDir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dataDir, "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	workspaces, err := workspace.Open(filepath.Join(dataDir, "storages"))
	if err != nil {
		t.Fatalf("workspace.Open: %v", err)
	}
	drafts, err := api.OpenDrafts(filepath.Join(dataDir, "storages"))
	if err != nil {
		t.Fatalf("OpenDrafts: %v", err)
	}
	catalog := modelconfig.Single("fake-provider", "fake-model", "")
	runner := agent.New(agent.Config{
		Client:   llm.New(llm.Config{BaseURL: "http://127.0.0.1:1", Model: "fake-model", APIKey: "k"}),
		Catalog:  catalog,
		Provider: "fake-provider",
		Model:    "fake-model",
		DSHHome:  dataDir,
	})
	t.Cleanup(runner.Close)

	service := api.New(api.Config{
		Store:      store,
		Runner:     runner,
		Catalog:    catalog,
		DefaultCwd: t.TempDir(),
		Workspaces: workspaces,
		Drafts:     drafts,
		HomeDir:    dataDir,
		DataDir:    dataDir,
		DSHHome:    dataDir,
	})
	srv, err := server.New(server.Config{
		Addr:            "127.0.0.1:0",
		Service:         service,
		ListenRecord:    true,
		ListenRecordDir: dataDir,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = store.Close()
	})
	go func() { _ = srv.Serve(ctx) }()

	recordPath := filepath.Join(dataDir, server.ListenFileName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(recordPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listen record %s never appeared", recordPath)
		}
		time.Sleep(20 * time.Millisecond)
	}

	client := agentprodsh.Client{Home: dataDir, HTTPClient: nil}
	if _, err := client.ModelCatalog(ctx); err != nil {
		t.Fatalf("agent-pro ModelCatalog: %v", err)
	}
	project := t.TempDir()
	sessionID := "session-compat-rpc"
	created, err := client.Create(ctx, project, sessionID)
	if err != nil {
		t.Fatalf("agent-pro Create: %v", err)
	}
	if created != sessionID {
		t.Fatalf("created = %q, want %q", created, sessionID)
	}
	if err := client.SetDraft(ctx, sessionID, "half typed prompt"); err != nil {
		t.Fatalf("agent-pro SetDraft: %v", err)
	}
	if err := client.Send(ctx, sessionID, "request-1", "queued prompt", true); err != nil {
		t.Fatalf("agent-pro Send(noSubmit): %v", err)
	}
	// A second session exists so a filtered query can be proven filtered: the
	// client wraps its ids in `request`, and ignoring that envelope would report
	// every stored session.
	if _, err := client.Create(ctx, t.TempDir(), "session-compat-other"); err != nil {
		t.Fatalf("agent-pro Create(other): %v", err)
	}
	status, err := client.Status(ctx, []string{sessionID, "session-compat-missing"})
	if err != nil {
		t.Fatalf("agent-pro Status: %v", err)
	}
	if running, ok := status[sessionID]; !ok || running {
		t.Fatalf("status = %v, want the idle session present", status)
	}
	if len(status) != 1 {
		t.Fatalf("filtered status = %v, want exactly the named session", status)
	}
	all, err := client.Status(ctx, nil)
	if err != nil {
		t.Fatalf("agent-pro Status(all): %v", err)
	}
	if _, ok := all["session-compat-other"]; !ok {
		t.Fatalf("all-ids status = %v, missing the second session", all)
	}
	model, err := client.SessionModel(ctx, sessionID)
	if err != nil {
		t.Fatalf("agent-pro SessionModel: %v", err)
	}
	_ = model

	selected, err := client.SelectModel(ctx, sessionID, "fake-provider", "fake-model", "")
	if err != nil {
		t.Fatalf("agent-pro SelectModel: %v", err)
	}
	if selected.Provider != "fake-provider" || selected.Model != "fake-model" {
		t.Fatalf("selected = %+v", selected)
	}
	draftText, err := serviceDraft(dataDir, sessionID)
	if err != nil {
		t.Fatalf("draft record: %v", err)
	}
	if draftText != "queued prompt" {
		t.Fatalf("durable draft = %q, want the last no-submit prompt", draftText)
	}
}

// serviceDraft reads the draft the way another Harness process would.
func serviceDraft(dataDir, sessionID string) (string, error) {
	drafts, err := api.OpenDrafts(filepath.Join(dataDir, "storages"))
	if err != nil {
		return "", err
	}
	return drafts.Get(sessionID), nil
}
