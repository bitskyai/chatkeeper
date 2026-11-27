# Notes for coding agents

## Setting up ChatKeeper for a user

Follow [docs/agent-setup.md](docs/agent-setup.md). In short:

```sh
chatkeeper setup slack --print-manifest > chatkeeper-manifest.yaml   # create the app from it
# human: Install App > Install to Workspace; export SLACK_BOT_TOKEN=xoxb-...
# human, optional: Basic Information > App-Level Tokens, scope connections:write; export SLACK_APP_TOKEN=xapp-...
chatkeeper setup slack --dry-run --channels auto --reactions white_check_mark
chatkeeper setup slack --workspace-name team --channels auto --reactions white_check_mark
```

Never print, log or commit tokens; pass them only through environment
variables. Slack has no API for app-level tokens and installing needs the
user's approval, so those stay human clicks. Results are JSON; on failure read
`failed_step`, `messages` and `links`.

## Working on this repository

- Go toolchain from `go.mod`; run `go test ./...` before pushing.
- Platform names (Slack and others) appear only under `internal/plugins/`;
  `internal/architecture` enforces this for code, templates and assets.
- Tests use synthetic fixtures (`TFIXTURE`, `AFIXTURE`, `xoxb-synthetic`);
  never commit real ids or tokens.
