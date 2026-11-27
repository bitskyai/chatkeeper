package web

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func TestStepBodyInlineCopy(t *testing.T) {
	parts, left := stepBody("Name: `kept` then `other` and `scope`.", []pluginapi.CopyText{{Label: "Name", Value: "kept"}, {Label: "Scope", Value: "scope"}, {Label: "Doc", Value: "a\nb"}})
	if len(left) != 1 || left[0].Label != "Doc" {
		t.Fatal("inline values must leave the copy list", left)
	}
	text := ""
	for _, p := range parts {
		if p.Copy {
			text += "[" + p.ID + ":" + p.Text + "]"
		} else {
			text += p.Text
		}
	}
	if text != "Name: [inline-0:kept] then `other` and [inline-1:scope]." {
		t.Fatal(text)
	}
	if parts, left := stepBody("plain", []pluginapi.CopyText{{Label: "x", Value: "y"}}); parts != nil || len(left) != 1 {
		t.Fatal("no inline values keeps the plain body")
	}
}

// Status and validation read a saved workspace without echoing secrets.
func TestSetupStatusAndValidate(t *testing.T) {
	fake := slacktest.New(t)
	home := t.TempDir()
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_APP_TOKEN", "")
	const bot = "xoxb-synthetic-status-secret"
	store := &secrets.Store{Home: home}
	cfg := filepath.Join(home, "config.yaml")
	env := map[string]string{"B": bot}
	res := RunSetup(context.Background(), SetupOptions{Platform: "slack", Workspace: "status", Config: cfg, Home: home, Mode: "server", Values: map[string]string{"bot_token": "B", "channels": "auto"}, Getenv: func(k string) string { return env[k] }, Secrets: store, HTTP: fake.Client()})
	if !res.OK || res.Steps[1].Skipped != true {
		t.Fatal(res)
	}
	st, e := SetupStatus(cfg, store)
	if e != nil || len(st.Workspaces) != 1 || !st.Workspaces[0].Credentials["bot_token"] || st.Workspaces[0].Credentials["app_token"] || len(st.Workspaces[0].Available) != 1 {
		t.Fatal(st, e)
	}
	v, e := ValidateWorkspace(context.Background(), cfg, store, "status", fake.Client())
	b, _ := json.Marshal(v)
	if e != nil || !v.OK || len(v.Steps) != 2 || v.Steps[0].ID != "bot_token" || !v.Steps[0].OK || !v.Steps[1].Skipped || strings.Contains(string(b), bot) {
		t.Fatal(string(b), e)
	}
	if _, e := ValidateWorkspace(context.Background(), cfg, store, "missing", fake.Client()); e == nil {
		t.Fatal("unknown workspace")
	}
}

// After the bot token is replaced by one for another app, validation must
// compare the app-level token with the freshly resolved app, not the app id
// saved at setup time.
func TestValidateAfterBotTokenRotation(t *testing.T) {
	fake := slacktest.New(t)
	home := t.TempDir()
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_APP_TOKEN", "")
	store := &secrets.Store{Home: home}
	cfg := filepath.Join(home, "config.yaml")
	env := map[string]string{"B": "xoxb-synthetic-rotate", "X": "xapp-synthetic-rotate"}
	res := RunSetup(context.Background(), SetupOptions{Platform: "slack", Workspace: "rot", Config: cfg, Home: home, Mode: "server", Values: map[string]string{"bot_token": "B", "app_token": "X", "channels": "auto"}, Getenv: func(k string) string { return env[k] }, Secrets: store, HTTP: fake.Client()})
	if !res.OK {
		t.Fatal(res)
	}
	if v, e := ValidateWorkspace(context.Background(), cfg, store, "rot", fake.Client()); e != nil || !v.OK {
		t.Fatal("matching tokens must validate", v, e)
	}
	// The stored bot token now belongs to app ANEW; the app-level token is
	// still AFIXTURE's and the saved settings still say AFIXTURE.
	fake.Overrides["bots.info"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"bot":{"id":"BFIXTURE","app_id":"ANEW"}}`))
	}
	v, e := ValidateWorkspace(context.Background(), cfg, store, "rot", fake.Client())
	b, _ := json.Marshal(v)
	if e != nil || v.OK || !strings.Contains(string(b), "different Slack app (AFIXTURE) than your bot token (ANEW)") {
		t.Fatal("mismatched credentials approved after rotation", string(b), e)
	}
}
