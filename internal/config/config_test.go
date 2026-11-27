//go:build !noslack

package config

import (
	_ "github.com/bitskyai/chatkeeper/internal/plugins/all"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `server:
  host: 127.0.0.1
  port: 8787
workspaces:
  - id: fixture
    platform: slack
    team_id: TFIXTURE
    bot_token_env: FIXTURE_BOT_TOKEN
    channels: [CFIXTURE]
    storage:
      type: folder
      path: archive-data
    trigger:
      reactions: [white_check_mark]
      who: owner
      owners: [UOWNER]
`

func load(t *testing.T, s string, secrets bool) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
	return Load(p, secrets)
}
func TestLoad(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	c, e := load(t, valid, true)
	if e != nil || len(c.Workspaces) != 1 {
		t.Fatal(c, e)
	}
	if c.Workspaces[0].Workspace.PollInterval.Seconds() != 120 {
		t.Fatal(c)
	}
	if c.Workspaces[0].Credentials["bot_token"] != "xoxb-synthetic" {
		t.Fatal("missing scoped credential")
	}
}
func TestOffline(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "")
	if _, e := load(t, valid, false); e != nil {
		t.Fatal(e)
	}
	if _, e := load(t, valid, true); e == nil {
		t.Fatal("missing secret accepted")
	}
}
func TestInvalidConfig(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	for name, s := range map[string]string{
		"unknown": valid + "unknown: true\n", "duplicate key": valid + "server: {}\n", "unknown workspace": strings.Replace(valid, "team_id:", "unexpected:", 1), "reserved": strings.Replace(valid, "id: fixture", "id: api", 1), "bad id": strings.Replace(valid, "id: fixture", "id: ../escape", 1), "bad team": strings.ReplaceAll(valid, "TFIXTURE", "wrong"), "empty reactions": strings.Replace(valid, "[white_check_mark]", "[]", 1), "duplicate reactions": strings.Replace(valid, "[white_check_mark]", "[white_check_mark, white_check_mark]", 1), "no owner": strings.Replace(valid, "[UOWNER]", "[]", 1), "bad who": strings.Replace(valid, "who: owner", "who: unknown", 1), "poll zero": valid + "    poll_interval: 0\n", "poll bool": valid + "    poll_interval: true\n", "storage type": strings.Replace(valid, "type: folder", "type: git", 1), "inline token": valid + "    bot_token: secret\n", "cleanup enabled": valid + "    cleanup: {enabled: true}\n", "duplicate channels": strings.Replace(valid, "[CFIXTURE]", "[CFIXTURE, CFIXTURE]", 1), "bad channels": strings.Replace(valid, "[CFIXTURE]", "[../escape]", 1), "multiple documents": valid + "---\n{}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := load(t, s, true); e == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestOverlap(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	second := strings.SplitN(valid, "workspaces:\n", 2)[1]
	second = strings.Replace(second, "id: fixture", "id: second", 1)
	for _, path := range []string{"archive-data", "archive-data/nested", "."} {
		if _, e := load(t, valid+strings.Replace(second, "path: archive-data", "path: "+path, 1), true); e == nil {
			t.Fatal(path)
		}
	}
}
func TestMixedTransportAndCleanupSecretIsolation(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("FIXTURE_APP_TOKEN", "xapp-synthetic")
	t.Setenv("FIXTURE_CLEANUP_TOKEN", "synthetic-cleanup-secret")
	second := strings.SplitN(valid, "workspaces:\n", 2)[1]
	second = strings.Replace(second, "id: fixture", "id: second", 1)
	second = strings.Replace(second, "path: archive-data", "path: second-data", 1)
	c, e := load(t, valid+"    cleanup: {enabled: false, token_env: FIXTURE_CLEANUP_TOKEN}\n"+second+"    app_token_env: FIXTURE_APP_TOKEN\n", true)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Workspaces) != 2 || c.Workspaces[0].Credentials["app_token"] != "" || c.Workspaces[1].Credentials["app_token"] != "xapp-synthetic" {
		t.Fatal("transport credentials mixed")
	}
	for _, entry := range c.Workspaces {
		for _, v := range entry.Credentials {
			if v == "synthetic-cleanup-secret" {
				t.Fatal("archive resolved cleanup credential")
			}
		}
	}
}
func TestAppTokenValidation(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	for _, token := range []string{"bad", "xapp-", "xapp-with space", "xapp-with\nline"} {
		t.Setenv("SLACK_APP_TOKEN", token)
		if _, e := load(t, valid, true); e == nil {
			t.Fatal("invalid app token accepted")
		}
	}
}
func TestBearerConfig(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	t.Setenv("CHATKEEPER_API_TOKEN", "synthetic-bearer")
	c, e := load(t, valid, true)
	if e != nil || c.Token["bearer"] != "synthetic-bearer" {
		t.Fatal(e)
	}
	t.Setenv("CHATKEEPER_API_TOKEN", "bad token")
	if _, e = load(t, valid, true); e == nil {
		t.Fatal("invalid bearer accepted")
	}
	if _, e = load(t, valid, false); e != nil {
		t.Fatal("offline resolved bearer")
	}
}
func TestReservedWorkspaceNames(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	for _, name := range []string{"api", "mcp", "healthz", "view"} {
		if _, e := load(t, strings.Replace(valid, "id: fixture", "id: "+name, 1), true); e == nil {
			t.Fatal(name)
		}
	}
}

func TestPublicBindRequiresToken(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	t.Setenv("CHATKEEPER_API_TOKEN", "")
	t.Setenv("THREADKEEP_API_TOKEN", "")
	for _, host := range []string{"0.0.0.0", "::", "example.invalid"} {
		if _, e := load(t, strings.Replace(valid, "127.0.0.1", "\""+host+"\"", 1), true); e == nil {
			t.Errorf("public bind %s without token accepted", host)
		}
	}
}

func TestBindTokenAndLoopback(t *testing.T) {
	t.Setenv("FIXTURE_BOT_TOKEN", "xoxb-synthetic")
	t.Setenv("THREADKEEP_API_TOKEN", "")
	for _, token := range []string{"", "synthetic-bearer"} {
		t.Setenv("CHATKEEPER_API_TOKEN", token)
		for _, host := range []string{"127.0.0.1", "::1", "0.0.0.0", "::"} {
			_, e := load(t, strings.Replace(valid, "127.0.0.1", "\""+host+"\"", 1), true)
			if (e == nil) != (token != "" || Loopback(host)) {
				t.Fatal(host, token != "", e)
			}
		}
	}
}
