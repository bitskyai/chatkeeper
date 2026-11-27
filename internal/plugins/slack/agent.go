package slack

// SetupManifest implements pluginapi.AgentSetup.
func (Plugin) SetupManifest() string { return AppManifest }

// AgentPrompt implements pluginapi.AgentSetup. It names environment variables,
// never token values, and leaves the two Slack-only clicks to the human.
func (Plugin) AgentPrompt() string { return agentPrompt }

// agentPrompt mirrors docs/agent-setup.md.
const agentPrompt = `Set up ChatKeeper for my Slack workspace. Follow docs/agent-setup.md in the ChatKeeper repository (https://github.com/bitskyai/chatkeeper/blob/main/docs/agent-setup.md). Never print, log or commit tokens.

1. Run "chatkeeper setup slack --print-manifest > chatkeeper-manifest.yaml". Create the app from it: open https://api.slack.com/apps?new_app=1, choose "From a manifest", pick my workspace, paste the YAML, Create. (With a Slack configuration token you may call apps.manifest.create instead.)
2. Ask me to click Install App > Install to Workspace > Allow, and to put the Bot User OAuth Token (xoxb-) in the SLACK_BOT_TOKEN environment variable.
3. Optional, for instant capture: Slack has no API for app-level tokens, so ask me to open Basic Information > App-Level Tokens > Generate Token and Scopes, name it chatkeeper, add the scope connections:write, Generate, and put the xapp- token in SLACK_APP_TOKEN. Socket Mode must be on (the manifest enables it).
4. Validate without saving: "chatkeeper setup slack --dry-run --channels auto --reactions white_check_mark". Fix whatever the JSON result reports (each failed check says how). Ask me which channels and reactions to use.
5. Save: "chatkeeper setup slack --workspace-name <name> --channels <ids or names, or auto> --reactions <names>". SLACK_BOT_TOKEN and SLACK_APP_TOKEN are read when set (or name others with --bot-token-env / --app-token-env); without an app-level token ChatKeeper polls every two minutes. The command prints JSON and never prints tokens.
6. Start ChatKeeper with "chatkeeper" (or reload the running app) and invite the bot to each channel with /invite @ChatKeeper.`
