> Deepseek-harness says: everything is a plugin. And this project adds that: even server is also a plugin.

# dsh-go

A Go reimplementation of the **DeepSeek Harness backend**. It serves the real
DeepSeek Harness web frontend and speaks the same browser protocol, but stores
sessions itself and talks to the model directly. The motivation is footprint:
the Node host needs hundreds of megabytes and a full workspace install, which
is too much for a 1 GB Raspberry Pi that also runs other services. `dsh-go` is
one static binary of a few tens of MiB with the frontend embedded.

The frontend is **not** forked. `go run ./script/build` resolves a DeepSeek
Harness checkout, builds it if needed, serves it once, captures every response
the page needs, and embeds that capture, so the browser runs the same React
client, the same shared multiplex worker, and the same session chat page. Only
the backend is new, and `serve` needs no Node process and no proxy.

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

Sessions are stored in the DeepSeek Harness v4 layout, byte-compatible with the
Node host: checksummed Zstandard frames by default, `session-<uuid>` identities,
a `session.lock` write lease, and the same header and event envelopes. A root
written here lists and reads on a real DSH host, and a real DSH root lists and
reads here. Out-of-process clients drive the backend over `POST /api/<method>`
with the same request envelopes the page uses, and discovery goes through
`<data-dir>/web-listen.json`. The full SDK surface needed by the `agent-pro`
client is implemented, including `session/inject`, `session/edit`,
`session/fork`, `session/status`, `session/refreshModelCatalog` and
`session/selectDefaultModel`.

Deliberately out of scope for now: subagents, the terminal and PTY tools, queue
steering, federation, compaction, and scheduled tasks. Background jobs are
process-local and do not survive a restart. Foreground `bash` calls do not yet
promote to a background job at their timeout; the command is stopped and the
timeout marker is returned.

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
variables. They receive the managed facts `DSH_HOME` (the data directory),
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
go run ./script/build           # stage the frontend, then build dist/dsh-go
export DEEPSEEK_API_KEY=sk-...
./dist/dsh-go serve             # http://127.0.0.1:8300/
```

`go run ./script/build` needs Node and pnpm only when it has to build the
DeepSeek Harness checkout; a checkout that is already built is served once and
captured. Nothing has to be running on port 3080, and the resulting binary
serves the real UI including the SharedWorker bundle, so `--proxy-to` is never
needed in production. `go run ./script/install` does the same and then installs
the binary. A bare `dsh-go` is the same as `dsh-go serve`; `dsh-go version`
prints the build version.

### Frontend source

The frontend is not an npm dependency: it is built from a git checkout. The
first match wins, and the chosen path is printed before the build.

1. `DSH_ROOT` — a local checkout (must exist).
2. `.deepseek-harness.json` `local` — a path relative to the file.
3. `.deepseek-harness.json` `repo` + `branch` — cloned into
   `~/.cache/dsh-go/deepseek-harness/<repo>/<branch>` and refreshed with a
   shallow fetch on each build.
4. The official `https://github.com/deepseek-ai/deepseek-harness` `main`, in the
   same cache.

`.deepseek-harness.json` is gitignored; `.deepseek-harness.example.json` is
committed. Copy it to opt in:

```sh
cp .deepseek-harness.example.json .deepseek-harness.json
```

A local checkout:

```json
{ "local": "../deepseek-harness" }
```

A fork branch:

```json
{ "repo": "https://github.com/xhd2015/deepseek-harness", "branch": "feat/x" }
```

If this tree is a linked git worktree without its own file, the main
checkout's copy is used and the build says so; `local` paths resolve relative to
the file that was found.

Useful flags:

```sh
go run ./script/build --force-frontend   # rebuild and recapture
go run ./script/build --source http://127.0.0.1:3080 --token T
                                         # capture from a running server instead
go run ./script/build --allow-missing-assets
                                         # backend-only build, with a warning
```

`go run ./script/install` stages the frontend the same way (`--force-bundle`,
`--skip-frontend`, `--source`, `--allow-missing-assets`), then installs through
`gotool/localbin/install`: an existing `dsh-go` on `PATH` is overwritten,
otherwise `~/.local/bin/dsh-go` is used; other existing copies under
`~/.local/bin`, `$GOBIN` and `$GOPATH/bin` are refreshed, `~/.local/bin` is
added to `PATH`, and the binary is ad-hoc codesigned on macOS. A failed
codesign is a `warning:`, not an error.

`script/install` honors `INSTALL_TO_DIR`, `INSTALL_GOOS`, and `INSTALL_GOARCH`,
so a Linux/arm64 build for a remote host needs no extra tooling:

```sh
INSTALL_GOOS=linux INSTALL_GOARCH=arm64 INSTALL_TO_DIR=/tmp/dsh-go-arm64 \
  go run ./script/install
```

### Embedding the backend

`host` wires the same backend `dsh-go serve` runs — session store, workspaces,
drafts, model catalog, turn runner, HTTP surface — without a frontend and
without a command line. Another Go program can mount it:

```go
handler, cleanup, err := host.Mount(host.Options{
    DataDir: "/path/to/data",   // default ~/.dsh-go
    Token:   "secret",          // empty disables auth
    Stderr:  os.Stderr,
})
defer cleanup()
```

`host.Mount` never loads the embedded capture, and `host` does not depend on
the `assets` package at all, so the importing binary does not grow by the
18 MiB frontend. The page then answers 503 with a setup hint; the `/api`
surface is complete. `run.NewServer` is the CLI's own entry: it loads the
capture and delegates to `host`.

The default port is **8300** — distinct from the DeepSeek Harness Web UI on
3080, so both can run at once.

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

`script/dev` builds and hot-reloads the Go backend on port **8300** and proxies
everything except `/api/remote.mux` to the frontend upstream. That means the
browser loads real assets from `:3080` while all protocol traffic is answered
by Go. Override the upstream with `DSH_GO_DEV_FRONTEND`. `--proxy-to` exists for
this loop only: a production `serve` answers the page from its embedded
capture.

Hot reload uses [Air](https://github.com/air-verse/air), so `air` and a Go
toolchain matching `go.mod` must be on `PATH`. `--no-use-air` runs the backend
in-process instead (no rebuild on save):

```sh
go run ./script/dev --no-use-air --no-open
```

`go run ./script/build` re-captures the frontend into `assets/dist` when a
build runs; it skips the work while a complete capture is staged and
`--force-frontend` overrides that. `go run ./script/sync-assets` is the manual
form, for capturing from a server you are already running:

```sh
go run ./script/sync-assets                 # http://127.0.0.1:3080
go run ./script/sync-assets --source URL --token T
```

The capture is written atomically, so a failed capture leaves the previous one
intact, and it is rejected unless the source served
`/api/remote.shared-mux-worker.js`, because a page captured without it would
never connect. `assets/dist` is gitignored; `assets/dist/.gitkeep` keeps the
embed directive valid on a fresh clone, and the server warns when no frontend is
embedded.

## Layout

```
cmd/dsh-go          thin main
run                 CLI: flags, embedded frontend, serve
host                the backend as a library (no frontend, no CLI);
                    what eluc mounts
server              HTTP: auth, assets, WebSocket mux, /api RPC bridge,
                    listen record, dev proxy
mux                 multiplexed WebSocket transport (streams, heartbeats)
api                 browser-facing endpoints: sessions, workspace, models, drafts
agent               one model turn written as DSH session events
llm                 model client: OpenAI chat or Anthropic Messages
session             DSH v4 event log: paths, zstd frames, generations,
                    write lease, store, paging, projections
workspace           durable workspace unit (<home>/storages/workspace.json)
assets              embedded frontend capture and URI lookup
build               shared build pipeline and version stamping
script/             sync-assets, dev, build, install
dshsource           DeepSeek Harness checkout lookup, build and one-shot serve
frontend            frontend capture and staging into assets/dist
testdata/           two real captured session logs used by tests
compat/             separate module: agent-pro reads our logs, its client
                    drives our HTTP bridge
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
  `cancel`, `projections`, `modelCatalog`, `selectModel`, `refreshModelCatalog`,
  `selectDefaultModel`, `status`, `inject`, `edit`, `fork`, `getDraft`,
  `setDraft`, `attachment`
- `workspace/follow`, `initializeDefault`, `create`
- `$events`, `$invoke`

Unary calls are also served as plain HTTP, for clients that are not a browser
page. Only a **POST** under `/api/` is a Remote call; a GET there is a static or
Host-provided route (`/api/remote.shared-mux-worker.js`, the export and present
routes, file uploads), so it is served from the capture or forwarded by
`--proxy-to` like any other page asset. The call itself is `POST /api/<method>`
with `{"type":"client-request","rpcId","method","payload":{"args":{...}}}` and an
`{"type":"server-response","rpcId","result":{"ok",value|error}}` reply. Requests
must be `application/json`; `method` must match the path; the launch token is
accepted as `Authorization: Bearer`. Streams stay on `/api/remote.mux`.

One Host route is implemented here rather than left to the capture, because it
is how the page shows a screenshot at all: **`GET`/`HEAD /api/file?path=…`**
reads one absolute path and answers its bytes, `FS_NOT_FOUND` (`404`),
`FS_NOT_REGULAR_FILE` (`403`), `FS_PERMISSION_DENIED` (`403`), `FS_TOO_LARGE`
(`413`) or `FS_BAD_PATH` (`400`) when it cannot. The failure body *is* that code
and carries no `Content-Length` of its own, so a client can always read it: a
length taken from the `stat` would describe bytes that a denied or vanished file
never produces, and Go drops a body shorter than its declared length. HEAD
reaches the same verdict as the GET, denial included, so a probe cannot report
an unreadable file as ready.
Like the Harness, it redirects a request without the current
`?v=` to the versioned URL (`private, no-store`), and the versioned answer is
`immutable` for a year; `HEAD` reports the size without the body. Authentication
matches the mux, and the byte bound defaults to the Harness image limit. The
sandbox confines writes, not reads, so the route reaches any path the process
can open — a session's screenshots routinely live outside its checkout. A GET
under `/api/` that no route claims answers `501 ROUTE_NOT_IMPLEMENTED: <path>`
instead of the setup page, so a missing route is never read as a broken
deployment or a deleted file.

Anything else returns a structured `unimplemented` error and ends the stream;
the UI stays usable because those calls are optional for the chat page.

## Data

Everything lives under one **data directory**, `~/.dsh-go` by default. It is
deliberately separate from the DeepSeek Harness home (`~/.dsh`): `DSH_HOME` is
never inherited, and pointing this backend at a Harness home requires saying so
with `--home ~/.dsh`. The layout inside it is identical to the Node host's:

```
<home>/sessions/<project-key>/session-<uuid>/session.v4.jsonl.zstd
<home>/sessions/<project-key>/session-<uuid>/session.lock
<home>/attachments/v1/objects/<sha256-prefix>/<sha256>
<home>/storages/session_composer_drafts/drafts/<session-id>.json
<home>/storages/workspace.json
<home>/web-listen.json
```

The first Zstandard frame is exactly the session header line; every later frame
is one append batch of event lines, checksummed the way DSH writes them. A root
that already holds plain `session.v4.jsonl` artifacts keeps that encoding, and
one root never mixes the two. `session.lock` is a kernel write lease held for
the life of the writing handle, so a second writer is refused with
`session/already-owned` instead of tearing the log; readers never take it.

`project-key` follows DSH's rule: separators collapse to one `-`, unsafe code
units escape as `~XXXX`, leading dashes are stripped, and the slug is bounded.
A session id always starts with `session-`; a log written before that rule keeps
its bare id on read and is repaired on the next write. Artifacts, records and
the lease file are `0600`, directories `0700`.

Workspaces are durable in `<home>/storages/workspace.json`, the same versioned
unit the Host writes (name `workspace`, version 2, opaque UUID ids), and each
session's membership is recomputed from its `cwd`. Composer drafts are durable
in `storages/session_composer_drafts`, so text saved without submitting survives
a restart and is readable by another Harness process.

## Configuration

Flags (also see `dsh-go serve -h`):

| flag | default | meaning |
| --- | --- | --- |
| `-p, --port` | `8300` | listen port |
| `-H, --host` | `127.0.0.1` | listen host |
| `--home`, `--data-dir` | `$DSH_GO_HOME` or `~/.dsh-go` | data directory |
| `--sessions-root` | `<home>/sessions` | override only the session root |
| `--session-compression` | `auto` | `zstd`, `none`, or keep the existing root |
| `--publish-listen-to` | none | also write `<dir>/web-listen.json` |
| `--assets-dir` | embedded capture | serve a capture from disk |
| `--token` | none | require a token; `/?token=…` sets the cookie |
| `--proxy-to` | none | development only: forward non-API requests to a live UI |
| `--provider` / `--model` / `--reasoning` | `deepseek` / `deepseek-chat` / unset | model selection |
| `--protocol` | `openai-chat` | wire format: `openai-chat` or `anthropic-messages` |
| `--base-url` | `https://api.deepseek.com/v1` | model endpoint base URL |
| `--tool-root` | process cwd | fallback workspace and startup validation root for tools |

Environment: `DEEPSEEK_API_KEY`, `DEEPSEEK_BASE_URL`,
`DEEPSEEK_SEARCH_BASE_URL`, `DSH_GO_HOME`, `DSH_GO_SESSIONS_ROOT`,
`DSH_GO_TOKEN`, `DSH_GO_PROVIDER`, `DSH_GO_MODEL`,
`DSH_GO_REASONING_EFFORT`, `DSH_GO_PROTOCOL`, `DSH_GO_ASSETS_DIR`,
`DSH_GO_TOOL_ROOT`. `DSH_HOME` is not read.

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

`compat/` is a separate module that imports `agent-pro` and proves the contract
from the other side: agent-pro's reader lists and inspects sessions this backend
writes, and agent-pro's own web client creates, drafts, selects a model on and
queries a session served by this backend.

```sh
go test ./...
go test -race ./...
(cd compat && go test ./...)
```
