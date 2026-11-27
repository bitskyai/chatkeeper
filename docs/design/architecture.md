# Design: ChatKeeper Go architecture

Status: proposed, awaiting maintainer review. No implementation in this PR.

ChatKeeper archives chat threads that a user marks with a reaction, makes them
searchable and serves them to people (browser) and agents (REST, MCP). It
replaces the private Python prototype `threadkeep`, which is the behavioural
reference: its archive format, safety rules and test cases are ported, its code
is not.

Tracking issues

| Milestone | Issue | Summary |
|---|---|---|
| M1 | [#1](https://github.com/bitskyai/chatkeeper/issues/1) | Core, plugin interface, Slack archive (Socket Mode + polling), folder storage, viewer |
| M2 | [#2](https://github.com/bitskyai/chatkeeper/issues/2) | Web UI, first-run Slack wizard, dashboard |
| M3 | [#3](https://github.com/bitskyai/chatkeeper/issues/3) | Search (FTS5), REST API, MCP server |
| M4 | [#4](https://github.com/bitskyai/chatkeeper/issues/4) | Release pipeline, Helm/Compose, public launch checklist |
| M5 | [#1](https://github.com/bitskyai/chatkeeper/issues/1) | Cleanup (archive-then-delete) with all fail-closed gates |
| Later | [#6](https://github.com/bitskyai/chatkeeper/issues/6) | Discord plugin |
| Later | [#7](https://github.com/bitskyai/chatkeeper/issues/7) | Telegram plugin |
| Later | [#8](https://github.com/bitskyai/chatkeeper/issues/8) | Git storage backend |

## 1. Requirements

1. One app that runs on a desktop or a server.
2. All UI is in the browser.
3. First run asks which platform to connect and walks through setup.
4. Slack first. Each platform (Slack, Discord, Telegram) is a plugin.
5. Released through GitHub Releases; open source (MIT).
6. Chat only: no generic document or email archiving.

## 2. Key decisions

| # | Decision | Why |
|---|---|---|
| D1 | Go, one static binary, `CGO_ENABLED=0` everywhere. | One artifact for desktop and server, cross-compiled from one Linux runner, no runtime to install. |
| D2 | Plugins are compiled in and self-register; the contract is a versioned Go interface built only from serialisable data, so it can later be served out-of-process without changing plugins' shape. | Simplest thing that keeps a single binary. Out-of-process costs an IPC protocol, a second artifact per platform and a process supervisor before there is a third-party plugin to justify it. |
| D3 | Core has zero platform-specific code, enforced by an architecture test. | The Discord and Telegram milestones must be additive. |
| D4 | Archive files plus their sha256 manifest are the source of truth, with byte-identical schema-1 compatibility and additive schema-2 history on refresh. | Existing archives migrate by pointing the new binary at the old directory. |
| D5 | SQLite holds only derived or app-state data. Driver `modernc.org/sqlite` (pure Go). No ORM. | Deleting any database loses no archived content. Pure Go keeps D1. |
| D6 | The cleanup journal stays a fsynced JSON file beside the archive, not in SQLite. | It records which source messages were already deleted. That is not rebuildable, so it must live and be backed up with the archive. (Deviation from the initial brief; see §13.) |
| D7 | Web UI is server-rendered: `net/http`, `html/template`, `embed.FS`, htmx vendored as one static file. No npm, no build step. | About a dozen screens. A bundler would be the largest toolchain in the repo. |
| D8 | YAML is the configuration source of truth; the UI edits the same file and preserves comments. Secrets are never in YAML. | Hand editing, Helm and Compose keep working. |
| D9 | Thin in-repo Slack client instead of a Slack SDK. | The prototype's client is under 400 lines and its strict limits (size caps, no redirects, host allowlists) are the security surface. An SDK would hide them. |

## 3. Language, module, layout

- Module path: `github.com/bitskyai/chatkeeper`.
- Go version: `go.mod` declares the previous stable release (`go 1.26` when
  work starts); CI and releases build with the latest stable toolchain. The
  floor moves up when a new Go release ships. Features the design relies on:
  `net/http` method/wildcard routing (1.22), `os.Root` for path-confined file
  access (1.24), `http.CrossOriginProtection` and `testing/synctest` (1.25).
- Dependencies (direct, all pure Go):

  | Need | Choice |
  |---|---|
  | SQLite + FTS5 | `modernc.org/sqlite` |
  | YAML | `go.yaml.in/yaml/v3` (maintained successor of `gopkg.in/yaml.v3`; node API keeps comments) |
  | WebSocket (Socket Mode) | `github.com/coder/websocket` |
  | MCP | `github.com/modelcontextprotocol/go-sdk` |
  | OS keychain | `github.com/zalando/go-keyring` |
  | Password hashing | `golang.org/x/crypto/argon2` |
  | Browser e2e (test only) | `github.com/chromedp/chromedp` |

  Everything else is the standard library (`log/slog`, `database/sql`,
  `html/template`, `embed`, `flag`). No web framework, no CLI framework, no
  ORM, no JSON Schema validator (§5.3).

```
cmd/chatkeeper/            main: flag parsing, mode selection, wiring; imports plugins/all
pkg/pluginapi/             the ONLY package plugins and core share (contract v1)
pkg/pluginapi/plugintest/  contract test kit every plugin runs
internal/core/             supervisor, workspace runtime, queue, archiver, trigger rule, registry
internal/config/           YAML load/validate/write, watch, diff
internal/secrets/          env, keychain, file backends
internal/storage/          archive format, folder backend, canonical JSON, verification
internal/search/           FTS5 index (derived)
internal/web/              admin UI, viewer, REST, auth, sessions; templates + static via embed.FS
internal/mcp/              MCP tools, stdio + streamable HTTP
internal/cleanup/          archive-then-delete engine (never imported by core runtime)
internal/plugins/slack/    Slack plugin (+ slacktest/ fake server)
internal/plugins/all/      blank imports of built-in plugins, one file per plugin with a build tag
deploy/helm/chatkeeper/    deploy/compose/
docs/
```

`pkg/pluginapi` is public on purpose: it is the stable surface a future
out-of-process plugin SDK is generated from. Everything else is `internal/`.

Dependency rule (checked by the architecture test):

```
cmd/chatkeeper ──> internal/plugins/all ──> internal/plugins/<id> ──┐
      │                                                             v
      └──> internal/{core,web,mcp,cleanup,search,storage,...} ──> pkg/pluginapi
```

`internal/plugins/<id>` may import `pkg/pluginapi` and the standard library
plus its own vetted dependencies; it may not import `internal/core` or another
plugin. Nothing outside `internal/plugins/` may import a plugin or contain the
identifiers `slack`, `discord` or `telegram` (case-insensitive, checked with
`go/parser` over non-test files, plus templates and static assets).

## 4. Plugin model

| Option | For | Against | Verdict |
|---|---|---|---|
| Compile-time registry: interface + `init()` registration, build tags to include/exclude | Single static binary; type-checked; no IPC; trivial to test; works on every OS | A plugin panic or leak is in-process; third parties must fork or upstream | **MVP** |
| Out-of-process (`hashicorp/go-plugin` gRPC, or subprocess JSON-RPC over stdio) | Crash and memory isolation; third-party plugins in any language; independent releases | IPC schema to design and version; one more binary per platform per OS/arch; process supervision; secrets cross a process boundary; harder first-run story | Later, behind the same contract, when a third-party plugin exists |
| Go `plugin` package (`.so`) | Looks native | No Windows; needs cgo; plugin and host must match toolchain and every dependency version exactly; cannot unload | **Rejected** |

Mechanics:

- Each plugin package has `func init() { pluginapi.Register(Plugin{}) }`.
- `internal/plugins/all/slack.go` is `//go:build !noslack` with a blank import.
  Default builds include every built-in; `-tags noslack` (etc.) drops one.
  Registration order is irrelevant; duplicate ids panic at start-up.
- Keeping the door open for out-of-process: every type crossing the contract
  is plain data (structs, strings, byte slices, `json.RawMessage`), every call
  takes a `context.Context`, callbacks are limited to one event sink, and no
  core object is handed to a plugin. A later `pluginapi/rpc` adapter can
  implement `Plugin` by proxying to a subprocess; core does not change.
- In-process plugins are not a security boundary. Built-ins are reviewed code
  in this repo; that is the control until out-of-process exists.

## 5. Plugin interface (contract v1)

Illustrative Go, not final signatures.

### 5.1 Manifest and plugin

```go
package pluginapi

const APIVersion = 1

type Manifest struct {
    ID           string       // ^[a-z][a-z0-9_]{1,31}$
    Name         string       // "Slack"
    Version      string       // plugin version
    APIVersion   int          // contract major it targets
    Capabilities Capabilities // ceiling; a session may report fewer
    ClaimsLegacy bool         // owns archives/configs with no "platform" field (exactly one plugin)
}

type Capabilities struct {
    ReadHistory bool // can fetch a thread on demand (polling, backfill)
    Realtime    bool // can push events (Socket Mode, gateway)
    Delete      bool // can delete source messages (cleanup)
}

type Plugin interface {
    Manifest() Manifest
    ConfigSchema() json.RawMessage     // JSON Schema: workspace settings
    CredentialSchema() json.RawMessage // JSON Schema: one property per credential
    ParseWorkspace(raw json.RawMessage, creds Credentials) (Settings, error) // authoritative validation
    ValidateWorkspaces(all []Settings) error                                 // cross-workspace rules
    SetupFlow() []SetupStep            // nil = no wizard yet ("coming soon", YAML still works)
    NewSession(ws Workspace, env Env) (Session, error)
    NewCleanupDriver(ws Workspace, env Env) (CleanupDriver, error) // ErrUnsupported unless Delete
    Content() ContentHooks             // snapshot validation, render labels, emoji map, mention resolver
}
```

### 5.2 Session, hooks, lifecycle

```go
type Session interface {
    Capabilities() Capabilities
    VerifyIdentity(ctx context.Context) (Identity, error)
    Channels(ctx context.Context) ([]Channel, error)
    History(ctx context.Context, channel string, oldest string) ([]RootRef, error)
    Root(ctx context.Context, channel, ts string) (Root, error) // root message with normalised reactions
    Snapshot(ctx context.Context, channel string, root Root, prev *Snapshot) (Snapshot, error)
    Start(ctx context.Context, sink EventSink) error // realtime; blocks until ctx is done or fatal error
    Stop(ctx context.Context) error                  // must return within 10 s
    Reconfigure(ctx context.Context, ws Workspace) (applied bool, err error)
}

type CleanupDriver interface {
    Verify(ctx context.Context, m ArchiveManifest) error             // API identity + workspace boundary
    Reason(msg Message) string                                       // "" = deletable (offline policy)
    Check(ctx context.Context, m ArchiveManifest, msgs []Message, permittedMissing []string) error
    Delete(ctx context.Context, m ArchiveManifest, msg Message) error
    Pace(ctx context.Context) error
}

type Env struct { // everything core gives a plugin
    Credentials Credentials  // this workspace only
    Log         *slog.Logger // redacting handler
    HTTP        *http.Client // timeouts, redirects off, User-Agent, response size caps
    StateDir    string       // per-workspace scratch (e.g. Telegram buffer)
    Now         func() time.Time
}
```

Who owns what:

| Hook | Plugin supplies | Core keeps |
|---|---|---|
| Trigger | reactions normalised on the root as `{name, users, count}`; ID patterns; default reactions | the rule: all listed reactions present; `who: any | owner`, `owners` |
| Archive | `Root`, `History`, `Snapshot`, content validation | archiver, storage, search index update, viewer |
| Realtime | transport inside `Start`, events or cheap hints | bounded coalescing queue, startup backfill, periodic repair poll |
| Cleanup | identity check, per-message deletability, live comparison, delete call, pacing | lock, journal, archive proof, age gate, owner-trigger gate, report |

Lifecycle, per workspace:

```
registered ─> [workspace] created ─> starting ─> running ─> stopping ─> stopped
     │                        \_________ failed (backoff 5 s .. 5 min) ________/
 incompatible (api_version not supported; listed in UI with reason)
```

- Start, stop and reload are per workspace. Reload calls `Reconfigure`; if it
  reports not applied, core does stop, create, start.
- Polling runs in core for every session with `ReadHistory`; realtime is an
  accelerator. Slack without an app token runs polling only and the UI says so.
- Plugin code is never hot-swapped; upgrading means a new binary.

Panic and failure isolation:

| Failure | Containment |
|---|---|
| Panic in any plugin call | Every call site in core goes through one `guard` wrapper with `recover`. Each workspace runs in its own supervised goroutine tree. The workspace is marked failed and restarted with backoff; others keep running. |
| Plugin starts its own goroutines | The contract forbids goroutines that outlive `Start`/a call; the test kit checks for leaks. A panic in a goroutine the plugin started cannot be recovered by core and crashes the process: this is the known limit of in-process plugins and the main argument for out-of-process later. |
| Hook hangs | Every call has a context deadline; `Env.HTTP` enforces timeouts. `Stop` is abandoned after 10 s and the workspace is quarantined (not restarted) and flagged in health. |
| Event flood | Bounded queue per workspace that coalesces by `(channel, root)`. |
| `ParseWorkspace` returns an unexpected error | Config error for that workspace only. |
| Setup step fails or panics | Generic step error to the UI; details to the log, redacted. |

Health exposes error classes only, never messages that may contain payloads.

Versioning: `APIVersion` is an integer major. Core loads plugins whose
`APIVersion` is in its supported set (`{1}`). Within a major, changes are
additive: new capabilities are optional interfaces discovered by type
assertion (`interface{ BeginBackfill(...) }`), so existing plugins keep
compiling. A breaking change bumps the major; core supports N and N-1 for one
minor release.

### 5.3 Schemas

JSON Schema drives form rendering only. `ParseWorkspace` is the authoritative
validator, which avoids a schema-validation dependency and keeps one set of
error messages. The contract test kit rejects anything outside this subset:

- Types: `object`, `string`, `integer`, `number`, `boolean`, `enum`, `array`
  of string/enum; keywords `title`, `description`, `default`, `pattern`,
  `minimum`, `maximum`, `required`, `examples` (string suggestions).
- Extensions: `x-secret` (write-only field), `x-env-default` (suggested env
  var name), `x-widget` (`channel-picker`, `user-picker`),
  `x-options-from` (id of a setup step that returns choices).

### 5.4 Setup steps

```go
type SetupStep struct {
    ID, Title string
    Kind      StepKind        // instructions | form | check | choose
    Body      string          // plain text, small inline subset (bold, code, numbered list)
    Links     []Link          // opened in a new tab
    Schema    json.RawMessage // for form / choose
    Run       func(ctx context.Context, st SetupState, in json.RawMessage, env Env) (StepResult, error)
}
type StepResult struct {
    OK          bool
    FieldErrors map[string]string
    Messages    []string
    Data        json.RawMessage // merged into wizard state (never secrets)
    Choices     []Choice
}
```

Core renders steps generically and never knows what a Slack app is. The Slack
plugin owns this flow:

| Step | Kind | What happens |
|---|---|---|
| `create_app` | instructions | Link `https://api.slack.com/apps?new_app=1&manifest_yaml=<encoded>`. The manifest comes from one constant that also generates `examples/slack-app-manifest.yaml`: bot scopes `channels:history channels:read groups:history groups:read reactions:read users:read files:read`; bot events `reaction_added reaction_removed message.channels message.groups`; `socket_mode_enabled: true`. Then: install to workspace, copy the Bot User OAuth Token, create an App-Level Token with `connections:write`. |
| `credentials` | form | `bot_token` (`xoxb-`, required); `app_token` (`xapp-`, optional: without it the workspace is polling-only). |
| `validate` | check | `auth.test` returns team and bot identity; granted scopes from the `x-oauth-scopes` header are compared with the required list and each missing scope is named; `apps.connections.open` proves the app token and Socket Mode, and the Socket Mode `hello` (`connection_info.app_id`) must name the bot token's app (`auth.test` → `bots.info`). App-level tokens cannot be created by API, so the step deep-links `api.slack.com/apps/<app id>/general` and `/socket-mode`, offers copy buttons for the token name and `connections:write`, and maps each failure to its fix. Polling-only workspaces reopen this step from the dashboard (`SetupStep.Upgrade`). |
| `channels` | choose | `users.conversations` merged with `conversations.list`. "All channels the bot is invited to" (`channels: auto`) or a selection; channels the bot is not in show the `/invite` command. |
| `trigger` | form | Reactions (default `white_check_mark`; all selected reactions must be on the thread root) and who: anyone, or owners picked from `users.list`. |

## 6. Storage

### 6.1 Source of truth

```
                 source of truth                         derived / app state
   +-----------------------------------------+   +------------------------------+
   | <workspace storage>/archive/...         |   | <home>/state/app.db          |
   |   manifest.json (sha256 of every object)|──>| <home>/state/index/<ws>.db   |
   |   messages.json, users.json, files/*    |   | (rebuilt from the archive)   |
   | <workspace storage>/cleanup/*.json      |   +------------------------------+
   +-----------------------------------------+
```

If every `.db` file is deleted, ChatKeeper restarts, rebuilds the search index
and loses only sessions and event history. If the archive is lost, it is gone.
Backups therefore cover the workspace storage directories and `config.yaml`.

### 6.2 On-disk format (schema 1 compatibility, schema 2 history)

```
<storage path>/
  archive/<channel>/<root_ts>/
      manifest.json     schema, platform, team_id, channel, root_ts, message_count,
                        objects{path: {size, sha256}}, files[...]
      messages.json     raw platform messages, root first
      users.json        users referenced by the thread
      files/<id>.bin    attachments
  archive/<channel>/.<root_ts>-previous      transient, crash recovery
  cleanup/<channel>_<root_ts>.json           cleanup journal (version 1)
  cleanup/lock
  .threadkeep.lock                           writer lock (name kept; see below)
```

Rules ported as is:

- Writes are staged in a sibling temp directory, fsynced, then swapped with
  rename; the old snapshot is kept as `-previous` until the swap is durable. A
  crash leaves the old or the new snapshot, never a mix; start-up recovery
  finishes or rolls back.
- Reads verify size and sha256 of every object against the manifest;
  attachments are verified by streaming, not loaded into memory. Symlinks are
  never followed. All file access goes through an `os.Root` opened on the
  storage path.
- Limits: 100 MiB per file, 512 MiB per snapshot, 16 MiB per JSON document.
- A re-archive that only differs in ephemeral fields (rich-text block ids,
  reaction member order) is a no-op.
- A snapshot whose cleanup journal shows progress is pinned: it is not
  replaced; a fully cleaned thread is skipped.

Compatibility requirements this creates:

- **Canonical JSON.** Hashes cover the stored bytes, and cleanup proofs are the
  sha256 of `manifest.json`. The Go encoder must reproduce the prototype's
  encoding exactly: keys sorted, 2-space indent, `": "` after keys, empty
  containers as `[]`/`{}`, non-ASCII escaped as lowercase `\uXXXX` with
  surrogate pairs, no HTML escaping, Python's number formatting, one trailing
  newline (Python `json.dumps(sort_keys=True, ensure_ascii=True, indent=2)`). A
  dedicated encoder in `internal/storage` plus golden fixtures written by the
  prototype prove load, verify and re-save are byte-identical.
- **Lock file.** Both `.threadkeep.lock` and a new `.chatkeeper.lock` are
  taken while the prototype may still run against the same directory (§15);
  the old name is dropped one release after cutover. Locking is `flock` on
  Unix and `LockFileEx` on Windows behind one small interface.
- **Legacy platform.** Manifests without `platform` belong to the plugin with
  `ClaimsLegacy` (Slack). Core never hard-codes that.

#### Never-drop refresh (schema 2)

A source snapshot is a complete observation, not a replacement archive. Under
both writer locks, storage merges messages by timestamp: new messages are added,
missing messages (including a missing root) stay with `source_deleted_at` set to
the first observed absence in UTC RFC3339Nano. Repeated absence is a no-op.
A missing root means the complete archived thread is absent; transient API or
permission failures never count as deletion. Existing archived threads continue
refreshing even if their trigger reaction has been removed; initial captures
still require the trigger before and after snapshot collection.

Changes retain each prior complete source message in `versions`, with
`edited_at` (source edit timestamp when supplied, otherwise observation time),
`content`, and `sha256` of the canonical `content` bytes. Archive annotations
are excluded from that content; volatile block ids and reaction-member ordering
remain no-ops. A source reappearance clears the deletion marker and appends a
`source_events` entry with `kind: reappeared`, observation `at`, and the prior
`source_deleted_at`. Re-saving an archived snapshot preserves its markers.
Prior author metadata and attachment inventories are retained too. A conflicting
attachment hash at the same file ID fails closed, leaving the old archive intact.

Schema 1 remains readable and unchanged snapshots re-save byte-identically.
Storage writes schema 2 only when merge adds history/deletion/reappearance
fields; an archive never downgrades. `message_count` counts all archived messages;
root `reply_count` may be smaller than that archive (the archive is a superset of
source). Both schemas retain the object-hash and inventory checks. History hashes
are independently verified on read. Old schema-1 writers cannot safely update a
schema-2 archive and must be stopped before a shared-directory cutover.

Refresh has no removal operation. `Folder.RemoveArchive` is the explicit archive
removal boundary; the authenticated `DELETE /api/archives/<workspace>/<channel>/<root>`
API is currently a disabled 501 stub, with no UI. Cleanup of source messages is
separate from archive removal. Snapshot size limits still apply: exceeding them
rejects the refresh and leaves the prior archive intact.

Queue overflow records bounded dropped-key hints and schedules a full source
sweep in a reserved backfill slot (1024 ordinary keys plus one backfill). Overflow
is logged without payloads. A full sweep covers hints beyond the bounded set;
repair state and `lastFull` advance only after successful capture of all channels.
Concurrent drops keep repair state pending.

The viewer refuses a non-loopback listener without a bearer token. Loopback means
a literal loopback IP, not a DNS name. Global mention metadata is cached per
workspace for 30 seconds; the requested thread is verified on every request and
its own users take precedence. The first request after cache expiry still scans
archives; unrelated mention changes can take up to 30 seconds to appear.

### 6.3 SQLite: derived data and app state

Driver comparison:

| | `modernc.org/sqlite` | `mattn/go-sqlite3` |
|---|---|---|
| cgo | no | yes |
| Cross-compile six targets from one runner | yes | needs a C cross toolchain per target |
| FTS5 | built in | build tag |
| Speed | slower on write-heavy loads | faster |
| Verdict | **chosen**; index writes are one thread at a time and tiny | rejected: breaks D1 |

Access is `database/sql` with hand-written SQL kept in one file per store.
Migrations are numbered `.sql` files embedded with `embed.FS` and applied in
order inside a transaction, tracked by `PRAGMA user_version`; a small in-repo
runner, no migration library. `sqlc` is optional and deferred: the query count
is small and FTS5 virtual tables fit it poorly. Connections use WAL,
`busy_timeout`, `foreign_keys=ON`; one writer connection per database.

| Database | Contents | If deleted |
|---|---|---|
| `state/index/<workspace>.db` | FTS5 table `threads` (tokenizer `unicode61`), archive revision | Rebuilt from the archive on start |
| `state/app.db` | sessions, wizard sessions, recent capture events, cleanup run reports (audit copy) | Users log in again; history views start empty |

Not in SQLite: archived content, cleanup journal, configuration, secrets, the
admin password hash (a 0600 file in `state/`, so losing `app.db` cannot reopen
bootstrap).

The index lives under the app home, not in the storage directory, so a storage
directory that is also a git working tree stays clean. On start the index
compares its stored archive revision with the archive and rebuilds on mismatch.

### 6.4 Storage backends

```go
type Store interface {
    Keys(ctx) ([]Key, error)
    Read(ctx, Key) (*Snapshot, error) // verified
    Save(ctx, Key, Payload) (changed bool, err error)
    Failures() []Failure
}
```

Folder is the only backend through M5. Git ([#8](https://github.com/bitskyai/chatkeeper/issues/8))
wraps folder: commit per snapshot, optional push, private-remote check, and
the committed-object verification cleanup depends on. Recommendation for that
issue: shell out to `git` as the prototype does; `go-git` avoids the runtime
dependency but is a large surface for a narrow need.

The viewer is rendered on request from verified snapshots through
`html/template` (contextual escaping, no-script CSP) with an in-memory cache
keyed by manifest hash. The prototype's pre-rendered `site/` directory is not
ported; a static export command can be added if someone needs it.

## 7. Web UI

- One `http.Server`, one listener, `http.ServeMux` patterns:

  ```
  /                      dashboard or first-run wizard        session
  /setup/...             wizard                               session
  /settings/...          workspaces, plugins, password        session
  /view/<ws>/...         archive viewer (untrusted content)   session or bearer
  /api/...               REST (§12)                           bearer or session
  /mcp                   MCP streamable HTTP (§12)            bearer
  /healthz               liveness, no settings                open
  /static/...            embedded CSS, htmx                   open
  ```

- Pages are `html/template` files embedded with `embed.FS`. htmx is vendored
  as a single file for partial updates (step validation, health polling); the
  wizard also works as plain form posts. No WebSocket to the browser; live
  views poll.
- The JSON-Schema form renderer and the setup-step runner are server-side Go
  templates over the subset in §5.3. Adding a platform adds no template.
- CSP: admin pages `default-src 'self'`; viewer pages add `script-src 'none'`
  and `sandbox`, because archived content is untrusted and shares the origin.
- `--dev` serves templates and static files from disk for live editing.

## 8. Modes and security

Mode comes from `--mode` or `CHATKEEPER_MODE`; otherwise `server` when the
bind address is not loopback, else `desktop`.

| Area | Desktop | Server |
|---|---|---|
| Bind | `127.0.0.1` only; non-loopback refused | configured address |
| Start | `chatkeeper` with no arguments: pick port 8787 or a free one, open the browser; a second launch finds the instance lock and only opens the browser; `--no-browser` for headless | `chatkeeper run --mode server` |
| Auth | per-launch token in the opened URL, exchanged for a session cookie | admin password |
| First run | wizard immediately | password bootstrap, then wizard |

Server first run: the admin password comes from `CHATKEEPER_ADMIN_PASSWORD`
(or `_HASH`), or is set in the browser using a one-time setup code printed to
the server log. Stored as an argon2id hash, 0600.

Both modes:

- Every admin route requires a session. No unauthenticated endpoint returns
  configuration.
- Sessions: random id, HttpOnly, SameSite=Strict, Secure over TLS, 12 h idle
  timeout, dropped on password change. Login is rate limited.
- CSRF: `http.CrossOriginProtection` (Sec-Fetch-Site / Origin) plus a
  per-session token on every non-GET, plus a Host allowlist, which also stops
  DNS rebinding against the loopback UI.
- Setup lock: bootstrap works only while no admin credential exists, then
  returns 409. A server that already has workspaces but no admin credential
  (an upgraded deployment) serves no bootstrap until the password is supplied
  by env, so it cannot be claimed by the first visitor.
- The read-only bearer token (`CHATKEEPER_API_TOKEN`) protects viewer, REST
  and MCP and never grants admin access.
- Server mode confines UI-chosen storage paths to the data root. Setup steps
  call only the plugin's fixed API base (overridable in tests only).
- TLS is terminated by a reverse proxy or ingress; built-in TLS is out of
  scope.

## 9. Configuration and hot reload

- App home: `$CHATKEEPER_HOME`, else the per-OS config dir
  (`os.UserConfigDir()/chatkeeper`). Contains `config.yaml`, `secrets.env`,
  `state/`. The container sets `CHATKEEPER_HOME=/data`.
- `config.yaml` is the source of truth. The schema is the prototype's (flat
  workspace entries with `id`, `platform`, `storage`, `trigger`, `channels`,
  `poll_interval`, `cleanup`, `*_token_env`), so existing files load. Core
  parses the neutral keys and hands the whole entry to the plugin's
  `ParseWorkspace`.
- The UI writes through the YAML node API: atomic (temp file, fsync, rename),
  one `.bak`, comments and key order preserved. A UI-written file passes
  `chatkeeper check-config`.
- Read-only config (Helm ConfigMap) puts the UI in managed mode: settings are
  displayed, each screen shows the YAML to apply, everything else works.
- No config file: start in setup mode with zero workspaces.

```
UI save / SIGHUP / file watch (stat every 2 s; follows ConfigMap symlink swaps)
      -> load + validate
           invalid -> keep running config; error shown in health and UI
           valid   -> diff by workspace id
                        added    create + start
                        removed  stop (files kept), drop from viewer and search
                        changed  Reconfigure, else stop/create/start
                        same     untouched (connection kept)
```

The supervisor owns the running set; viewer, search and health are views over
it. Listener address changes need a restart and the UI says so. Secret rotation
counts as a change for the workspaces that reference it. Apply is serialised;
one workspace failing to start does not roll back the others.

## 10. Secrets

YAML names secrets, never contains them (`bot_token_env: CHATKEEPER_ACME_BOT_TOKEN`).
Resolution order per name:

```
process environment  ->  OS keychain (desktop)  ->  <home>/secrets.env (0600)
```

| Deployment | Backend |
|---|---|
| Desktop | OS keychain through `zalando/go-keyring` (macOS Keychain, Windows Credential Manager, Secret Service on Linux; all without cgo). Falls back to the file when no keychain service is available. |
| Server, single host | `secrets.env`, 0600 in a 0700 directory, `KEY=value` so Compose can share it. Refused if the mode is wider than 0600. |
| Kubernetes / Compose | Environment from a Secret or `.env`. Env wins, so the UI shows such fields as "provided by environment" and validates instead of asking for a paste. |

Never shown back: the API and UI report `{set, source}` only. Tokens are kept
out of logs (redacting `slog` handler), error text, wizard state and
`fmt` output (credential types implement `LogValuer`/`Formatter` to print
`[redacted]`). Wizard credentials stay in server memory until commit.

## 11. First-run wizard and dashboard

```
[Platform] -> [Create app] -> [Tokens] -> [Validate] -> [Channels] -> [Trigger]
    core         plugin        plugin       plugin        plugin        plugin
-> [Storage] -> [Live test] -> [Done]
     core          core         core
```

- Platform: cards from the registry. Plugins without a setup flow show
  "coming soon". There is no platform list in core or templates.
- Storage: folder; default `~/ChatKeeper/<workspace-id>` on desktop,
  `<data root>/<workspace-id>` on a server. Commit happens here: secrets
  stored, YAML written, workspace started through hot reload.
- Live test: "React with the trigger emoji on a thread". The page polls
  capture events and shows the archived thread. After 2 minutes it shows
  diagnostics (bot not in channel, polling-only delay, wrong reaction, owner
  filter). Skippable.
- "Add workspace" on the dashboard reruns the same wizard.

Dashboard: health per workspace (state, realtime or polling, last capture,
last error class, index state), recent archives, search box, settings
(workspaces, credential rotation, pause, remove; admin password; plugins;
config path and whether it is writable), cleanup (M5).

## 12. REST API and MCP

REST (bearer token or admin session; contract carried over from the prototype):

```
GET /api/search?q=&workspace=&channel=&author=&from=&to=&limit=&after=
GET /api/threads/{workspace}/{channel}/{ts}
GET /api/openapi.json
```

MCP, using the official Go SDK, read-only tools with the same semantics:

| Tool | Arguments |
|---|---|
| `search_threads` | `q`, optional `workspace`, `channel`, `author`, `from`, `to`, `limit`, `after` |
| `get_thread` | `workspace`, `channel`, `ts` |
| `list_workspaces` | none; configured IDs only |

Transports: stdio (`chatkeeper mcp`, for desktop agent clients; it opens the
archive and index read-only and does not need the server running) and
streamable HTTP at `/mcp` on the main listener (bearer token, Origin check).
REST and MCP call one `search.Service`; neither can write or delete.

M3 implementation detail: stdio opens the persistent index with SQLite
`mode=ro&readonly_shm=1` and reopens it per query to observe index replacement.
Live WAL is never opened immutable. A cold, checkpointed database can be read
immutable only while shared index writer/publication leases exclude every writer;
these existing lock files are opened read-only and no archive lock is taken.
Missing/corrupt or otherwise unusable indexes trigger one warning per workspace
and a private verified fallback bounded by time, threads, directory entries,
extracted text, and SQLite pages (see README). Fallback is an optimistic traversal:
concurrent publication may omit a thread; it never blocks capture publication.
Cross-workspace queries group by workspace ID and rank within each workspace,
avoiding comparisons of unrelated BM25 statistics. Literal CJK queries use an
indexed-body substring scan, including short Chinese words; FTS grammar retains
unicode61 semantics. See `docs/porting.md` for measurements and protocol coverage.

## 13. Cleanup (archive-then-delete)

Cleanup deletes source messages after proving they are archived. It is off by
default, per workspace, and every doubt blocks the whole thread. The engine is
`internal/cleanup`; the archive runtime never imports it (architecture test).

Gates ported from the prototype, each with its own test in M5:

| # | Gate | Blocks when |
|---|---|---|
| 1 | Enabled | `cleanup.enabled` is false (default) |
| 2 | Cleanup credential | credential missing/malformed; bot token configured to delete others (reviewed own-bot policy below) |
| 3 | Identity | live identity differs from workspace; deleting others without a user admin/owner identity |
| 4 | Workspace boundary | manifest platform or team differs from the workspace; channel not watched |
| 5 | Archive integrity | any object fails size/sha256 verification, or snapshot unreadable |
| 6 | Durability (git storage) | local objects differ from `HEAD`, branch is not `main`, remote URLs disagree, or remote `main` is not `HEAD` |
| 7 | Age | newest message is younger than `min_age_hours` (default 24): deferred |
| 8 | Owner trigger | workspace is not `who: owner` with owners, or the archived root lacks the owner's trigger reactions |
| 9 | Deletability | any message is not deletable under policy (e.g. others' messages unless `allow_others`) |
| 10 | Live comparison | live thread has messages not in the archive, content differs from the archive, history is incomplete or paginates inconsistently, or the owner trigger was removed |
| 11 | Proof pinning | `manifest.json` hash changed since the journal was created, or changes during the run |
| 12 | Journal | journal malformed, targets differ, progress not a prefix of targets |
| 13 | Lock | another cleanup holds the per-storage lock |
| 14 | Unknown outcome | a delete call times out or is ambiguous: stop, record `pending`, never assume success or failure |
| 15 | Rate limit / refusal | stop the thread; resume later from the journal |

Order of deletion is replies newest-first, root last. Before each delete: pace,
re-run gates 5, 6, 10, 11, write `pending` to the journal and fsync, delete,
then record `done` and fsync. On resume, `pending` is treated as possibly
deleted. Dry-run needs no credentials and makes no network calls.

Journal location (D6): the journal stays `cleanup/<channel>_<ts>.json` beside
the archive, same format as the prototype. Reasons: it is the only record of
irreversible actions, storage consults it to pin snapshots, it must travel
with the archive in backups and git, and in-flight journals from the prototype
must resume. SQLite gets a copy of each run report for the dashboard only.

UI flow (M5): Preview runs the dry-run and returns a report plus digest. Apply
re-runs the dry-run, refuses if the digest differs, and requires the typed
workspace id and, in server mode, password re-entry. `chatkeeper cleanup
[--apply]` and a Helm CronJob remain for unattended use.

## 14. Release and CI

- **goreleaser** on tag `v*`: `darwin`, `linux`, `windows` x `amd64`, `arm64`;
  `tar.gz` (zip on Windows); `checksums.txt`; SBOM per archive (syft); keyless
  cosign signature of the checksum file (optional in the M4 delivery scope) and GitHub build provenance
  attestation. Version, commit and date are injected with `-ldflags`; builds
  use `-trimpath` and are reproducible.
- **Container**: multi-arch (`linux/amd64`, `linux/arm64`)
  `ghcr.io/bitskyai/chatkeeper`, distroless static base, non-root,
  `CHATKEEPER_HOME=/data`, `CHATKEEPER_MODE=server`. GitHub provenance is required; cosign signing is optional in the M4 delivery scope.
- **Homebrew tap** later (goreleaser can publish it). macOS notarisation and
  Windows code signing are deferred; the README documents the Gatekeeper
  step until then (open question 2).
- **CI** on GitHub-hosted runners (the repo will be public; no self-hosted
  runners on public PRs): `go vet`, `staticcheck`, `govulncheck`,
  `go test -race` on Linux, plain `go test` on macOS and Windows, the six-way
  cross-build, gitleaks, `goreleaser check`, and a separate browser e2e job.
  Actions are pinned by commit SHA; workflow permissions default to read.
- Versioning: semver. `0.x` until the plugin contract and config schema have
  survived a second platform.

## 15. Migration from threadkeep

The existing private Helm deployment of threadkeep keeps running until parity.

| Item | threadkeep | ChatKeeper | Migration |
|---|---|---|---|
| Archives | schema 1 folders | same | none; point at the same directory |
| Cleanup journals | `cleanup/*.json` v1 | same | none |
| Search index | `search.sqlite3` in storage | `state/index/<ws>.db` | rebuilt; old file ignored, removable |
| Rendered `site/` | in storage | rendered on request | ignored, removable |
| Config | YAML | same schema | copied as is |
| Env vars | `THREADKEEP_*` | `CHATKEEPER_*` | `THREADKEEP_*` accepted with a deprecation warning for two minor releases |
| Lock file | `.threadkeep.lock` | both, then `.chatkeeper.lock` | automatic |

Cutover plan:

1. **Shadow (after M1).** Run ChatKeeper `verify` read-only against a copy of
   the production data: every snapshot must verify, and re-save must be
   byte-identical.
2. **Parallel (after M3).** Deploy ChatKeeper beside threadkeep with its own
   Slack app and its own storage, watching the same channels. Compare archives
   (manifest hashes after normalisation), search results and viewer output
   for one week.
3. **Cutover (after M4; cleanup stays on threadkeep's CronJob until M5 if it
   is in use).** Scale threadkeep to zero, back up the volume, start
   ChatKeeper on the existing volume and config, run startup backfill, check
   health and hashes. Rollback is scaling threadkeep back up: nothing on disk
   has changed format.
4. **Retire.** Remove threadkeep after two weeks without rollback; archive the
   prototype repository.

`chatkeeper migrate-from-threadkeep` (M4) automates the config and env-name
mapping and the verification, dry-run by default.

Tests: the prototype has 291 test functions (about 450 cases once
parametrised). Each milestone ports the cases for its area as table-driven Go
tests, and `docs/porting.md` maps every prototype test to its Go counterpart
or to a written reason for dropping it.

## 16. Milestones and acceptance

Full checklists are in the issues.

| Milestone | Delivers | Acceptance highlights |
|---|---|---|
| M1 [#1](https://github.com/bitskyai/chatkeeper/issues/1) | Layout, CI, `pluginapi` v1, registry, core runtime, Slack plugin (Socket Mode + polling), folder storage, viewer, YAML-only CLI | Architecture test; fake Slack server (Web API + Socket Mode) end-to-end, with and without Socket Mode; byte-identical golden archives from the prototype; crash-recovery tests; fixture-plugin isolation tests; six-target `CGO_ENABLED=0` build |
| M2 [#2](https://github.com/bitskyai/chatkeeper/issues/2) | Web UI, wizard, dashboard, modes, auth, config writer, hot reload, secrets backends | Browser e2e of the Slack wizard against the fake Slack server in desktop and server mode, including negative paths; security tests (session, CSRF, setup lock, loopback); hot-reload tests; secrets never returned |
| M3 [#3](https://github.com/bitskyai/chatkeeper/issues/3) | FTS5 search, REST, MCP stdio + streamable HTTP | Ported search/API/MCP cases; delete-index-and-rebuild equivalence; MCP client test over both transports; result parity with the prototype on a golden corpus |
| M4 [#4](https://github.com/bitskyai/chatkeeper/issues/4) | goreleaser, image, Helm, Compose, migration command, public launch checklist | Release candidate produces signed artifacts; clean-machine smoke test on three OSes; Compose and kind e2e; migration rehearsal; gitleaks clean; SECURITY.md and CONTRIBUTING.md; repo goes public last |
| M5 [#1](https://github.com/bitskyai/chatkeeper/issues/1) | Cleanup engine, Slack cleanup driver, CLI, UI preview/apply, CronJob | One test per gate in §13 proving zero delete calls; all prototype cleanup tests ported; crash injection around journal writes and delete; live gate on a throwaway workspace |
| Later [#6](https://github.com/bitskyai/chatkeeper/issues/6) | Discord plugin | Wizard e2e against a fake Discord; no core or web diff |
| Later [#7](https://github.com/bitskyai/chatkeeper/issues/7) | Telegram plugin | Wizard e2e against a fake Bot API; no core or web diff |
| Later [#8](https://github.com/bitskyai/chatkeeper/issues/8) | Git storage | Existing git archives open unchanged; cleanup durability gate |

Cleanup is last on purpose: it is the only destructive feature, it depends on
everything else being trustworthy, and the prototype can keep doing it until
then.

Test infrastructure shared across milestones:

- `internal/plugins/slack/slacktest`: an in-process fake Slack: HTTP Web API
  (`auth.test` with scope header, `users.conversations`, `conversations.list`,
  `conversations.history`, `conversations.replies`, `users.list`, `users.info`,
  file download, `apps.connections.open`, and for M5 `chat.delete`) plus a
  Socket Mode WebSocket the test drives to emit `reaction_added`. It can
  inject rate limits, truncated bodies, redirects and disconnects.
- Browser e2e: chromedp against headless Chrome, starting the real binary on
  a temp home with the Slack API base pointed at the fake.
- `pluginapi/plugintest`: manifest shape, schema subset, setup-step shape,
  goroutine-leak and panic-containment checks. Every plugin runs it.

## 17. Out of scope

- A hosted Slack app / OAuth distribution (users create their own app from the
  manifest).
- Multiple users, roles, SSO. One admin credential.
- Native app shell, tray icon, auto-update, start at login.
- Out-of-process or sandboxed plugins, a plugin marketplace.
- Built-in TLS.
- Archiving anything that is not chat.

## 18. Open questions

1. **Cleanup journal location (D6).** The brief put the cleanup journal in
   SQLite. This design keeps it as a file beside the archive because it is not
   rebuildable. Confirm, or state the reason it should move.
2. **Desktop signing.** Unsigned macOS and Windows binaries trigger Gatekeeper
   and SmartScreen warnings. Is a documented workaround acceptable for the
   first public release, or should M4 include an Apple Developer ID and
   notarisation (paid account, secrets in CI)?

### M5 implementation clarifications

The reviewed prototype own-bot policy supersedes gate 2's original user-token-only
wording: Slack bot tokens may delete only their own messages. User tokens may
also delete their own messages; deleting others requires `allow_others: true`
and a live admin/owner identity. `own_only: true` always restricts deletion to
that identity. Cleanup resolves its own credential reference only when applying;
the archive runtime never resolves cleanup credentials. A dedicated cleanup
environment reference is recommended; an own-bot credential may have the same
value as the capture token.

Folder storage is the only currently accepted storage type. Unsupported git
storage is rejected by configuration, so no unverified git durability fallback
exists. Destructive cleanup is refused on Windows while durable directory sync
is unavailable there; offline previews remain available.

The cleanup transaction holds both `cleanup/lock` and the archive writer lock
through all verification, journal persistence and mutation. Cleanup explicitly
fsyncs verified snapshot objects and their directory chain, including imported
archives, before accepting their proof. Snapshot documents
remain byte-for-byte immutable once cleanup has progress. Version-1 journals
retain `version`, `proof`, `targets`, `done` and `pending`, adding an optional
`source_deleted_at` mapping from message timestamp to deletion-observed time.
`Snapshot.SourceDeletedAt` exposes that metadata without changing source message
bytes or their sha256 manifest. Keep journals with the archive in backups.
Legacy v1 journals without timestamps remain resumable; absent timestamps are
unknown rather than invented. A completed cleanup remains pinned during refresh
and backfill, retaining every original message and object hash. To archive new
activity under an already-cleaned root, explicit manual reconciliation is
required; collectors cannot overwrite the proof.

Preview checks available author/subtype fields offline and explicitly reports
that live author-identity authorization is deferred until apply. All other
offline policy/integrity gates run even when age would defer the thread. UI
previews expire after ten minutes, bind the session, workspace configuration,
archive proofs and report, and are consumed once. Apply rechecks each proof under
the archive lock as well as repeating live comparisons. UI audit reports are a
bounded SQLite copy; journals remain the restart authority.
