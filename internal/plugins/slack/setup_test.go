package slack_test

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
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
	if e != nil || !res.OK || len(res.Checks) != 2 || res.Checks[1].Detail != "connected (hello received)" {
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
