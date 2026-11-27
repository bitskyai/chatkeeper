package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
)

func setupRun(t *testing.T, home string, args ...string) (map[string]any, string, error) {
	t.Helper()
	var out bytes.Buffer
	e := execute(append([]string{"setup"}, args...), &out)
	var res map[string]any
	_ = json.Unmarshal(out.Bytes(), &res)
	return res, out.String(), e
}

// The agent path: validate, save config plus secrets, print JSON without
// tokens, then the saved workspace passes check-config.
func TestSetupCommandEndToEnd(t *testing.T) {
	fake := slacktest.New(t)
	before := setupHTTP
	setupHTTP = fake.Client()
	defer func() { setupHTTP = before }()
	home := t.TempDir()
	t.Setenv("CHATKEEPER_HOME", home)
	t.Setenv("CHATKEEPER_MODE", "server")
	const bot, app = "xoxb-synthetic-agent-secret", "xapp-synthetic-agent-secret"
	t.Setenv("AGENT_BOT", bot)
	t.Setenv("AGENT_APP", app)
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_APP_TOKEN", "")

	res, raw, e := setupRun(t, home, "slack", "--dry-run", "--bot-token-env", "AGENT_BOT", "--app-token-env", "AGENT_APP", "--channels", "#fixture", "--reactions", "tv,white_check_mark")
	if e != nil || res["ok"] != true || res["dry_run"] != true {
		t.Fatal(e, raw)
	}
	if strings.Contains(raw, bot) || strings.Contains(raw, app) {
		t.Fatal("token printed")
	}
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err == nil {
		t.Fatal("dry run wrote config")
	}
	settings := res["settings"].(map[string]any)
	if ch := settings["channels"].([]any); len(ch) != 1 || ch[0] != "CFIXTURE" {
		t.Fatal("channel name not mapped to id", settings)
	}

	res, raw, e = setupRun(t, home, "slack", "--workspace-name", "agent", "--bot-token-env", "AGENT_BOT", "--app-token-env=AGENT_APP", "--channels", "auto", "--reactions", "tv,white_check_mark")
	if e != nil || res["ok"] != true || res["workspace"] != "agent" {
		t.Fatal(e, raw)
	}
	if strings.Contains(raw, bot) || strings.Contains(raw, app) {
		t.Fatal("token printed")
	}
	steps := res["steps"].([]any)
	if len(steps) != 4 || steps[1].(map[string]any)["id"] != "app_token" || steps[1].(map[string]any)["ok"] != true {
		t.Fatal(raw)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	if strings.Contains(string(cfg), bot) || !strings.Contains(string(cfg), "TFIXTURE") || !strings.Contains(string(cfg), "tv") {
		t.Fatal("config", string(cfg))
	}
	secrets, _ := os.ReadFile(filepath.Join(home, "secrets.env"))
	if !strings.Contains(string(secrets), bot) || !strings.Contains(string(secrets), app) {
		t.Fatal("secrets not stored")
	}
	var out bytes.Buffer
	if e := execute([]string{"check-config", "--offline"}, &out); e != nil || !strings.Contains(out.String(), "1 workspaces") {
		t.Fatal(e, out.String())
	}

	// Existing names need --replace; --skip drops the optional token.
	if res, _, e := setupRun(t, home, "slack", "--workspace-name", "agent", "--bot-token-env", "AGENT_BOT", "--channels", "auto"); e == nil || !strings.Contains(res["error"].(string), "--replace") {
		t.Fatal("overwrote existing workspace", res)
	}
	res, raw, e = setupRun(t, home, "slack", "--workspace-name", "agent", "--replace", "--bot-token-env", "AGENT_BOT", "--app-token-env", "AGENT_APP", "--skip", "app_token", "--channels", "auto")
	if e != nil || res["steps"].([]any)[1].(map[string]any)["skipped"] != true {
		t.Fatal(e, raw)
	}
	if links := res["steps"].([]any)[1].(map[string]any)["links"].([]any); len(links) != 2 || links[0].(map[string]any)["url"] != "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general" {
		t.Fatal("skipped token step must link where to create it", raw)
	}
	if secrets, _ := os.ReadFile(filepath.Join(home, "secrets.env")); strings.Contains(string(secrets), app) {
		t.Fatal("skipped token kept")
	}
}

func TestSetupCommandFailures(t *testing.T) {
	fake := slacktest.New(t)
	before := setupHTTP
	setupHTTP = fake.Client()
	defer func() { setupHTTP = before }()
	home := t.TempDir()
	t.Setenv("CHATKEEPER_HOME", home)
	t.Setenv("CHATKEEPER_MODE", "server")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_APP_TOKEN", "")
	const app = "xapp-1-bogus-secret"
	t.Setenv("AGENT_BOT", "xoxb-synthetic")
	t.Setenv("AGENT_APP", app)
	fake.Overrides["apps.connections.open"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}
	res, raw, e := setupRun(t, home, "slack", "--dry-run", "--bot-token-env", "AGENT_BOT", "--app-token-env", "AGENT_APP", "--channels", "auto")
	if e == nil || res["ok"] != false || res["failed_step"] != "app_token" || !strings.Contains(raw, "rejected the app-level token") || strings.Contains(raw, app) {
		t.Fatal(e, raw)
	}
	if !strings.Contains(raw, "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general") {
		t.Fatal("failure must link the fix", raw)
	}
	for _, args := range [][]string{
		{"slack", "--bot-token", "xoxb-inline"},                         // secrets only via env
		{"slack", "--dry-run", "--bot-token-env", "UNSET_AGENT_VAR_X"},  // empty env var
		{"slack", "--channels", "auto", "--bot-token-env", "AGENT_BOT"}, // no workspace name
		{"slack", "--dry-run", "--skip", "channels"},                    // not optional
		{"nope"},
	} {
		if _, raw, e := setupRun(t, home, args...); e == nil || strings.Contains(raw, "xoxb-inline") {
			t.Fatal("accepted", args, raw)
		}
	}
}

func TestSetupPrintManifestAndHelp(t *testing.T) {
	var out bytes.Buffer
	if e := execute([]string{"setup", "slack", "--print-manifest"}, &out); e != nil || !strings.Contains(out.String(), "socket_mode_enabled: true") || !strings.Contains(out.String(), "reactions:read") {
		t.Fatal(e, out.String())
	}
	out.Reset()
	if e := execute([]string{"setup", "slack", "--help"}, &out); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"--bot-token-env ENV_VAR", "--app-token-env ENV_VAR", "--channels A,B", "--reactions A,B", "--skip app_token", "--print-manifest"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("help lacks", want, out.String())
		}
	}
	out.Reset()
	if e := execute([]string{"setup", "slack", "--print-agent-prompt"}, &out); e != nil || !strings.Contains(out.String(), "--dry-run") {
		t.Fatal(e)
	}
}
