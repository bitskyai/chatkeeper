# ChatKeeper

Keep chat threads that matter. Mark a thread with a reaction and ChatKeeper saves
its messages and attachments in a verified, portable archive. Search in your
browser or give an agent read-only access through REST or MCP.

One static Go binary runs on your desktop or server. Slack capture is available;
Discord and Telegram are planned. Capture never deletes source messages. MIT licensed.

![Desktop setup](docs/screenshots/desktop-platforms.png)
![Archived thread viewer](docs/screenshots/desktop-viewer.png)

## Install

### Homebrew (macOS and Linux)

```sh
brew install bitskyai/tap/chatkeeper            # CLI + local server
brew install --cask bitskyai/tap/chatkeeper-app # macOS menu-bar app (bundles the server)
```

Run `chatkeeper` to open the local setup wizard, or keep it running in the
background with `brew services start chatkeeper` and run `chatkeeper` again to
open the browser UI of the running instance. The tap is
[bitskyai/homebrew-tap](https://github.com/bitskyai/homebrew-tap).

### Direct download

Download the archive for your OS and CPU from
[GitHub Releases](https://github.com/bitskyai/chatkeeper/releases), extract it and run
the binary. Your browser opens the local setup wizard: connect Slack, select
channels, choose root reactions, and test capture.

| OS | Archive | Run |
| --- | --- | --- |
| macOS, Apple silicon | `chatkeeper_VERSION_darwin_arm64.tar.gz` | `./chatkeeper` |
| macOS, Intel | `chatkeeper_VERSION_darwin_amd64.tar.gz` | `./chatkeeper` |
| Linux x86-64 / arm64 | `chatkeeper_VERSION_linux_amd64.tar.gz` / `_linux_arm64` | `./chatkeeper` |
| Windows x86-64 / arm64 | `chatkeeper_VERSION_windows_amd64.zip` / `_windows_arm64` | `.\chatkeeper.exe` |

Use `./chatkeeper --no-browser` to print the launch URL. The desktop listener is
loopback-only. macOS binaries are not notarized and Windows binaries are not
Authenticode signed yet. After verifying the download, use your OS's explicit
approval flow for an unsigned app. On macOS, if necessary:
`xattr -d com.apple.quarantine ./chatkeeper`.

### macOS menu-bar app

`ChatKeeper-VERSION.dmg` on each release (or the `chatkeeper-app` cask above) is a
universal (Apple silicon + Intel) menu-bar app for macOS 13+. It runs the bundled
server in the background and shows its status (running, capturing, paused, needs
attention). From the menu you can open ChatKeeper in the browser, jump to the five
most recent archives, pause or resume capture, start at login, and check for updates.
It shares the CLI's data folder and instance lock, so if `chatkeeper` or
`brew services` is already running the app attaches to it.

v0.1 is **not signed with an Apple Developer ID**. After dragging it to
Applications, right-click (Control-click) ChatKeeper → **Open** → **Open** once. On
macOS 15+, use System Settings → Privacy & Security → **Open Anyway**. Or run
`xattr -dr com.apple.quarantine /Applications/ChatKeeper.app`. The Homebrew cask
does this for you. Building and signing details: [desktop/macos](desktop/macos/README.md).

Before running a download, verify its checksum and GitHub provenance:

```sh
# Download SHA256SUMS with your archive; check the matching line:
sha256sum --ignore-missing --check SHA256SUMS   # macOS: shasum -a 256 -c SHA256SUMS
# GitHub CLI verifies the artifact against the repository's attestation:
gh attestation verify chatkeeper_VERSION_linux_amd64.tar.gz --repo bitskyai/chatkeeper
```

Releases include six archives, SHA256SUMS and per-archive Syft SBOMs. GitHub build
attestations cover archives, SBOMs and checksums. The image has BuildKit SBOM and
provenance plus a GitHub attestation. Keyless cosign signing is optional and is
not enabled; do not expect `.sig` files.

## Server: Docker Compose or Helm

Server mode supports administrator sign-in and a read-only API bearer. Put network
access behind a TLS reverse proxy and restrict inbound access.

```sh
cd deploy/compose
cp .env.example .env
# Edit .env: set a unique administrator password (12+ characters), image version,
# allowed hosts, and CHATKEEPER_SECURE_COOKIES=1 when using HTTPS.
docker compose up -d
```

Open `http://localhost:8787/login`. Compose binds loopback by default and persists
configuration, secrets, indexes and archives in a named volume. Pin a released
`v*` image tag for production. A configured admin password is reapplied at every
restart; rotate the environment value when rotating that password. To use a
one-time setup code instead, omit the password environment entry, read the code
from container logs, and visit `/bootstrap`.

```sh
kubectl create secret generic chatkeeper --from-literal=admin-password='YOUR-LONG-RANDOM-PASSWORD'
helm upgrade --install chatkeeper deploy/helm/chatkeeper --set image.tag=vVERSION
kubectl port-forward service/chatkeeper 8787:8787
```

The chart deploys a single writer with Recreate updates, a PVC, ClusterIP service,
non-root security context and `/healthz` startup/readiness/liveness probes. Set
`existingSecret`, `envFromSecrets` (platform credentials), `persistence.existingClaim`,
`persistence.storageClass` and `resources` as needed. It creates no Ingress or
credential Secret. Use a TLS ingress/proxy and an explicit Host allowlist for
remote access (for example `--set allowedHosts=chatkeeper.example.com --set secureCookies=true`; then use the HTTPS hostname instead of port forwarding). The app creates its private home at `/data/chatkeeper` on the PVC. `/healthz` reports process availability, not workspace capture health.
Back up the volume before upgrades; keep only one writer per archive.

To build from source, use Go 1.26+: `go build -trimpath -o chatkeeper ./cmd/chatkeeper`.
To build a container: `docker build -t chatkeeper:local .` (BuildKit required).

## Slack app setup

The wizard guides these steps and checks credentials before saving a workspace:

1. Create a Slack app in your workspace. Add bot scopes `channels:history`,
   `channels:read`, `groups:history`, `groups:read`, `reactions:read`, `users:read`,
   and `files:read`, then install it and copy the bot token.
2. Optionally enable Socket Mode and generate an app token with `connections:write`.
   Subscribe bot events to `reaction_added`, `reaction_removed`, `message.channels`
   and `message.groups`. Polling also runs to repair gaps.
3. Invite the bot to each watched channel, including private channels. Paste
   credentials into the wizard, select channels, reaction names and owner rules.
4. Put all selected reactions on a thread's root. An owner rule requires the
   same selected owner to supply all reactions. Removing a trigger stops future
   refreshes and keeps the existing archive.

For managed YAML, copy [examples/config.yaml](examples/config.yaml) and replace
synthetic IDs and credential environment names. Storage paths resolve relative to
that YAML. Server paths are confined to `<app home>/data`. Read-only managed YAML
can be exported from the wizard and applied externally.

## Search and agents

Browser search supports SQLite FTS5 phrases, prefixes, boolean operators and
`NEAR`; literal CJK text uses substring matching. Prior edits and messages deleted
at source remain searchable. The archive viewer shows those annotations.

Claude Code stdio MCP configuration (`.mcp.json`, use an absolute binary path):

```json
{"mcpServers":{"chatkeeper":{"command":"/absolute/path/chatkeeper","args":["mcp","--config","/absolute/path/config.yaml"],"env":{"CHATKEEPER_HOME":"/absolute/path/app-home"}}}}
```

Codex MCP configuration (`~/.codex/config.toml`):

```toml
[mcp_servers.chatkeeper]
command = "/absolute/path/chatkeeper"
args = ["mcp", "--config", "/absolute/path/config.yaml"]
env = { CHATKEEPER_HOME = "/absolute/path/app-home" }
```

Tools: `search_threads`, `get_thread`, `list_workspaces`. Stdio reads local archives
without source credentials and emits JSON-RPC only on stdout. Use the same home
as the running app to share its index. REST offers `GET /api/search`,
`GET /api/threads/{workspace}/{channel}/{ts}` and `GET /api/openapi.json`.
HTTP MCP is `/mcp` and always requires a configured read-only bearer:
`Authorization: Bearer <CHATKEEPER_API_TOKEN>`. The bearer grants all configured
workspaces, never administrator access. HTTP MCP uses stateless JSON responses.

## Privacy and security

Your archive stays on your filesystem; there is no hosted ChatKeeper service.
Source APIs receive normal capture requests. ChatKeeper stores messages,
attachments and history in plaintext files; use disk encryption and protected
backups. Agents with local filesystem or API access can read archived content.

Desktop credentials use the OS keychain when available, otherwise a private
`secrets.env` file. Environment values take precedence. Server credentials use
environment variables or that file. Secrets are not stored in workspace YAML.
App home defaults to the OS configuration directory's `chatkeeper` folder;
`CHATKEEPER_HOME` overrides it. Desktop archives default to `~/ChatKeeper/<workspace>`.
Browser sessions expire after 12 hours. Server setup uses a one-time code or
`CHATKEEPER_ADMIN_PASSWORD`; set secure cookies when terminating HTTPS at a proxy.
Forwarded headers are not trusted. Compiled plugins are trusted in-process code.

Archive files and SHA-256 manifests are authoritative. Search indexes are derived
and rebuildable. Back up archives, YAML, application state and cleanup journals;
store credential backups separately. On Windows, directory fsync is unavailable,
so power-loss durability depends on filesystem behavior. Read the
[security policy](SECURITY.md) for private vulnerability reporting.

## Commands and development

`chatkeeper --help` lists commands and flags. Common maintenance:

```sh
chatkeeper check-config --offline --config config.yaml
chatkeeper backfill --config config.yaml
chatkeeper verify --config config.yaml
chatkeeper version
```

`verify` checks snapshots without source calls and fails on corruption. It may
create locks or recover an interrupted rename; use a copy for migration rehearsals.
`run` reloads valid YAML and credential changes; listener changes need a restart.
Workspace removal keeps archives. See [CONTRIBUTING](CONTRIBUTING.md),
[architecture](docs/design/architecture.md), [porting details](docs/porting.md),
[launch checklist](docs/launch-checklist.md) and [MIT license](LICENSE).
