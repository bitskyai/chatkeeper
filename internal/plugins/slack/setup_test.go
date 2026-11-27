package slack_test

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func step(t *testing.T, id string) pluginapi.SetupStep {
	t.Helper()
	for _, s := range (slack.Plugin{}).SetupFlow() {
		if s.ID == id {
			return s
		}
	}
	t.Fatal("missing step", id)
	return pluginapi.SetupStep{}
}

func TestSetupValidation(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "scope"}[missing], func(t *testing.T) {
			f := slacktest.New(t)
			if missing {
				f.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-OAuth-Scopes", "channels:read")
					_, _ = w.Write([]byte(`{"ok":true,"team_id":"TFIXTURE","user_id":"UBOT"}`))
				}
			}
			bot := step(t, "bot_token")
			if bot.Test == nil {
				t.Fatal("bot token step has no Test connection action")
			}
			res, e := bot.Run(context.Background(), nil, nil, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}, HTTP: f.Client()})
			if e != nil {
				t.Fatal(e)
			}
			if missing {
				if res.OK || !strings.Contains(strings.Join(res.Messages, " "), "files:read") {
					t.Fatal(res)
				}
				found := false
				for _, c := range res.Checks {
					found = found || c.Label == "Scope files:read" && !c.OK && c.Detail == "missing"
				}
				if !found {
					t.Fatal("missing scope not itemized", res.Checks)
				}
				return
			}
			if !res.OK || !strings.Contains(string(res.Data), `"app_id":"AFIXTURE"`) {
				t.Fatal(res)
			}
			text := ""
			for _, c := range res.Checks {
				text += c.Label + "=" + c.Detail + ";"
			}
			for _, want := range []string{"Workspace=Fixture (TFIXTURE)", "Bot user=@fixturebot (UBOT)", "Scope reactions:read=granted", "Scope emoji:read=missing (optional"} {
				if !strings.Contains(text, want) {
					t.Fatal(want, text)
				}
			}
		})
	}
}

func TestAppTokenGuidanceAndSocketTest(t *testing.T) {
	f := slacktest.New(t)
	app := step(t, "app_token")
	if app.Skip == "" || !strings.Contains(app.Body, "App-Level Tokens") || !strings.Contains(app.Body, "connections:write") {
		t.Fatal("app token step lacks guidance or polling fallback", app)
	}
	prep, e := app.Prepare(context.Background(), pluginapi.SetupState{"team_id": json.RawMessage(`"TFIXTURE"`), "app_id": json.RawMessage(`"AFIXTURE"`)}, pluginapi.Env{})
	if e != nil || len(prep.Links) == 0 || prep.Links[0].URL != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general" {
		t.Fatal("deep link", prep, e)
	}
	env := pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}, HTTP: f.Client()}
	if res, _ := app.Run(context.Background(), nil, nil, env); res.OK || res.FieldErrors["app_token"] == "" {
		t.Fatal("blank app token must not silently fall back to polling", res)
	}
	env.Credentials["app_token"] = "xoxb-wrong-kind"
	if res, _ := app.Test(context.Background(), nil, nil, env); res.OK {
		t.Fatal("bot token accepted as app token")
	}
	env.Credentials["app_token"] = "xapp-synthetic"
	res, e := app.Test(context.Background(), nil, nil, env)
	if e != nil || !res.OK || len(res.Checks) != 3 || res.Checks[1].Detail != "connected (hello received)" || res.Checks[2].Detail != "AFIXTURE" {
		t.Fatal(res, e)
	}
	f.Overrides["apps.connections.open"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}
	if res, _ := app.Test(context.Background(), nil, nil, env); res.OK || !strings.Contains(res.Checks[0].Detail, "rejected") {
		t.Fatal(res)
	}
}

func TestReactionPickerChoices(t *testing.T) {
	f := slacktest.New(t)
	trigger := step(t, "trigger")
	res, e := trigger.Prepare(context.Background(), nil, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}, HTTP: f.Client()})
	if e != nil {
		t.Fatal(e)
	}
	byID := map[string]pluginapi.Choice{}
	for _, c := range res.Choices {
		byID[c.ID] = c
	}
	if byID["wastebasket"].Glyph != "🗑️" || byID["wastebasket"].Group != "Objects" || len(byID) < 1800 {
		t.Fatal("standard emoji missing", byID["wastebasket"], len(byID))
	}
	if byID["fixture_parrot"].Image == "" || byID["fixture_parrot"].Group != "Custom" || byID["fixture_ok"].Glyph != "✅" {
		t.Fatal("custom emoji missing", byID["fixture_parrot"], byID["fixture_ok"])
	}
	f.Overrides["emoji.list"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	}
	res, _ = trigger.Prepare(context.Background(), nil, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}, HTTP: f.Client()})
	if !strings.Contains(strings.Join(res.Messages, " "), "emoji:read") || len(res.Choices) < 1800 {
		t.Fatal("missing emoji:read should degrade to standard emoji", res.Messages)
	}
	out, _ := trigger.Run(context.Background(), nil, json.RawMessage(`{"reactions":["wastebasket",":bookmark:, my_custom","wastebasket"],"match":"any"}`), pluginapi.Env{})
	if !out.OK || string(out.Data) != `{"trigger":{"match":"any","reactions":["wastebasket","bookmark","my_custom"],"who":"any"}}` {
		t.Fatal(string(out.Data), out)
	}
	if out, _ := trigger.Run(context.Background(), nil, json.RawMessage(`{"reactions":["bad name!"]}`), pluginapi.Env{}); out.OK {
		t.Fatal("invalid name accepted")
	}
}

// The app-level token step resolves the bot token's app, links its settings,
// proves the xapp- token belongs to it and maps each failure to a precise fix.
func TestAppTokenSameAppAndFixes(t *testing.T) {
	f := slacktest.New(t)
	app := step(t, "app_token")
	if app.Upgrade == "" || len(app.Copy) != 2 || app.Copy[1].Value != "connections:write" || app.Copy[0].Value != "chatkeeper" {
		t.Fatal("copy values or dashboard upgrade missing", app.Copy, app.Upgrade)
	}
	const secret = "xapp-1-synthetic-secret"
	env := pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "app_token": secret}, HTTP: f.Client()}
	// Without saved state the app id comes from auth.test -> bots.info.
	prep, _ := app.Prepare(context.Background(), nil, env)
	if len(prep.Links) != 2 || prep.Links[0].URL != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general" || prep.Links[1].URL != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/socket-mode" {
		t.Fatal("deep links", prep.Links)
	}
	f.Overrides["bots.info"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	}
	prep, _ = app.Prepare(context.Background(), nil, env)
	if len(prep.Links) != 1 || prep.Links[0].URL != "https://api.slack.com/apps" || len(prep.Messages) == 0 {
		t.Fatal("unresolved app must fall back to the app list", prep)
	}
	// Fail closed: without the bot token's app id a working token from an
	// unrelated app must not pass, even with stale saved ids.
	for _, st := range []pluginapi.SetupState{nil, {"team_id": json.RawMessage(`"TFIXTURE"`), "app_id": json.RawMessage(`"AFIXTURE"`)}} {
		res, _ := app.Test(context.Background(), st, nil, env)
		if res.OK || !strings.Contains(strings.Join(res.Messages, " "), "could not confirm which Slack app") {
			t.Fatal("unresolved bot app must fail closed", res)
		}
	}
	delete(f.Overrides, "bots.info")
	// A hello without an app id cannot be compared either.
	f.Overrides["apps.connections.open"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"url":"wss://socket.slack.com/socket?app=none"}`))
	}
	if res, _ := app.Test(context.Background(), nil, nil, env); res.OK || !strings.Contains(strings.Join(res.Messages, " "), "did not say which app") {
		t.Fatal("hello without app id must fail closed", res)
	}
	delete(f.Overrides, "apps.connections.open")
	// The live lookup beats stale saved ids (bot token rotated to another app).
	if res, _ := app.Test(context.Background(), pluginapi.SetupState{"app_id": json.RawMessage(`"AOLD"`)}, nil, env); !res.OK {
		t.Fatal("fresh bot identity must win over saved app id", res)
	}
	open := func(body string) {
		f.Overrides["apps.connections.open"] = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
	}
	cases := []struct{ body, want string }{
		{`{"ok":true,"url":"wss://socket.slack.com/socket?app=AOTHER"}`, "different Slack app (AOTHER) than your bot token (AFIXTURE)"},
		{`{"ok":false,"error":"missing_scope"}`, "lacks connections:write"},
		{`{"ok":false,"error":"token_revoked"}`, "revoked"},
		{`{"ok":false,"error":"not_allowed_token_type"}`, "not an app-level token"},
		{`{"ok":false,"error":"socket_mode_not_enabled"}`, "Enable Socket Mode"},
	}
	for _, c := range cases {
		open(c.body)
		res, e := app.Test(context.Background(), nil, nil, env)
		text := strings.Join(res.Messages, " ")
		for _, ch := range res.Checks {
			text += " " + ch.Label + " " + ch.Detail
		}
		if e != nil || res.OK || !strings.Contains(text, c.want) {
			t.Fatal(c.body, text, e)
		}
		if len(res.Links) != 2 || res.Links[0].URL != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general" {
			t.Fatal("failure must link the fix", res.Links)
		}
		if b, _ := json.Marshal(res); strings.Contains(string(b), secret) {
			t.Fatal("token echoed")
		}
	}
	env.Credentials["app_token"] = "xoxp-user"
	if res, _ := app.Test(context.Background(), nil, nil, env); res.OK || !strings.Contains(res.FieldErrors["app_token"], "Basic Information") {
		t.Fatal(res)
	}
}

func TestCreateAppLinkCarriesManifest(t *testing.T) {
	create := step(t, "create_app")
	u, e := url.Parse(create.Links[0].URL)
	if e != nil || u.Host != "api.slack.com" || u.Query().Get("new_app") != "1" || u.Query().Get("manifest_yaml") != slack.AppManifest {
		t.Fatal("prefilled create link", create.Links[0].URL)
	}
	if len(create.Copy) != 1 || create.Copy[0].Value != slack.AppManifest {
		t.Fatal("copy-manifest fallback missing")
	}
	for _, want := range []string{"socket_mode_enabled: true", "      - reaction_added\n", "      - message.channels\n", "      - message.groups\n"} {
		if !strings.Contains(slack.AppManifest, want) {
			t.Fatal("manifest lacks", want)
		}
	}
}

// Slack moved app settings to app.slack.com/app-settings/<team>/<app>/<page>;
// api.slack.com/apps/<app>/socket-mode now returns 404. Pin the shapes.
func TestAppSettingsURLShapes(t *testing.T) {
	if got := slack.AppSettingsURL("TFIXTURE", "AFIXTURE", "general"); got != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general" {
		t.Fatal(got)
	}
	if got := slack.AppSettingsURL("TFIXTURE", "AFIXTURE", "socket-mode"); got != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/socket-mode" {
		t.Fatal(got)
	}
	for _, ids := range [][2]string{{"", "AFIXTURE"}, {"TFIXTURE", ""}, {"tbad", "AFIXTURE"}, {"TFIXTURE", "A/../x"}} {
		if got := slack.AppSettingsURL(ids[0], ids[1], "general"); got != "https://api.slack.com/apps" {
			t.Fatal("unknown ids must fall back to the app list", ids, got)
		}
	}
	links := slack.AppLinks("TFIXTURE", "AFIXTURE")
	if len(links) != 2 || !strings.Contains(links[0].Label, "Basic Information") || !strings.HasSuffix(links[1].URL, "/socket-mode") {
		t.Fatal(links)
	}
	if links := slack.AppLinks("", ""); len(links) != 1 || links[0].URL != slack.AppsURL {
		t.Fatal(links)
	}
	for _, s := range (slack.Plugin{}).SetupFlow() {
		for _, l := range s.Links {
			if strings.HasPrefix(l.URL, "https://api.slack.com/apps/") {
				t.Fatal("stale api.slack.com/apps/<id> deep link", s.ID, l.URL)
			}
		}
	}
}

// The token step lists the six numbered actions and marks the two values that
// render inline with copy buttons.
func TestAppTokenInstructions(t *testing.T) {
	app := step(t, "app_token")
	for _, want := range []string{"1. Click Open Basic Information", "App-Level Tokens", "Generate Token and Scopes", "2. Token Name: `chatkeeper`", "3. Click Add Scope and choose `connections:write`", "4. Click Generate", "xapp-", "5. Click Open Socket Mode", "Enable Socket Mode", "6. Paste the token", "polls every two minutes"} {
		if !strings.Contains(app.Body, want) {
			t.Fatal("instructions lack", want)
		}
	}
}

func TestAgentSetup(t *testing.T) {
	var p pluginapi.Plugin = slack.Plugin{}
	agent, ok := p.(pluginapi.AgentSetup)
	if !ok || agent.SetupManifest() != slack.AppManifest {
		t.Fatal("manifest must have a single source")
	}
	prompt := agent.AgentPrompt()
	for _, want := range []string{"chatkeeper setup slack --print-manifest", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "connections:write", "--dry-run", "docs/agent-setup.md", "Never print"} {
		if !strings.Contains(prompt, want) {
			t.Fatal("prompt lacks", want)
		}
	}
	if strings.Contains(prompt, "xoxb-1") || strings.Contains(prompt, "xapp-1") {
		t.Fatal("prompt contains a token")
	}
}
