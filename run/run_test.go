package run

import (
	"testing"

	"github.com/xhd2015/deepseek-harness-go/llm"
)

func TestApplyDefaultsReadsEnvironment(t *testing.T) {
	t.Setenv(EnvSessionsRoot, "/tmp/dsh-go-test-sessions")
	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvProvider, "env-provider")
	t.Setenv(EnvModel, "env-model")
	t.Setenv(EnvReasoning, "high")
	t.Setenv(EnvProtocol, llm.ProtocolAnthropicMessages)
	t.Setenv(EnvAssetsDir, "/tmp/dsh-go-test-assets")
	t.Setenv(EnvBaseURL, "http://127.0.0.1:9/v1")
	t.Setenv(EnvAPIKey, "env-key")

	var opts Options
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	if opts.Host != DefaultHost || opts.Port != DefaultPort {
		t.Fatalf("listen defaults = %s:%d, want %s:%d", opts.Host, opts.Port, DefaultHost, DefaultPort)
	}
	checks := map[string]struct{ got, want string }{
		"sessions root": {opts.SessionsRoot, "/tmp/dsh-go-test-sessions"},
		"token":         {opts.Token, "env-token"},
		"provider":      {opts.Provider, "env-provider"},
		"model":         {opts.Model, "env-model"},
		"reasoning":     {opts.Reasoning, "high"},
		"protocol":      {opts.Protocol, llm.ProtocolAnthropicMessages},
		"assets dir":    {opts.AssetsDir, "/tmp/dsh-go-test-assets"},
		"base URL":      {opts.BaseURL, "http://127.0.0.1:9/v1"},
		"API key":       {opts.APIKey, "env-key"},
	}
	for name, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %q, want %q", name, check.got, check.want)
		}
	}
}

func TestApplyDefaultsPrefersExplicitOptions(t *testing.T) {
	t.Setenv(EnvSessionsRoot, "/tmp/dsh-go-env-sessions")
	t.Setenv(EnvModel, "env-model")
	t.Setenv(EnvAPIKey, "env-key")

	opts := Options{
		SessionsRoot: "/tmp/dsh-go-flag-sessions",
		Model:        "flag-model",
		APIKey:       "flag-key",
	}
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	if opts.SessionsRoot != "/tmp/dsh-go-flag-sessions" {
		t.Errorf("sessions root = %q, want the explicit value", opts.SessionsRoot)
	}
	if opts.Model != "flag-model" {
		t.Errorf("model = %q, want the explicit value", opts.Model)
	}
	if opts.APIKey != "flag-key" {
		t.Errorf("API key = %q, want the explicit value", opts.APIKey)
	}
	if opts.Provider != "deepseek" {
		t.Errorf("provider = %q, want the built-in default", opts.Provider)
	}
}
