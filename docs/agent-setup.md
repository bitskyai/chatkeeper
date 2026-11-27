# Set up ChatKeeper with a coding agent

A coding agent (Claude Code, Codex or similar) can run the whole Slack setup
from a terminal. You do two things only Slack lets a person do: install the app
in your workspace and, for instant capture, create the app-level token. The
wizard shows the same instructions as a copyable prompt under **Set up with an
AI agent** on its first page; `chatkeeper setup slack --print-agent-prompt`
prints it.

Rules for the agent: never print, log, paste into chat or commit a token. Pass
tokens only through environment variables. `chatkeeper setup` reads them from
the variables you name, stores them in ChatKeeper's secret store (the OS
keychain on a desktop, `secrets.env` with mode 0600 on a server) and prints a
JSON result that never contains token values.

## 1. Create the Slack app from the manifest

```sh
chatkeeper setup slack --print-manifest > chatkeeper-manifest.yaml
```

The manifest is the same one the wizard uses (single source in the Slack
plugin): bot scopes, the reaction and message events, and
`socket_mode_enabled: true`.

- **By hand (one click):** open <https://api.slack.com/apps?new_app=1>, choose
  **From a manifest**, pick the workspace, switch to YAML, paste the file, then
  **Next** and **Create**.
- **By API (optional):** with a Slack app configuration token in
  `SLACK_CONFIG_TOKEN`, call
  [`apps.manifest.create`](https://docs.slack.dev/reference/methods/apps.manifest.create)
  with the manifest as JSON. It returns the app id and OAuth credentials; it
  does not install the app or return a bot token.

## 2. Human: install the app and hand over the bot token

Ask the user to open the new app, click **Install App > Install to Workspace >
Allow**, and copy the **Bot User OAuth Token** (`xoxb-`) into an environment
variable the agent can read, for example `export SLACK_BOT_TOKEN=...` in the
agent's shell. Installing requires a person to approve the OAuth screen.

## 3. Human, optional: create the app-level token

Live events (Socket Mode) let ChatKeeper archive a thread the moment someone
reacts on any message. Without them ChatKeeper polls every two minutes and only
sees reactions on the first message of a thread.

Slack has no API that creates app-level tokens: `apps.manifest.create` returns
only the app id and OAuth credentials, and Slack's
[Socket Mode guide](https://docs.slack.dev/apis/events-api/using-socket-mode)
and [token reference](https://docs.slack.dev/authentication/tokens) generate
them in the app's settings. So this is one human click sequence:

1. Open the app's **Basic Information** page:
   `https://app.slack.com/app-settings/<TEAM_ID>/<APP_ID>/general` (the setup
   JSON prints this link once the bot token is known), scroll to
   **App-Level Tokens**, click **Generate Token and Scopes**.
2. Name it `chatkeeper`.
3. **Add Scope** `connections:write` (the only scope it needs).
4. **Generate**, copy the `xapp-` token into `SLACK_APP_TOKEN`.
5. On **Socket Mode** (`https://app.slack.com/app-settings/<TEAM_ID>/<APP_ID>/socket-mode`)
   make sure **Enable Socket Mode** is on. The manifest turns it on.

Slack moved app settings to `app.slack.com/app-settings/<team>/<app>/...`;
older `api.slack.com/apps/<app>/socket-mode` links return 404.

## 4. Validate without saving

```sh
chatkeeper setup slack --dry-run --channels auto --reactions white_check_mark
```

This runs exactly the wizard's checks: the bot token (`auth.test`, workspace,
bot user, every required scope), the app-level token (a real Socket Mode
handshake that must report the bot token's app), the channel list and the
reactions. `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` are used when set; name
other variables with `--bot-token-env VAR` and `--app-token-env VAR`. A
failing check sets `"ok": false`, names `failed_step`, explains the fix in
`messages` or `field_errors` and lists the pages to open in `links`. The exit
code is non-zero on failure.

Ask the user which channels and reactions to watch. `--channels` takes channel
ids, names (`general` or `#general`) or `auto` (every channel the bot is
invited to); `--reactions` takes emoji names without colons, comma-separated.

## 5. Save

```sh
chatkeeper setup slack --workspace-name team \
  --channels auto --reactions tv,white_check_mark
```

Optional flags: `--storage PATH` (archive folder), `--match all` (require all
reactions), `--skip app_token` (polling only even if `SLACK_APP_TOKEN` is set),
`--replace` (overwrite a workspace of the same name), `--config PATH`,
`--mode desktop|server`. `chatkeeper setup slack --help` lists every option.

The result looks like this (ids shortened):

```json
{
  "ok": true,
  "platform": "slack",
  "workspace": "team",
  "config": "/home/me/.config/chatkeeper/config.yaml",
  "storage": "/home/me/ChatKeeper/team",
  "credentials": {"app_token": "stored as CHATKEEPER_..._APP_TOKEN", "bot_token": "stored as CHATKEEPER_..._BOT_TOKEN"},
  "settings": {"team_id": "T...", "app_id": "A...", "channels": "auto", "trigger": {"reactions": ["tv", "white_check_mark"], "match": "any", "who": "any"}},
  "steps": [{"id": "bot_token", "ok": true, "checks": [...]}, {"id": "app_token", "ok": true}, {"id": "channels", "ok": true}, {"id": "trigger", "ok": true}]
}
```

## 6. Run

Start ChatKeeper with `chatkeeper` (a running instance reloads the new
workspace within seconds). Ask the user to invite the bot to each watched
channel with `/invite @ChatKeeper`.

## MCP setup tools

The stdio MCP server (`chatkeeper mcp`) also offers two read-only tools:

- `setup_status`: platforms, configured workspaces and which credentials are
  present (`true`/`false`, never values), plus optional actions still
  available, such as enabling instant capture.
- `validate_setup` (`workspace`): re-runs the connection tests with the stored
  credentials and returns each check, redacted. It saves nothing.

Configuration itself is done with `chatkeeper setup`, not over MCP. HTTP MCP
(`/mcp`) does not offer these tools.
