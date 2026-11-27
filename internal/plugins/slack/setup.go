package slack

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// AppManifest is the single source for the browser link and example manifest.
const AppManifest = `display_information:
  name: ChatKeeper
  description: Archive threads you mark with a reaction
features:
  bot_user:
    display_name: ChatKeeper
    always_online: false
oauth_config:
  scopes:
    bot:
      - channels:history
      - channels:read
      - groups:history
      - groups:read
      - reactions:read
      - files:read
      - users:read
settings:
  event_subscriptions:
    bot_events:
      - reaction_added
      - reaction_removed
      - message.channels
      - message.groups
  socket_mode_enabled: true
  org_deploy_enabled: false
  token_rotation_enabled: false
`

var requiredScopes = []string{"channels:history", "channels:read", "groups:history", "groups:read", "reactions:read", "files:read", "users:read"}

func (Plugin) SetupFlow() []pluginapi.SetupStep {
	return []pluginapi.SetupStep{
		{ID: "create_app", Title: "Create your Slack app", Kind: "instructions", Body: "1. Click Create Slack app, choose your workspace, then click Create. The permissions and Socket Mode are already filled in.\n2. In OAuth & Permissions, click Install to Workspace, then Allow. Copy the Bot User OAuth Token (xoxb-).\n3. In Basic Information → App-Level Tokens, generate a token named ChatKeeper with connections:write. Copy the xapp- token. Keep this page open while you continue.", Links: []pluginapi.Link{{Label: "Create Slack app", URL: "https://api.slack.com/apps?new_app=1&manifest_yaml=" + url.QueryEscape(AppManifest)}}},
		{ID: "credentials", Title: "Paste your tokens", Kind: "form", Body: "Paste the Bot User OAuth Token (xoxb-) and App-Level Token (xapp-). Tokens stay on this device or server and are never shown back. The app token is optional; without it, polling checks for reactions every two minutes.", Schema: json.RawMessage(`{"type":"object","properties":{"bot_token":{"type":"string","title":"Bot User OAuth Token (xoxb-)","x-secret":true},"app_token":{"type":"string","title":"App-Level Token (xapp-), optional","x-secret":true}},"required":["bot_token"]}`), Run: checkTokenShape},
		{ID: "validate", Title: "Check your connection", Kind: "check", Body: "We will check your workspace, every required permission, and your Socket Mode token.", Run: validateSetup},
		{ID: "channels", Title: "Choose channels", Kind: "choose", Body: "Choose All invited channels, or a channel below. Private channels appear after inviting the bot. If a channel says invite the bot, open that channel and type /invite @ChatKeeper, then refresh this page.", Schema: json.RawMessage(`{"type":"object","properties":{"channels":{"type":"array","title":"Channels","items":{"type":"string"},"x-widget":"channel-picker"}}}`), Run: setupChannels},
		{ID: "trigger", Title: "Choose a reaction", Kind: "form", Body: "React on any message in a thread to archive the entire thread. Choose a reaction or enter its emoji name without colons.", Schema: json.RawMessage(`{"type":"object","properties":{"reaction":{"type":"string","title":"Reaction emoji name","default":"white_check_mark","examples":["white_check_mark","bookmark","star","heart"]}},"required":["reaction"]}`), Run: func(_ context.Context, _ pluginapi.SetupState, in json.RawMessage, _ pluginapi.Env) (pluginapi.StepResult, error) {
			var v struct {
				Reaction string `json:"reaction"`
			}
			if json.Unmarshal(in, &v) != nil || !reactionPattern.MatchString(v.Reaction) {
				return pluginapi.StepResult{FieldErrors: map[string]string{"reaction": "Use an emoji name without colons"}}, nil
			}
			b, _ := json.Marshal(map[string]any{"trigger": map[string]any{"reactions": []string{v.Reaction}, "who": "any"}})
			return pluginapi.StepResult{OK: true, Data: b}, nil
		}},
		{ID: "ready", Title: "Connection ready", Kind: "instructions", Body: "Your connection is ready. Next choose where to save your archived threads."},
	}
}
func checkTokenShape(_ context.Context, _ pluginapi.SetupState, _ json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
	fields := map[string]string{}
	if !validToken(env.Credentials["bot_token"], "xoxb-") {
		fields["bot_token"] = "Paste the Bot User OAuth Token starting with xoxb-"
	}
	if v := env.Credentials["app_token"]; v != "" && !validToken(v, "xapp-") {
		fields["app_token"] = "Paste the App-Level Token starting with xapp-"
	}
	return pluginapi.StepResult{OK: len(fields) == 0, FieldErrors: fields}, nil
}
func validateSetup(ctx context.Context, _ pluginapi.SetupState, _ json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
	c := NewClient(env.Credentials["bot_token"], env.HTTP)
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://slack.com/api/auth.test", nil)
	req.Header.Set("Authorization", "Bearer "+env.Credentials["bot_token"])
	res, e := c.http.Do(req)
	if e != nil {
		return pluginapi.StepResult{Messages: []string{"Could not connect. Check your token and try again."}}, nil
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if e != nil || len(b) > int(maxJSON) || res.StatusCode != 200 {
		return pluginapi.StepResult{Messages: []string{"Could not validate the bot token."}}, nil
	}
	doc, e := parse(b)
	if e != nil || !id(doc["team_id"], "team") || !id(doc["user_id"], "user") {
		return pluginapi.StepResult{Messages: []string{"Bot token is invalid. Copy the Bot User OAuth Token from OAuth & Permissions."}}, nil
	}
	granted := map[string]bool{}
	for _, s := range strings.Split(res.Header.Get("X-OAuth-Scopes"), ",") {
		granted[strings.TrimSpace(s)] = true
	}
	missing := []string{}
	for _, s := range requiredScopes {
		if !granted[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return pluginapi.StepResult{Messages: []string{"Missing scopes: " + strings.Join(missing, ", ") + ". Add these bot permissions in OAuth & Permissions, then reinstall the app."}}, nil
	}
	messages := []string{"Connected. All required permissions granted."}
	if token := env.Credentials["app_token"]; token != "" {
		b, e = c.get(ctx, "POST", "https://slack.com/api/apps.connections.open", token, maxJSON, false)
		if e != nil {
			return pluginapi.StepResult{Messages: []string{"App token validation failed. Check connections:write and Socket Mode."}}, nil
		}
		d, e := parse(b)
		if e != nil || SocketURL(str(d["url"])) != nil {
			return pluginapi.StepResult{Messages: []string{"App token validation failed. Enable Socket Mode and generate an xapp- token with connections:write."}}, nil
		}
	} else {
		messages = append(messages, "No app token: polling-only mode; captures can take up to two minutes.")
	}
	data, _ := json.Marshal(map[string]any{"team_id": doc["team_id"]})
	return pluginapi.StepResult{OK: true, Data: data, Messages: messages}, nil
}
func setupChannels(ctx context.Context, _ pluginapi.SetupState, in json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
	c := NewClient(env.Credentials["bot_token"], env.HTTP)
	all := map[string]pluginapi.Choice{}
	members := map[string]bool{}
	for _, method := range []string{"users.conversations", "conversations.list"} {
		cursor := ""
		seen := map[string]bool{}
		for pages := 0; pages < 1000; pages++ {
			doc, e := c.Call(ctx, method, url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"200"}, "cursor": {cursor}})
			if e != nil {
				return pluginapi.StepResult{}, errAPI
			}
			chs, ok := doc["channels"].([]any)
			if !ok {
				return pluginapi.StepResult{}, errAPI
			}
			for _, raw := range chs {
				ch, ok := raw.(map[string]any)
				if !ok || !id(ch["id"], "channel") || ch["is_archived"] == true {
					continue
				}
				cid := str(ch["id"])
				if method == "users.conversations" || ch["is_member"] == true {
					members[cid] = true
				}
				label := "#" + str(ch["name"])
				if !members[cid] {
					label += " — invite the bot: /invite @ChatKeeper"
				}
				all[cid] = pluginapi.Choice{ID: cid, Label: label}
			}
			cursor, e = nextCursor(doc)
			if e != nil {
				return pluginapi.StepResult{}, e
			}
			if cursor == "" {
				break
			}
			if seen[cursor] {
				return pluginapi.StepResult{}, errAPI
			}
			seen[cursor] = true
		}
	}
	choices := []pluginapi.Choice{{ID: "auto", Label: "All channels the bot is invited to"}}
	keys := []string{}
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		choices = append(choices, all[k])
	}
	if len(in) == 0 {
		return pluginapi.StepResult{OK: true, Choices: choices}, nil
	}
	var v struct {
		Channels []string `json:"channels"`
	}
	if json.Unmarshal(in, &v) != nil || len(v.Channels) == 0 {
		return pluginapi.StepResult{Choices: choices, Messages: []string{"Choose at least one channel"}}, nil
	}
	for _, id := range v.Channels {
		if id == "auto" {
			if len(v.Channels) != 1 {
				return pluginapi.StepResult{Choices: choices, Messages: []string{"Choose All invited channels alone"}}, nil
			}
			return pluginapi.StepResult{OK: true, Data: json.RawMessage(`{"channels":"auto"}`)}, nil
		}
		if !members[id] {
			return pluginapi.StepResult{Choices: choices, Messages: []string{"Invite the bot to the selected channel: /invite @ChatKeeper, then try again."}}, nil
		}
	}
	b, _ := json.Marshal(map[string]any{"channels": v.Channels})
	return pluginapi.StepResult{OK: true, Data: b}, nil
}
