# dsh-go

A Go reimplementation of the **DeepSeek Harness backend**. It serves the real
DeepSeek Harness web frontend and speaks the same browser protocol, but stores
sessions itself and talks to the model directly. The motivation is footprint:
the Node host needs hundreds of megabytes and a full workspace install, which
is too much for a 1 GB Raspberry Pi that also runs other services. `dsh-go` is
one static binary of a few tens of MiB with the frontend embedded.

The frontend is **not** forked. Assets are captured from a running DeepSeek
Harness web UI and served byte-for-byte, so the browser runs the same React
client, the same shared multiplex worker, and the same session chat page. Only
the backend is new.

## Status

A session can be listed, created (with or without a workspace), renamed,
prompted, cancelled, paged, followed live and projected, and the chat page
renders both sides of the conversation. Reasoning and answer tokens stream
live and survive reconnects.

All thirteen DSH model tools are implemented with DSH-compatible definitions
and results: `bash`, `read`, `write`, `edit`, `glob`, `grep`, `todo_write`,
`job_output`, `job_list`, `job_kill`, `read_image`, `web_search` and
`web_fetch`. Commands can run in the background, image reads use the same
content-addressed attachment store and can be sent to vision models, and web
tools return the same trust notice, source list and truncation contracts.
Oversized grep results are spilled under the session directory so the model can
page through them.

Deliberately out of scope for now: subagents, the terminal and PTY tools, queue
steering, federation, and the Node host's zstd compression and cross-process
log locking. Background jobs are process-local and do not survive a restart.
Foreground `bash` calls do not yet promote to a background job at their
timeout; the command is stopped and the timeout marker is returned.

## Tools and permissions

Tool calls resolve relative paths against the session workspace. Each session
gets the permission and sandbox modes recorded in its header:

- `danger-full-access` (the default) allows reads and writes anywhere the
  process can reach.
- `workspace-write` allows reads anywhere, but confines `write` and `edit`
  mutations to the session workspace. Symlinks are resolved before the check.
- `read-only` rejects file mutations.

`write` and `edit` require the file to have been observed first, either by
`read`, `read_image`, or an earlier successful write/edit in the same session.
The observation memory is in-process and resets when the server restarts.

Approval prompts are not implemented; the approval policy is accepted and
recorded, and mutations run without an interactive approval step. Shell
commands run with the harness process's privileges: the file policy does not
confine what a `bash` command itself writes with its own tools.

Like DSH, `bash` children do not inherit credential-shaped variables
(`KEY`, `PASSWORD`, `SECRET`, `TOKEN`, case-insensitive) or ambient `DSH_*`
variables. They receive the managed facts `DSH_HOME` (`~/.dsh-go`),
`DSH_SHELL=1`, and `DSH_SESSION_ID`.

`bash` with `run_in_background` registers the process in the in-memory job
registry and returns its id immediately. `job_output` reads output produced
since the previous read (with an optional wait), `job_list` lists the session's
jobs, and `job_kill` requests cancellation. A completion notice is appended to
a running turn, or wakes an idle session, unless the job was already awaited or
killed by the model. The limit is ten concurrent jobs per session.

`read_image` accepts PNG, JPEG, WebP and GIF files (including extension-less
files whose magic bytes identify them), downscales oversized images, stores
them under `~/.dsh-go/attachments/v1` by SHA-256, and returns both a text
envelope and an image block for the model. The same attachment bytes are
available to the UI through `session/attachment`. Unlike DSH, the Go reader
passes bytes through unchanged when no downscale is needed, so EXIF
orientation, animation and 16-bit PNG depth are left to the model provider.

`web_search` and `web_fetch` treat external content as untrusted data.
Searching uses the DeepSeek Search API when `DEEPSEEK_SEARCH_BASE_URL` is set
(authorization reuses `DEEPSEEK_API_KEY`) and otherwise falls back to
DuckDuckGo's HTML endpoint. Fetching rejects non-public addresses, follows
same-origin redirects only, converts HTML to Markdown, and caps the response
body and final text.

## Requirements

- Go 1.25 or newer.
- `DEEPSEEK_API_KEY` for real prompts. Any OpenAI-compatible endpoint works
  through `--base-url`; `--protocol anthropic-messages` selects the Anthropic
  Messages wire format for endpoints that only expose that API.

## Quick start

```sh
go run ./script/sync-assets     # capture the frontend from a running DSH web UI
go run ./script/build           # build dist/dsh-go with the capture embedded
export DEEPSEEK_API_KEY=sk-...
./dist/dsh-go serve             # http://127.0.0.1:3070/
```

`go run ./script/install` does the build and copies the binary to
`~/.local/bin/dsh-go`. A bare `dsh-go` is the same as `dsh-go serve`; `dsh-go
version` prints the build version.

`script/install` honors `INSTALL_TO_DIR`, `INSTALL_GOOS`, and `INSTALL_GOARCH`,
so a Linux/arm64 build for a remote host needs no extra tooling:

```sh
INSTALL_GOOS=linux INSTALL_GOARCH=arm64 INSTALL_TO_DIR=/tmp/dsh-go-arm64 \
  go run ./script/install
```

When only a local `llm-proxy` is reachable and it exposes Anthropic Messages:

```sh
DSH_GO_PROTOCOL=anthropic-messages \
DEEPSEEK_BASE_URL=http://127.0.0.1:8890 \
DSH_GO_MODEL=deepseek/deepseek-v4.1-flash \
DSH_GO_PROVIDER=commandcode \
./dist/dsh-go serve
```

## Development

A running DeepSeek Harness web UI is the source of truth for the frontend and a
useful reference backend. Point the dev server at it:

```sh
# terminal 1: the real DSH web UI (asset source), default http://127.0.0.1:3080
# terminal 2:
go run ./script/dev
```

`script/dev` builds and hot-reloads the Go backend on port **3070** and proxies
everything except `/api/remote.mux` to the frontend upstream. That means the
browser loads real assets from `:3080` while all protocol traffic is answered
by Go. Override the upstream with `DSH_GO_DEV_FRONTEND`.

Hot reload uses [Air](https://github.com/air-verse/air), so `air` and a Go
toolchain matching `go.mod` must be on `PATH`. `--no-use-air` runs the backend
in-process instead (no rebuild on save):

```sh
go run ./script/dev --no-use-air --no-open
```

`go run ./script/sync-assets` re-captures the frontend into `assets/dist` when
the upstream revision changes. The capture is gitignored; `assets/dist/.gitkeep`
keeps the embed directive valid on a fresh clone, and the server shows a setup
page instead of the UI until a capture exists.

## Layout

```
cmd/dsh-go          thin main
run                 CLI, configuration, wiring
server              HTTP: auth, assets, WebSocket mux, dev proxy
mux                 multiplexed WebSocket transport (streams, heartbeats)
api                 browser-facing endpoints: sessions, workspace, models, drafts
agent               one model turn written as DSH session events
llm                 model client: OpenAI chat or Anthropic Messages
session             DSH v4 JSONL event log: store, paging, projections
workspace           in-memory workspace registry
assets              embedded frontend capture and URI lookup
build               shared build pipeline and version stamping
script/             sync-assets, dev, build, install
testdata/           two real captured session logs used by tests
```

## Browser protocol

The client opens one WebSocket at `/api/remote.mux` and multiplexes logical
streams over it:

- open a stream with `{type:"open",streamId,endpoint,payload}`, then receive
  `item` values followed by `end`, or an `error` frame.
- unary calls are a stream to `$invoke` with `{endpoint,payload}`; the single
  item is `{ok:true,value}` or `{ok:false,error:{code,message,details}}`.
- `$events` announces `{type:"ready",clientId,host:{home}}` and then stays quiet
  for the life of the connection.

Endpoint names are `<namespace>/<method>`, for example `session/follow`. Args
are serialized by parameter name, which is why session methods expect
`{args:{"request":{...}}}` while `session/list` expects `{args:{"_request":{}}}`.

Implemented endpoints:

- `session/list`, `create`, `rename`, `page`, `follow`, `control`, `prompt`,
  `cancel`, `projections`, `modelCatalog`, `selectModel`, `getDraft`, `setDraft`,
  `attachment`
- `workspace/follow`, `initializeDefault`, `create`
- `$events`, `$invoke`

Anything else returns a structured `unimplemented` error and ends the stream;
the UI stays usable because those calls are optional for the chat page.

## Data

Sessions live under `~/.dsh-go/sessions` unless `--sessions-root` or
`DSH_GO_SESSIONS_ROOT` says otherwise, in the DSH v4 layout:

```
<root>/<cwd-slug>/session-<uuid>/session.v4.jsonl
```

The first line is the session header, every later line is one event. Unlike the
Node host the log is plain JSONL, not zstd, and there is no lock file; one
process owns a session. Attachments are stored once under
`~/.dsh-go/attachments/v1/objects/<sha256-prefix>/<sha256>` and referenced from
session events by `sha256:<hex>` ids. The event vocabulary and wire shapes match DSH v4 so
the existing frontend can project and render them. Session ids are bare UUIDs
inside the header; the directory adds the `session-` prefix.

Workspaces are in memory only. They are keyed by absolute path (`ws-<hash>`),
created by `workspace/create` or the default first-use flow, and their session
membership is computed from each session's `cwd` when the follow stream emits a
baseline. Restarting loses the registry but not the sessions; on the next start
the workspace is recreated as sessions are attached.

## Configuration

Flags (also see `dsh-go serve -h`):

| flag | default | meaning |
| --- | --- | --- |
| `-p, --port` | `3070` | listen port |
| `-H, --host` | `127.0.0.1` | listen host |
| `--sessions-root` | `~/.dsh-go/sessions` | where logs are written |
| `--assets-dir` | embedded capture | serve a capture from disk |
| `--token` | none | require a token; `/?token=…` sets the cookie |
| `--proxy-to` | none | forward non-API requests to a web UI |
| `--provider` / `--model` / `--reasoning` | `deepseek` / `deepseek-chat` / unset | model selection |
| `--protocol` | `openai-chat` | wire format: `openai-chat` or `anthropic-messages` |
| `--base-url` | `https://api.deepseek.com/v1` | model endpoint base URL |
| `--tool-root` | process cwd | fallback workspace and startup validation root for tools |

Environment: `DEEPSEEK_API_KEY`, `DEEPSEEK_BASE_URL`,
`DEEPSEEK_SEARCH_BASE_URL`, `DSH_GO_SESSIONS_ROOT`, `DSH_GO_TOKEN`,
`DSH_GO_PROVIDER`, `DSH_GO_MODEL`, `DSH_GO_REASONING_EFFORT`,
`DSH_GO_PROTOCOL`, `DSH_GO_ASSETS_DIR`, `DSH_GO_TOOL_ROOT`.

By default the server binds loopback and has no authentication. `--token`
mints a signed cookie from `GET /?token=…`; WebSocket clients may also pass
`?token=` on `/api/remote.mux` directly.

## Tests

```sh
go test ./...
go test -race ./...
```

Every package has tests. They use an in-process fake model server and a
temporary session store, so no API key or network access is needed.
`testdata/sessions` contains two logs captured from a real DSH host to keep the
reader honest about the real event vocabulary.
