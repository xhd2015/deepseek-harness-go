// Package run is the dsh-go command line: it parses flags, loads the embedded
// frontend, and serves the backend that package host wires.
package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	lessflags "github.com/xhd2015/less-flags"

	"github.com/xhd2015/deepseek-harness-go/assets"
	"github.com/xhd2015/deepseek-harness-go/host"
	"github.com/xhd2015/deepseek-harness-go/server"
	"github.com/xhd2015/deepseek-harness-go/version"
)

// Re-exported settings so a host and this command line cannot drift.
const (
	DefaultPort   = host.DefaultPort
	DefaultHost   = host.DefaultHost
	DefaultPreset = host.DefaultPreset

	EnvHome         = host.EnvHome
	EnvSessionsRoot = host.EnvSessionsRoot
	EnvToken        = host.EnvToken
	EnvProvider     = host.EnvProvider
	EnvModel        = host.EnvModel
	EnvReasoning    = host.EnvReasoning
	EnvProtocol     = host.EnvProtocol
	EnvModels       = host.EnvModels
	EnvToolRoot     = host.EnvToolRoot
	EnvAssetsDir    = host.EnvAssetsDir
	EnvBaseURL      = host.EnvBaseURL
	EnvAPIKey       = host.EnvAPIKey

	// DefaultAddr is the loopback listen address the server falls back to.
	DefaultAddr = host.DefaultAddr
)

// Options is the host configuration plus the CLI's frontend source.
type Options = host.Options

// NewServer builds the CLI's server: the host backend plus the embedded
// frontend. A host that serves its own page calls host.NewServer directly.
func NewServer(opts Options) (*server.Server, func(), error) {
	// The CLI always publishes discovery: a `dsh-go serve` on this machine is
	// something another process should be able to find.
	opts.ListenRecord = true
	captured, err := assets.Load(opts.AssetsDir)
	if err != nil {
		return nil, nil, err
	}
	opts.Frontend = captured
	return host.NewServer(opts)
}

// NewHandler builds only the HTTP handler, for the development server.
func NewHandler(opts Options) (http.Handler, func(), error) {
	srv, cleanup, err := NewServer(opts)
	if err != nil {
		return nil, nil, err
	}
	return srv.Handler(), cleanup, nil
}

// serveHelp documents every flag this command accepts.
const serveHelp = `
Usage: dsh-go [serve] [options]

Serve the DeepSeek Harness web client backed by a Go session store.

Options:
  -p, --port PORT          listen port (default 8300)
  -H, --host HOST          listen host (default 127.0.0.1)
      --home DIR           data directory for sessions, attachments, storages
                           and the listen record (default $DSH_GO_HOME or
                           ~/.dsh-go; DSH_HOME is deliberately not inherited)
      --data-dir DIR       alias for --home
      --sessions-root DIR  override only the session storage directory
                           (default <home>/sessions)
      --session-compression zstd|none|auto
                           session artifact encoding (default auto: keep an
                           existing root's encoding, otherwise zstd)
      --publish-listen-to DIR
                           also write DIR/web-listen.json so a client that
                           resolves DSH_HOME=D data dir finds this process
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
                           (default <home>/profiles/web/cordis.patch.yml,
                           falling back to ~/.dsh/profiles/web/cordis.patch.yml)
      --base-url URL       model endpoint base URL
                           (default https://api.deepseek.com/v1)
      --tool-root DIR      fallback workspace when a session has no cwd
                           (default: process working directory)
  -v, --version            print the version
  -h, --help               show this help

Environment:
  DEEPSEEK_API_KEY         API key for the model provider
  DEEPSEEK_BASE_URL        alternative OpenAI-compatible base URL
  DSH_GO_HOME              data directory (default ~/.dsh-go)
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
		String("--home,--data-dir", &opts.DataDir).
		String("--sessions-root", &opts.SessionsRoot).
		String("--session-compression", &opts.Compression).
		String("--publish-listen-to", &opts.PublishListenTo).
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

func serve(opts host.Options) {
	// Only this command line knows about the embedded capture; a host that
	// mounts the backend brings its own page.
	captured, err := assets.Load(opts.AssetsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsh-go: %v\n", err)
		os.Exit(1)
	}
	opts.Frontend = captured
	srv, cleanup, err := host.NewServer(opts)
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
