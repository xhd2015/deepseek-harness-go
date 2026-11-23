// Package host wires a complete dsh-go backend — session store, workspaces,
// drafts, the model catalog, the turn runner, and the HTTP surface — without
// the embedded frontend and without a command line.
//
// It exists so another Go program (eluc) can mount the same backend dsh-go
// serves, sharing one set of functions instead of reimplementing the wiring.
// Importing this package never links the 18 MiB frontend capture: the page is
// whatever Frontend the host supplies, and `run` is the only package that
// loads the embedded copy.
package host

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/agent"
	"github.com/xhd2015/deepseek-harness-go/api"
	"github.com/xhd2015/deepseek-harness-go/llm"
	"github.com/xhd2015/deepseek-harness-go/modelconfig"
	"github.com/xhd2015/deepseek-harness-go/server"
	"github.com/xhd2015/deepseek-harness-go/session"
	"github.com/xhd2015/deepseek-harness-go/workspace"
)

// Defaults for one host.
const (
	DefaultPort   = 8300
	DefaultHost   = "127.0.0.1"
	DefaultPreset = "std-no-subagent"
	// DefaultAddr is the loopback address DefaultHost and DefaultPort spell.
	DefaultAddr = "127.0.0.1:8300"
)

// Environment variables read by the command line and this package.
const (
	EnvHome         = "DSH_GO_HOME"
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
	Host string
	Port int
	// Addr overrides Host and Port when set, for a host that lets the OS pick
	// (127.0.0.1:0) or binds somewhere the port number cannot express.
	Addr string
	// DataDir holds sessions, attachments, storages and the listen record. It
	// defaults to ~/.dsh-go and is deliberately distinct from DSH's ~/.dsh.
	DataDir string
	// Compression is the session artifact encoding: zstd, none, or empty to
	// detect an existing root and otherwise write zstd.
	Compression string
	// ListenRecord asks the server to write a listen record under the data dir.
	ListenRecord bool
	// PublishListenTo additionally writes the record into this directory, so
	// agent-pro and dsh web open can find this process without DSH_HOME.
	PublishListenTo string
	SessionsRoot    string
	// AssetsDir is the frontend capture directory the CLI loads. A host that
	// brings its own frontend leaves it empty and sets Frontend instead.
	AssetsDir string
	// Frontend answers page requests. Nil serves the setup page; the CLI fills
	// it from the embedded capture, a host from whatever it wants to show.
	Frontend  server.Frontend
	Token     string
	ProxyTo   string
	Provider  string
	Model     string
	Reasoning string
	Protocol  string
	// Models points at the cordis patch that describes every route llm-proxy
	// serves. An empty value uses ~/.dsh/profiles/web/cordis.patch.yml.
	Models     string
	BaseURL    string
	APIKey     string
	DefaultCwd string
	// UserHome is the operating-system home directory.
	UserHome string
	// ToolRoot is the fallback workspace for tool calls whose session has no
	// recorded cwd, and the directory validated at startup. It is not a read
	// jail. Empty uses the process working directory.
	ToolRoot string

	Stdout io.Writer
	Stderr io.Writer

	// routeExplicit is set when the provider/model came from a flag or the
	// environment rather than from the built-in single-route default.
	routeExplicit bool
	// sharedHarnessHome records that the data directory is DSH's own home, so
	// startup can warn about two hosts sharing one state tree.
	sharedHarnessHome bool
}

// listenAddr is the address this host binds: the explicit Addr, else Host:Port.
func (opts Options) listenAddr() string {
	if addr := strings.TrimSpace(opts.Addr); addr != "" {
		return addr
	}
	return net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
}

// ApplyDefaults fills every unset field from the environment and the built-in
// defaults. Callers that build an Options directly use it; NewServer calls it
// for them.
func (opts *Options) ApplyDefaults() error {
	opts.applyEnv()
	opts.routeExplicit = strings.TrimSpace(opts.Provider) != "" || strings.TrimSpace(opts.Model) != ""
	if opts.Host == "" {
		opts.Host = DefaultHost
	}
	if opts.Port == 0 && strings.TrimSpace(opts.Addr) == "" {
		opts.Port = DefaultPort
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return fmt.Errorf("port %d is out of range", opts.Port)
	}
	if opts.UserHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home directory: %w", err)
		}
		opts.UserHome = home
	}
	if opts.DataDir == "" {
		opts.DataDir = filepath.Join(opts.UserHome, ".dsh-go")
	}
	if opts.SessionsRoot == "" {
		opts.SessionsRoot = filepath.Join(opts.DataDir, "sessions")
	}
	if sameDir(opts.DataDir, filepath.Join(opts.UserHome, ".dsh")) {
		opts.sharedHarnessHome = true
	}
	if opts.Compression == "auto" {
		opts.Compression = ""
	}
	switch session.Compression(opts.Compression) {
	case "", session.CompressionZstd, session.CompressionNone:
	default:
		return fmt.Errorf("--session-compression must be zstd, none, or auto")
	}
	if opts.DefaultCwd == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.DefaultCwd = cwd
		} else {
			opts.DefaultCwd = opts.UserHome
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

// NewServer builds a server and its cleanup function.
func NewServer(opts Options) (*server.Server, func(), error) {
	if err := opts.ApplyDefaults(); err != nil {
		return nil, nil, err
	}
	store, err := session.NewStoreWithConfig(session.Config{
		Root:        opts.SessionsRoot,
		Compression: session.Compression(opts.Compression),
	})
	if err != nil {
		return nil, nil, err
	}
	workspaces, err := workspace.Open(filepath.Join(opts.DataDir, "storages"))
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	drafts, err := api.OpenDrafts(filepath.Join(opts.DataDir, "storages"))
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
	// The session store is handed to the runner as well: compaction and the
	// manual /compact path open a session by id, with no handle in hand.
	runner := agent.New(agent.Config{
		Catalog:         catalog,
		Clients:         clients,
		Provider:        catalog.Default.Provider,
		Model:           catalog.Default.Model,
		ReasoningEffort: catalog.Default.ReasoningEffort,
		ToolRoot:        opts.ToolRoot,
		DSHHome:         opts.DataDir,
		ModelsPath:      opts.Models,
		Store:           store,
		Compaction:      &agent.CompactionConfig{ContextWindowTokens: compactionWindow(catalog)},
		Skills:          loadSkillsFor(opts, logf),
		// The catalog is per project, so it is re-resolved for a session whose cwd
		// differs from the boot directory. Boot-time resolution alone would give a
		// session the skills of whatever directory the server happened to start in.
		ResolveSkills: func(cwd string) []agent.SkillDefinition {
			if cwd == "" || cwd == opts.ToolRoot {
				return nil
			}
			return loadSkillsFor(Options{ToolRoot: cwd, DataDir: opts.DataDir}, logf)
		},
		Logf: logf,
	})
	// The page decides approvals; the runner only records and waits. Without this
	// hook an escalation would be recorded and then fail closed on timeout, which
	// looks to the user like a hang rather than a question.
	var serviceRef *api.Service
	runner.SetApprovalNotifier(func(request agent.ApprovalRequest) {
		if serviceRef == nil {
			return
		}
		// Publish only. The answer arrives through $events/result and is settled
		// against the same wait the tool call is blocked on, so there is one chain
		// from question to decision rather than two that can disagree.
		serviceRef.PublishApproval(request)
	})
	service := api.New(api.Config{
		Store:       store,
		Runner:      runner,
		Catalog:     catalog,
		DefaultCwd:  opts.DefaultCwd,
		AgentPreset: DefaultPreset,
		HomeDir:     opts.UserHome,
		DataDir:     opts.DataDir,
		DSHHome:     opts.DataDir,
		Workspaces:  workspaces,
		Drafts:      drafts,
		ReloadModels: func(path string) (*modelconfig.Catalog, map[string]*llm.Client, error) {
			return buildModelSurface(Options{
				DataDir:   opts.DataDir,
				UserHome:  opts.UserHome,
				Models:    path,
				Provider:  opts.Provider,
				Model:     opts.Model,
				Reasoning: opts.Reasoning,
				Protocol:  opts.Protocol,
				BaseURL:   opts.BaseURL,
				APIKey:    opts.APIKey,
			}, logf)
		},
		Logf: logf,
	})
	serviceRef = service
	srv, err := server.New(server.Config{
		Addr:     opts.listenAddr(),
		Service:  service,
		Frontend: opts.Frontend,
		ProxyTo:  opts.ProxyTo,
		Token:    opts.Token,
		// The listen record is how external clients find this process.
		ListenRecord:    opts.ListenRecord,
		ListenRecordDir: opts.DataDir,
		PublishListenTo: opts.PublishListenTo,
		Logf:            logf,
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}

	logf("models: %d routes, default %s/%s", catalog.RouteCount(), catalog.Default.Provider, catalog.Default.Model)
	logf("data dir %s", opts.DataDir)
	if opts.sharedHarnessHome {
		logf("warning: sharing a harness home with a running DSH host; the write lease is required and this root's encoding must match")
	}
	logf("sessions in %s (%s)", opts.SessionsRoot, store.Compression())
	if opts.Frontend != nil && opts.Frontend.Responses() > 0 {
		logf("frontend: %d responses", opts.Frontend.Responses())
	}

	cleanup := func() {
		runner.Close()
		_ = store.Close()
	}
	return srv, cleanup, nil
}

// NewHandler builds only the HTTP handler, for a host that owns its listener.
func NewHandler(opts Options) (http.Handler, func(), error) {
	srv, cleanup, err := NewServer(opts)
	if err != nil {
		return nil, nil, err
	}
	return srv.Handler(), cleanup, nil
}

// Mount is NewHandler for a host that serves its own page (eluc). It never
// loads a frontend, so the caller's binary carries no capture.
func Mount(opts Options) (http.Handler, func(), error) {
	opts.Frontend = nil
	return NewHandler(opts)
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
	set(&opts.DataDir, EnvHome)
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

func (opts Options) logf() func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(opts.Stderr, "dsh-go: "+format+"\n", args...)
	}
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
			Provider: opts.Provider,
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
		models = filepath.Join(opts.DataDir, "profiles", "web", "cordis.patch.yml")
		if _, statErr := os.Stat(models); statErr != nil {
			// The model catalog predates the split data directory; keep reading
			// the existing Harness profile rather than silently dropping it.
			legacy := filepath.Join(opts.UserHome, ".dsh", "profiles", "web", "cordis.patch.yml")
			if _, legacyErr := os.Stat(legacy); legacyErr == nil {
				logf("models: %s not found, reading %s", models, legacy)
				models = legacy
			}
		}
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
			// The provider name decides provider-specific attribution headers: an
			// OpenCode route rejects a request that omits its session header.
			Provider: provider.ID,
		})
		for _, model := range provider.Models {
			clients[modelRouteKey(provider.ID, model.ID)] = client
		}
	}
	return catalog, clients, nil
}

func modelRouteKey(provider, model string) string { return provider + "\x00" + model }

// sameDir compares two directory paths after cleaning.
func sameDir(a, b string) bool {
	left, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// compactionWindow reports the window the routed model declares, or zero when it
// declares none.
//
// Zero deliberately disables the automatic trigger: without a capacity there is no
// pressure to measure, and firing compaction on a guess would replace history the
// session still fits.
func compactionWindow(catalog *modelconfig.Catalog) int64 {
	if catalog == nil {
		return 0
	}
	for _, provider := range catalog.Providers {
		if provider.ID != catalog.Default.Provider {
			continue
		}
		for _, model := range provider.Models {
			if model.ID == catalog.Default.Model {
				return int64(model.ContextWindow)
			}
		}
	}
	return 0
}

// loadSkillsFor reads the skill catalog once at boot.
//
// The roots are the ones DSH searches: the project's own directories, then the
// user's, so a repository can pin instructions without renaming them.
func loadSkillsFor(opts Options, logf func(format string, args ...any)) []agent.SkillDefinition {
	roots := agent.SkillRoots(opts.ToolRoot, opts.DataDir, agent.AgentsHome(), nil)
	skills, err := agent.LoadSkills(roots)
	if err != nil && logf != nil {
		logf("skills: %v", err)
	}
	return skills
}
