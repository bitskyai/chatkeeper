package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
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
      - emoji:read
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

// optionalScopes unlock extras; setup continues without them.
var optionalScopes = map[string]string{"emoji:read": "custom workspace emoji in the reaction picker"}

const createBody = `1. Click Create Slack app, choose your workspace, review the settings and click Create. Socket Mode and every permission ChatKeeper needs are already filled in.
2. In the left menu open Install App, click Install to Workspace, then Allow.
3. Copy the Bot User OAuth Token (starts with xoxb-).

Slack's Install App page only shows the bot token. You will create the app-level token (xapp-) in a later step; this wizard links you straight to the right page.`

const appTokenBody = `Live events need an app-level token (xapp-). It is created separately from the bot token:
1. Open Basic Information (button below) and scroll to App-Level Tokens.
2. Click Generate Token and Scopes, name it ChatKeeper, click Add Scope and choose connections:write, then Generate.
3. Copy the token that starts with xapp- and paste it here, then click Test connection.

Socket Mode is already turned on by the manifest. If the test says otherwise, open Socket Mode settings and switch on Enable Socket Mode.

No app-level token? Choose Continue with polling only. ChatKeeper will then check for reactions every two minutes instead of instantly; you can add the token later in Settings.`

func (Plugin) SetupFlow() []pluginapi.SetupStep {
	return []pluginapi.SetupStep{
		{ID: "create_app", Title: "Create your Slack app", Kind: "instructions", Body: createBody, Links: []pluginapi.Link{{Label: "Create Slack app", URL: "https://api.slack.com/apps?new_app=1&manifest_yaml=" + url.QueryEscape(AppManifest)}}},
		{ID: "bot_token", Title: "Paste your bot token", Kind: "form", Body: "Paste the Bot User OAuth Token (xoxb-) from Install App or OAuth & Permissions, then click Test connection. Tokens stay on this device or server and are never shown back.", Schema: json.RawMessage(`{"type":"object","properties":{"bot_token":{"type":"string","title":"Bot User OAuth Token (xoxb-)","x-secret":true}},"required":["bot_token"]}`), Run: validateBot, Test: validateBot},
		{ID: "app_token", Title: "Add the app-level token", Kind: "form", Body: appTokenBody, Schema: json.RawMessage(`{"type":"object","properties":{"app_token":{"type":"string","title":"App-Level Token (xapp-)","x-secret":true}}}`), Prepare: appTokenLinks, Run: validateApp, Test: validateApp, Skip: "Continue with polling only", SkipNote: "Polling only: ChatKeeper checks for reactions on the first message of each thread every two minutes; reactions on replies need live events. Add an app-level token later in Settings for instant archiving."},
		{ID: "channels", Title: "Choose channels", Kind: "choose", Body: "Choose All invited channels, or a channel below. Private channels appear after inviting the bot. If a channel says invite the bot, open that channel and type /invite @ChatKeeper, then refresh this page.", Schema: json.RawMessage(`{"type":"object","properties":{"channels":{"type":"array","title":"Channels","items":{"type":"string"},"x-widget":"channel-picker"}}}`), Run: setupChannels, Prepare: func(ctx context.Context, st pluginapi.SetupState, env pluginapi.Env) (pluginapi.StepResult, error) {
			return setupChannels(ctx, st, nil, env)
		}},
		{ID: "trigger", Title: "Choose reactions", Kind: "form", Body: "Pick one or more reactions. With live events (Socket Mode), adding one to any message in a thread, the first message or a reply, archives the whole thread instantly. Polling only reads reactions on the first message of each thread, so without an app-level token react on the first message. Skin tones count: :+1: matches every tone; type a toned name such as +1::skin-tone-3 to require that tone.", Schema: json.RawMessage(`{"type":"object","properties":{"reactions":{"type":"array","title":"Trigger reactions","items":{"type":"string"},"default":["white_check_mark"],"x-widget":"reaction-picker"},"match":{"type":"string","title":"Archive when","enum":["any","all"],"default":"any"}},"required":["reactions"]}`), Prepare: reactionChoices, Run: setupTrigger},
		{ID: "ready", Title: "Connection ready", Kind: "instructions", Body: "Your connection is ready. Next choose where to save your archived threads."},
	}
}

func authTest(ctx context.Context, c *Client, token string) (map[string]any, map[string]bool, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://slack.com/api/auth.test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := c.http.Do(req)
	if e != nil {
		return nil, nil, errAPI
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if e != nil || len(b) > int(maxJSON) || res.StatusCode != 200 {
		return nil, nil, errAPI
	}
	doc, e := parse(b)
	if e != nil {
		return nil, nil, e
	}
	granted := map[string]bool{}
	for _, s := range strings.Split(res.Header.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			granted[s] = true
		}
	}
	return doc, granted, nil
}

// validateBot is both the Test connection action and the Continue check.
func validateBot(ctx context.Context, _ pluginapi.SetupState, _ json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
	token := env.Credentials["bot_token"]
	if !validToken(token, "xoxb-") {
		return pluginapi.StepResult{FieldErrors: map[string]string{"bot_token": "Paste the Bot User OAuth Token starting with xoxb-"}}, nil
	}
	c := NewClient(token, env.HTTP)
	doc, granted, e := authTest(ctx, c, token)
	if e != nil {
		var api apiError
		if errors.As(e, &api) {
			return pluginapi.StepResult{Messages: []string{"Bot token is invalid (" + api.code + "). Copy the Bot User OAuth Token from Install App or OAuth & Permissions."}, Checks: []pluginapi.Check{{Label: "Bot token", Detail: "rejected by Slack: " + api.code}}}, nil
		}
		return pluginapi.StepResult{Messages: []string{"Could not connect to Slack. Check your network and try again."}, Checks: []pluginapi.Check{{Label: "Slack API", Detail: "unreachable"}}}, nil
	}
	if !id(doc["team_id"], "team") || !id(doc["user_id"], "user") {
		return pluginapi.StepResult{Messages: []string{"Bot token is invalid. Copy the Bot User OAuth Token from Install App or OAuth & Permissions."}}, nil
	}
	checks := []pluginapi.Check{
		{Label: "Workspace", Detail: strings.TrimSpace(str(doc["team"]) + " (" + str(doc["team_id"]) + ")"), OK: true},
		{Label: "Bot user", Detail: strings.TrimSpace("@" + str(doc["user"]) + " (" + str(doc["user_id"]) + ")"), OK: true},
	}
	appID := ""
	if bot := str(doc["bot_id"]); bot != "" {
		if info, e := c.Call(ctx, "bots.info", url.Values{"bot": {bot}}); e == nil {
			if b, ok := info["bot"].(map[string]any); ok && appPattern.MatchString(str(b["app_id"])) {
				appID = str(b["app_id"])
				checks = append(checks, pluginapi.Check{Label: "Slack app", Detail: appID, OK: true})
			}
		}
	}
	missing := []string{}
	for _, s := range requiredScopes {
		if !granted[s] {
			missing = append(missing, s)
		}
		checks = append(checks, pluginapi.Check{Label: "Scope " + s, Detail: grantedText(granted[s]), OK: granted[s]})
	}
	optional := []string{}
	for s := range optionalScopes {
		optional = append(optional, s)
	}
	sort.Strings(optional)
	for _, s := range optional {
		detail := grantedText(granted[s]) + " (optional: " + optionalScopes[s] + ")"
		checks = append(checks, pluginapi.Check{Label: "Scope " + s, Detail: detail, OK: granted[s], Optional: true})
	}
	if len(missing) > 0 {
		return pluginapi.StepResult{Checks: checks, Messages: []string{"Missing scopes: " + strings.Join(missing, ", ") + ". Add these bot permissions in OAuth & Permissions, then reinstall the app."}}, nil
	}
	data := map[string]any{"team_id": doc["team_id"]}
	if appID != "" {
		data["app_id"] = appID
	}
	b, _ := json.Marshal(data)
	msg := "Connected to " + str(doc["team"]) + " as @" + str(doc["user"]) + ". All required permissions granted."
	return pluginapi.StepResult{OK: true, Data: b, Checks: checks, Messages: []string{msg}}, nil
}

var appPattern = regexp.MustCompile(`^A[A-Z0-9]{2,30}$`)

func grantedText(ok bool) string {
	if ok {
		return "granted"
	}
	return "missing"
}

// AppLinks deep-links the app's own settings pages once its id is known.
func AppLinks(appID string) []pluginapi.Link {
	if !appPattern.MatchString(appID) {
		return []pluginapi.Link{{Label: "Open your Slack apps", URL: "https://api.slack.com/apps"}}
	}
	return []pluginapi.Link{{Label: "Open Basic Information", URL: "https://api.slack.com/apps/" + appID + "/general"}, {Label: "Socket Mode settings", URL: "https://api.slack.com/apps/" + appID + "/socket-mode"}}
}
func appTokenLinks(_ context.Context, st pluginapi.SetupState, _ pluginapi.Env) (pluginapi.StepResult, error) {
	var appID string
	_ = json.Unmarshal(st["app_id"], &appID)
	return pluginapi.StepResult{OK: true, Links: AppLinks(appID)}, nil
}

// validateApp opens a real Socket Mode connection and waits for Slack's hello.
func validateApp(ctx context.Context, _ pluginapi.SetupState, _ json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
	token := env.Credentials["app_token"]
	if token == "" {
		return pluginapi.StepResult{FieldErrors: map[string]string{"app_token": "Paste the App-Level Token starting with xapp-, or choose Continue with polling only"}}, nil
	}
	if !validToken(token, "xapp-") {
		return pluginapi.StepResult{FieldErrors: map[string]string{"app_token": "Paste the App-Level Token starting with xapp- (the xoxb- bot token goes in the previous step)"}}, nil
	}
	check, ok := SocketCheck(ctx, NewClient("", env.HTTP), token)
	if !ok {
		return pluginapi.StepResult{Checks: []pluginapi.Check{check}, Messages: []string{"Socket Mode test failed: " + check.Detail}}, nil
	}
	return pluginapi.StepResult{OK: true, Checks: []pluginapi.Check{{Label: "App-level token", Detail: "accepted (connections:write)", OK: true}, check}, Messages: []string{"Socket Mode connected. Reactions will be archived instantly."}}, nil
}

// SocketCheck performs one complete Socket Mode handshake.
func SocketCheck(ctx context.Context, c *Client, token string) (pluginapi.Check, bool) {
	fail := func(detail string) (pluginapi.Check, bool) {
		return pluginapi.Check{Label: "Socket Mode", Detail: detail}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, e := c.get(ctx, "POST", "https://slack.com/api/apps.connections.open", token, maxJSON, false)
	if e != nil {
		return fail("could not reach Slack")
	}
	doc, e := parse(b)
	var api apiError
	if errors.As(e, &api) {
		switch api.code {
		case "invalid_auth", "not_authed", "token_revoked":
			return fail("the app-level token was rejected (" + api.code + "); generate a new one")
		case "not_allowed_token_type":
			return fail("this is not an app-level token; it must start with xapp-")
		case "missing_scope":
			return fail("the token lacks connections:write; generate it with that scope")
		default:
			return fail("Slack refused the connection (" + api.code + "); confirm Enable Socket Mode is on")
		}
	}
	if e != nil || SocketURL(str(doc["url"])) != nil {
		return fail("Slack returned an invalid Socket Mode address")
	}
	conn, _, e := websocket.Dial(ctx, str(doc["url"]), &websocket.DialOptions{HTTPClient: c.http})
	if e != nil {
		return fail("the Socket Mode websocket could not connect")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	_, raw, e := conn.Read(ctx)
	if e != nil {
		return fail("connected, but Slack sent no hello")
	}
	var hello struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &hello) != nil || hello.Type != "hello" {
		return fail("connected, but Slack did not confirm the session")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return pluginapi.Check{Label: "Socket Mode", Detail: "connected (hello received)", OK: true}, true
}

func reactionChoices(ctx context.Context, _ pluginapi.SetupState, env pluginapi.Env) (pluginapi.StepResult, error) {
	choices := []pluginapi.Choice{}
	var messages []string
	token := env.Credentials["bot_token"]
	if validToken(token, "xoxb-") {
		custom, e := customEmoji(ctx, NewClient(token, env.HTTP))
		var api apiError
		switch {
		case e == nil:
			choices = append(choices, custom...)
		case errors.As(e, &api) && api.code == "missing_scope":
			messages = append(messages, "Custom workspace emoji need the optional emoji:read permission. Standard emoji are listed; you can still type a custom name.")
		default:
			messages = append(messages, "Custom workspace emoji could not be loaded. You can still type a custom name.")
		}
	}
	return pluginapi.StepResult{OK: true, Choices: append(choices, StandardEmoji()...), Messages: messages}, nil
}

// ReactionNames splits picker and free-text input into unique emoji names.
func ReactionNames(values []string) []string {
	out, seen := []string{}, map[string]bool{}
	for _, v := range values {
		for _, name := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
			name = strings.Trim(name, ":")
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

func setupTrigger(_ context.Context, _ pluginapi.SetupState, in json.RawMessage, _ pluginapi.Env) (pluginapi.StepResult, error) {
	var v struct {
		Reactions []string `json:"reactions"`
		Match     string   `json:"match"`
	}
	if json.Unmarshal(in, &v) != nil {
		return pluginapi.StepResult{FieldErrors: map[string]string{"reactions": "Choose at least one reaction"}}, nil
	}
	names := ReactionNames(v.Reactions)
	if len(names) == 0 || len(names) > 20 {
		return pluginapi.StepResult{FieldErrors: map[string]string{"reactions": "Choose between one and 20 reactions"}}, nil
	}
	for _, n := range names {
		if !reactionPattern.MatchString(n) {
			return pluginapi.StepResult{FieldErrors: map[string]string{"reactions": "Use emoji names without colons, for example white_check_mark"}}, nil
		}
	}
	if v.Match != "all" {
		v.Match = "any"
	}
	b, _ := json.Marshal(map[string]any{"trigger": map[string]any{"reactions": names, "who": "any", "match": v.Match}})
	return pluginapi.StepResult{OK: true, Data: b}, nil
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
