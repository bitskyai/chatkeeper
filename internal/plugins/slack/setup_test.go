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
	prep, e := app.Prepare(context.Background(), pluginapi.SetupState{"app_id": json.RawMessage(`"AFIXTURE"`)}, pluginapi.Env{})
	if e != nil || len(prep.Links) == 0 || prep.Links[0].URL != "https://api.slack.com/apps/AFIXTURE/general" {
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
	if len(prep.Links) != 2 || prep.Links[0].URL != "https://api.slack.com/apps/AFIXTURE/general" || prep.Links[1].URL != "https://api.slack.com/apps/AFIXTURE/socket-mode" {
		t.Fatal("deep links", prep.Links)
	}
	f.Overrides["bots.info"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	}
	prep, _ = app.Prepare(context.Background(), nil, env)
	if len(prep.Links) != 1 || prep.Links[0].URL != "https://api.slack.com/apps" || len(prep.Messages) == 0 {
		t.Fatal("unresolved app must fall back to the app list", prep)
	}
	if res, _ := app.Test(context.Background(), nil, nil, env); !res.OK {
		t.Fatal("an unresolvable app id must not block a working token", res)
	}
	delete(f.Overrides, "bots.info")
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
		if len(res.Links) != 2 || res.Links[0].URL != "https://api.slack.com/apps/AFIXTURE/general" {
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
