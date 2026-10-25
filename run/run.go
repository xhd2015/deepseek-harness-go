// Package run wires configuration, storage, the agent, and the HTTP server
// together, and implements the dsh-go command line.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/api"
	"github.com/xhd2015/deepseek-harness-go/assets"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/server"
	"github.com/xhd2015/deepseek-harness-go/session"
	"github.com/xhd2015/deepseek-harness-go/version"
)

// Defaults for the serve command.
const (
	DefaultPort   = 3070
	DefaultHost   = "127.0.0.1"
	DefaultPreset = "std-no-subagent"
)

// Environment variables read by the command line.
const (
	EnvSessionsRoot = "DSH_GO_SESSIONS_ROOT"
	EnvToken        = "DSH_GO_TOKEN"
	EnvProvider     = "DSH_GO_PROVIDER"
	EnvModel        = "DSH_GO_MODEL"
	EnvReasoning    = "DSH_GO_REASONING_EFFORT"
	EnvProtocol     = "DSH_GO_PROTOCOL"
	EnvModels       = "DSH_GO_MODELS"
	EnvToolRoot     = "DSH_GO_TOOL_ROOT"
	EnvAssetsDir    = "DSH_GO_ASSETS_DIR"
	EnvBaseURL      = "DEEPSEEK_BASE_URL"
	EnvAPIKey       = "DEEPSEEK_API_KEY"
)

// Options configures one dsh-go process.
type Options struct {
	Host         string
	Port         int
	SessionsRoot string
	AssetsDir    string
	Token        string
	ProxyTo      string
	Provider     string
	Model        string
	Reasoning    string
	Protocol     string
	// Models points at the cordis patch that describes every route llm-proxy
	// serves. An empty value uses ~/.dsh/profiles/web/cordis.patch.yml.
	Models     string
	BaseURL    string
	APIKey     string
	DefaultCwd string
	Home       string
	// ToolRoot is the fallback workspace for tool calls whose session has no
	// recorded cwd, and the directory validated at startup. It is not a read
	// jail. Empty uses the process working directory.
	ToolRoot string

	Stdout io.Writer
	Stderr io.Writer

	// routeExplicit is set when the provider/model came from a flag or the
	// environment rather than from the built-in single-route default.
	routeExplicit bool
}

// NewServer builds a server and its cleanup function.
func NewServer(opts Options) (*server.Server, func(), error) {
	if err := opts.applyDefaults(); err != nil {
		return nil, nil, err
	}
	store, err := session.NewStore(opts.SessionsRoot)
	if err != nil {
		return nil, nil, err
	}
	captured, err := assets.Load(opts.AssetsDir)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	logf := opts.logf()
	catalog, clients, err := buildModelSurface(opts, logf)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	runner := agent.New(agent.Config{
		Catalog:         catalog,
		Clients:         clients,
		Provider:        catalog.Default.Provider,
		Model:           catalog.Default.Model,
		ReasoningEffort: catalog.Default.ReasoningEffort,
		ToolRoot:        opts.ToolRoot,
		Logf:            logf,
	})
	service := api.New(api.Config{
		Store:       store,
		Runner:      runner,
		Catalog:     catalog,
		DefaultCwd:  opts.DefaultCwd,
		AgentPreset: DefaultPreset,
		HomeDir:     opts.Home,
		Logf:        logf,
	})
	srv, err := server.New(server.Config{
		Addr:    net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)),
		Service: service,
		Assets:  captured,
		ProxyTo: opts.ProxyTo,
		Token:   opts.Token,
		Logf:    logf,
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}

	logf("models: %d routes, default %s/%s", catalog.RouteCount(), catalog.Default.Provider, catalog.Default.Model)
	logf("sessions in %s", opts.SessionsRoot)
	if opts.AssetsDir != "" {
		logf("assets from %s", opts.AssetsDir)
	} else if captured.Available() {
		logf("assets embedded (%d responses)", len(captured.Manifest().Entries))
	} else {
		logf("no web assets staged: run `go run ./script/sync-assets` then rebuild")
	}

	cleanup := func() {
		runner.Close()
		_ = store.Close()
	}
	return srv, cleanup, nil
}

// applyEnv fills unset options from the environment. The command line parses
// flags after the environment, and the development server builds options
// directly, so both entry points share this single source of defaults.
func (opts *Options) applyEnv() {
	set := func(target *string, name string) {
		if *target == "" {
			*target = os.Getenv(name)
		}
	}
	set(&opts.SessionsRoot, EnvSessionsRoot)
	set(&opts.Token, EnvToken)
	set(&opts.Provider, EnvProvider)
	set(&opts.Model, EnvModel)
	set(&opts.Reasoning, EnvReasoning)
	set(&opts.Protocol, EnvProtocol)
	set(&opts.Models, EnvModels)
	set(&opts.ToolRoot, EnvToolRoot)
	set(&opts.AssetsDir, EnvAssetsDir)
	set(&opts.BaseURL, EnvBaseURL)
	set(&opts.APIKey, EnvAPIKey)
}

// NewHandler builds only the HTTP handler, for the development server.
func NewHandler(opts Options) (http.Handler, func(), error) {
	srv, cleanup, err := NewServer(opts)
	if err != nil {
		return nil, nil, err
	}
	return srv.Handler(), cleanup, nil
}

func (opts *Options) applyDefaults() error {
	opts.applyEnv()
	opts.routeExplicit = strings.TrimSpace(opts.Provider) != "" || strings.TrimSpace(opts.Model) != ""
	if opts.Host == "" {
		opts.Host = DefaultHost
	}
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return fmt.Errorf("port %d is out of range", opts.Port)
	}
	if opts.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home directory: %w", err)
		}
		opts.Home = home
	}
	if opts.SessionsRoot == "" {
		opts.SessionsRoot = filepath.Join(opts.Home, ".dsh-go", "sessions")
	}
	if opts.DefaultCwd == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.DefaultCwd = cwd
		} else {
			opts.DefaultCwd = opts.Home
		}
	}
	if opts.Provider == "" {
		opts.Provider = "deepseek"
	}
	if opts.Model == "" {
		opts.Model = "deepseek-chat"
	}
	if opts.BaseURL == "" {
		opts.BaseURL = llm.DefaultBaseURL
	}
	if opts.Protocol == "" {
		opts.Protocol = llm.ProtocolOpenAIChat
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	return nil
}

func (opts Options) logf() func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(opts.Stderr, "dsh-go: "+format+"\n", args...)
	}
}

const serveHelp = `
Usage: dsh-go [serve] [options]

Serve the DeepSeek Harness web client backed by a Go session store.

Options:
  -p, --port PORT          listen port (default 3070)
  -H, --host HOST          listen host (default 127.0.0.1)
      --sessions-root DIR  session storage (default ~/.dsh-go/sessions)
      --assets-dir DIR     serve a captured frontend from DIR instead of the
                           embedded copy
      --token TOKEN        require a token; open /?token=TOKEN once to set the
                           auth cookie (default: no authentication)
      --proxy-to URL       forward non-API requests to a running web UI, for
                           development
      --provider NAME      model provider label (default deepseek)
      --model NAME         model name (default deepseek-chat)
      --reasoning LEVEL    reasoning effort passed to the model
      --protocol NAME      model wire protocol: openai-chat or
                           anthropic-messages (default openai-chat)
      --models FILE        cordis patch that defines every model route
                           (default ~/.dsh/profiles/web/cordis.patch.yml)
      --base-url URL       model endpoint base URL
                           (default https://api.deepseek.com/v1)
      --tool-root DIR      fallback workspace when a session has no cwd
                           (default: process working directory)
  -v, --version            print the version
  -h, --help               show this help

Environment:
  DEEPSEEK_API_KEY         API key for the model provider
  DEEPSEEK_BASE_URL        alternative OpenAI-compatible base URL
  DSH_GO_SESSIONS_ROOT     session storage directory
  DSH_GO_TOKEN             auth token
  DSH_GO_PROVIDER          provider label
  DSH_GO_MODEL             model name
  DSH_GO_REASONING_EFFORT  reasoning effort
  DSH_GO_PROTOCOL         model wire protocol (openai-chat or anthropic-messages)
  DSH_GO_MODELS           cordis patch that defines every model route
  DSH_GO_TOOL_ROOT        fallback tool workspace (default: process cwd)
  DSH_GO_ASSETS_DIR        captured frontend directory

Commands:
  serve                    run the web server (default)
  version                  print the version
`

// Main runs the command line.
func Main(args []string) {
	var opts Options
	var showVersion bool
	remain, err := lessflags.Int("-p,--port", &opts.Port).
		String("-H,--host", &opts.Host).
		String("--sessions-root", &opts.SessionsRoot).
		String("--assets-dir", &opts.AssetsDir).
		String("--token", &opts.Token).
		String("--proxy-to", &opts.ProxyTo).
		String("--provider", &opts.Provider).
		String("--model", &opts.Model).
		String("--reasoning", &opts.Reasoning).
		String("--protocol", &opts.Protocol).
		String("--models", &opts.Models).
		String("--base-url", &opts.BaseURL).
		String("--tool-root", &opts.ToolRoot).
		Bool("-v,--version", &showVersion).
		Help("-h,--help", serveHelp).
		Parse(args)
	if err != nil {
		if errors.Is(err, lessflags.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "dsh-go: %v\n", err)
		fmt.Fprint(os.Stderr, serveHelp)
		os.Exit(2)
	}
	if showVersion {
		printVersion()
		return
	}

	command := "serve"
	if len(remain) > 0 {
		command = remain[0]
		remain = remain[1:]
	}
	switch command {
	case "serve":
		if len(remain) > 0 {
			fmt.Fprintf(os.Stderr, "dsh-go: serve takes no arguments, got %q\n", remain)
			os.Exit(2)
		}
		serve(opts)
	case "version":
		printVersion()
	default:
		fmt.Fprintf(os.Stderr, "dsh-go: unknown command %q\n\n%s", command, serveHelp)
		os.Exit(2)
	}
}

func serve(opts Options) {
	srv, cleanup, err := NewServer(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsh-go: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "dsh-go: %v\n", err)
		os.Exit(1)
	}
}

func printVersion() {
	fmt.Printf("dsh-go %s\n", strings.TrimSpace(version.Version))
}

// buildModelSurface loads the cordis model patch and builds one client per
// provider. A missing or empty patch falls back to the legacy single route,
// which keeps old deployments working while the patch is being rolled out.
func buildModelSurface(opts Options, logf func(format string, args ...any)) (*modelconfig.Catalog, map[string]*llm.Client, error) {
	legacy := func() (*modelconfig.Catalog, map[string]*llm.Client) {
		catalog := modelconfig.Single(opts.Provider, opts.Model, opts.Reasoning)
		client := llm.New(llm.Config{
			BaseURL:  opts.BaseURL,
			APIKey:   opts.APIKey,
			Model:    opts.Model,
			Protocol: opts.Protocol,
		})
		return catalog, map[string]*llm.Client{
			modelRouteKey(opts.Provider, opts.Model): client,
		}
	}

	models := strings.TrimSpace(opts.Models)
	if models == "off" || models == "none" {
		catalog, clients := legacy()
		return catalog, clients, nil
	}
	if models == "" {
		models = filepath.Join(opts.Home, ".dsh", "profiles", "web", "cordis.patch.yml")
	}
	catalog, err := modelconfig.LoadPatch(models)
	if err != nil {
		return nil, nil, fmt.Errorf("load model catalog: %w", err)
	}
	if catalog == nil {
		logf("models: %s has no providers, using %s/%s", models, opts.Provider, opts.Model)
		catalog, clients := legacy()
		return catalog, clients, nil
	}

	if opts.routeExplicit {
		selection := modelconfig.Selection{
			Provider:        opts.Provider,
			Model:           opts.Model,
			ReasoningEffort: opts.Reasoning,
		}
		route, resolveErr := catalog.Resolve(selection)
		if resolveErr != nil && strings.Contains(resolveErr.Error(), "unknown reasoning effort") {
			selection.ReasoningEffort = ""
			route, resolveErr = catalog.Resolve(selection)
		}
		if resolveErr != nil && selection.Provider != "" && !catalog.HasProvider(selection.Provider) {
			// The deployment predates the patch and names a provider the patch
			// spells differently. Keep that route alive instead of silently
			// switching the public default to another vendor.
			catalog.Providers = append(catalog.Providers, modelconfig.Provider{
				ID:       selection.Provider,
				Name:     selection.Provider,
				Protocol: opts.Protocol,
				BaseURL:  opts.BaseURL,
				Models: []modelconfig.Model{{
					ID:            selection.Model,
					Name:          selection.Model,
					DefaultEffort: selection.ReasoningEffort,
				}},
			})
			route, resolveErr = catalog.Resolve(selection)
			if resolveErr == nil {
				logf("models: kept legacy route %s/%s", route.Provider, route.Model)
			}
		}
		if resolveErr != nil {
			logf("models: configured route %s/%s is unavailable: %v; using %s/%s",
				selection.Provider, selection.Model, resolveErr,
				catalog.Default.Provider, catalog.Default.Model)
		} else {
			catalog.Default = route.Selection
		}
	}

	clients := make(map[string]*llm.Client, catalog.RouteCount())
	for _, provider := range catalog.Providers {
		client := llm.New(llm.Config{
			BaseURL:  provider.BaseURL,
			APIKey:   provider.APIKey,
			Protocol: provider.Protocol,
		})
		for _, model := range provider.Models {
			clients[modelRouteKey(provider.ID, model.ID)] = client
		}
	}
	return catalog, clients, nil
}

func modelRouteKey(provider, model string) string { return provider + "\x00" + model }
