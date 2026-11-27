//go:build !noslack

package web_test

import (
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/zalando/go-keyring"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type wizardClient struct {
	t      *testing.T
	base   string
	http   *http.Client
	csrf   string
	body   string
	header http.Header
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (c *wizardClient) do(method, path string, form url.Values) string {
	c.t.Helper()
	var res *http.Response
	var e error
	if method == "GET" {
		res, e = c.http.Get(c.base + path)
	} else {
		if form == nil {
			form = url.Values{}
		}
		form.Set("csrf", c.csrf)
		res, e = c.http.PostForm(c.base+path, form)
	}
	if e != nil {
		c.t.Fatal(e)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	c.body = html.UnescapeString(string(b))
	c.header = res.Header
	if m := csrfPattern.FindStringSubmatch(string(b)); m != nil {
		c.csrf = m[1]
	}
	if res.StatusCode != 200 {
		c.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, c.body)
	}
	return c.body
}
func (c *wizardClient) title() string {
	m := regexp.MustCompile(`<h1>([^<]*)</h1>`).FindStringSubmatch(c.body)
	if m == nil {
		return ""
	}
	return m[1]
}
func (c *wizardClient) expect(title string, contains ...string) {
	c.t.Helper()
	if c.title() != title {
		c.t.Fatalf("want page %q, got %q", title, c.title())
	}
	for _, s := range contains {
		if !strings.Contains(c.body, s) {
			c.t.Fatalf("page %q lacks %q", title, s)
		}
	}
}

// TestWizardBackTestSkipPickerAndCheckNow drives the whole wizard over HTTP
// against the fake Slack server (no browser), covering Neo's alpha.2 feedback.
func TestWizardBackTestSkipPickerAndCheckNow(t *testing.T) {
	keyring.MockInit()
	fake := slacktest.New(t)
	// A token from another Slack app connects, but its hello names that app.
	fake.Overrides["apps.connections.open"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer xapp-other-app" {
			_, _ = w.Write([]byte(`{"ok":true,"url":"wss://socket.slack.com/socket?app=AOTHER"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"url":"wss://socket.slack.com/socket"}`))
	}
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	os.WriteFile(cfgPath, []byte("workspaces: []\n"), 0600)
	a, e := web.NewApp(web.Options{Home: home, Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	server := httptest.NewServer(a)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	c := &wizardClient{t: t, base: server.URL, http: &http.Client{Jar: jar}}
	c.do("GET", "/login?token=synthetic-launch", nil)
	c.do("POST", "/setup/start", url.Values{"platform": {"slack"}})
	c.expect("Create your Slack app", "only shows the bot token", "Set up with an AI agent", `data-copy="agent-prompt"`, "chatkeeper setup slack --print-manifest")
	c.do("POST", "/setup/step", nil)
	if strings.Contains(c.body, "Set up with an AI agent") {
		t.Fatal("agent panel belongs to the first step")
	}

	// 3. Test connection itemizes workspace, bot user and scopes without advancing.
	c.expect("Paste your bot token", "Test connection", "← Back")
	c.do("POST", "/setup/step", url.Values{"bot_token": {"xoxb-synthetic"}, "action": {"test"}})
	c.expect("Paste your bot token", "Fixture (TFIXTURE)", "@fixturebot (UBOT)", "Scope reactions:read", "missing (optional: custom workspace emoji")
	if strings.Contains(c.body, "xoxb-synthetic") {
		t.Fatal("secret rendered")
	}
	// 2. The secret is kept server-side and shown as saved; blank keeps it.
	c.expect("Paste your bot token", "Saved · leave blank to keep it")
	c.do("POST", "/setup/step", url.Values{"bot_token": {""}, "action": {"continue"}})

	// 1. App-level token guidance deep-links the app's Basic Information page.
	c.expect("Add the app-level token", "App-Level Tokens", "connections:write", "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general", "Continue with polling only")
	// The token step shows only a one-line bot summary, not the bot step's checklist.
	c.expect("Add the app-level token", "Bot token OK · Fixture", `data-copy="inline-0"`, "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/socket-mode")
	for _, stale := range []string{"Scope reactions:read", "@fixturebot (UBOT)", "Fixture (TFIXTURE)", "api.slack.com/apps/"} {
		if strings.Contains(c.body, stale) {
			t.Fatal("app-level token step shows", stale)
		}
	}
	c.do("POST", "/setup/step", url.Values{"app_token": {""}, "action": {"continue"}})
	c.expect("Add the app-level token", "or choose Continue with polling only")
	c.do("POST", "/setup/step", url.Values{"app_token": {"xapp-synthetic"}, "action": {"test"}})
	c.expect("Add the app-level token", "connected (hello received)", "Saved · leave blank")
	c.do("POST", "/setup/step", url.Values{"action": {"continue"}})
	c.expect("Choose channels", "#fixture")
	c.do("POST", "/setup/step", url.Values{"channels": {"CFIXTURE"}})

	// 4. Reaction picker: glyph grid with categories, custom emoji, multi-select.
	c.expect("Choose reactions", `data-picker`, "🗑️", `data-group-section="Objects"`, `data-group-section="Custom"`, "https://emoji.slack-edge.com/TFIXTURE/fixture_parrot/0.png")
	if csp := c.header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' https://emoji.slack-edge.com") {
		t.Fatal("custom emoji images blocked", csp)
	}
	c.do("POST", "/setup/step", url.Values{"reactions": {"wastebasket", "fixture_parrot", "bookmark, my_custom"}, "match": {"any"}})
	c.expect("Connection ready")

	// 2. Back keeps the non-secret choices and secrets.
	c.do("POST", "/setup/back", nil)
	c.expect("Choose reactions", `value="wastebasket" checked`, `value="fixture_parrot" checked`, `value="bookmark" checked`, `value="my_custom"`)
	c.do("POST", "/setup/back", nil)
	c.expect("Choose channels", `value="CFIXTURE" checked`)
	c.do("POST", "/setup/back", nil)
	c.expect("Add the app-level token", "Saved · leave blank")
	// Explicit polling fallback clears the app token.
	c.do("POST", "/setup/step", url.Values{"action": {"skip"}})
	c.expect("Choose channels", "Polling only: ChatKeeper checks")
	c.do("POST", "/setup/step", url.Values{"channels": {"CFIXTURE"}})
	c.do("POST", "/setup/step", url.Values{"reactions": {"wastebasket", "fixture_parrot"}, "match": {"any"}})
	c.do("POST", "/setup/step", nil)
	c.expect("Where should we save your threads?", "← Back")
	storage := filepath.Join(home, "data", "fixture")
	c.do("POST", "/setup/step", url.Values{"workspace": {"fixture"}, "storage": {storage}})
	c.expect("Try your first archive", ":wastebasket: or :fixture_parrot:", "Polling only", "Check now", "Reactions on replies need live events")

	cfg, e := config.LoadWithResolver(cfgPath, true, func(k string) string { v, _, _ := a.Secrets.Get(k); return v })
	if e != nil || len(cfg.Workspaces) != 1 {
		t.Fatal(cfg, e)
	}
	ws := cfg.Workspaces[0]
	if strings.Join(ws.Workspace.Trigger.Reactions, ",") != "wastebasket,fixture_parrot" || ws.Workspace.Trigger.Match != "any" || ws.Credentials["app_token"] != "" || ws.Workspace.AutoChannels {
		t.Fatalf("%+v %v", ws.Workspace, ws.Credentials["app_token"] != "")
	}

	// 5. Check now explains a reaction on a reply, then archives once it is on the root.
	fake.SetReaction("fixture_parrot")
	fake.MarkReply(true)
	waitRunning(t, a)
	c.do("POST", "/setup/check", nil)
	c.expect("Try your first archive", "#fixture", "CFIXTURE", "bot is a member", "thread roots scanned", "on a reply (101.000001)", "Polling only reads reactions on the first message of a thread", "Live events are off (polling only)", "first message of each thread only")
	fake.MarkReply(false)
	fake.Mark(true)
	c.do("POST", "/setup/check", nil)
	c.expect("Your first thread is saved!", ":fixture_parrot: ×1", "archived", "Recent capture events")
	// Back from the live page returns to storage with the saved values.
	c.do("POST", "/setup/back", nil)
	c.expect("Where should we save your threads?", `value="fixture"`, "readonly", "cannot change after saving")
	// A different name submitted after Back must not create a second workspace
	// sharing the same credentials (duplicate capture workers).
	c.do("POST", "/setup/step", url.Values{"workspace": {"renamed"}, "storage": {storage}})
	if !strings.Contains(c.body, `id="check-now"`) {
		t.Fatal("re-saving the same workspace did not return to the live test")
	}
	cfg, e = config.LoadWithResolver(cfgPath, true, func(k string) string { v, _, _ := a.Secrets.Get(k); return v })
	if e != nil || len(cfg.Workspaces) != 1 || cfg.Workspaces[0].Workspace.ID != "fixture" {
		t.Fatalf("rename after Back duplicated the workspace: %+v %v", cfg, e)
	}
	if n := len(a.Manager.List()); n != 1 {
		t.Fatalf("want 1 capture worker, got %d", n)
	}
	c.do("POST", "/setup/finish", nil)
	c.expect("Clean up Slack (optional)", "Skip / Finish setup")
	c.do("POST", "/setup/finish", url.Values{"cleanup_done": {"true"}})
	c.expect("Your threads, kept safe", "Recent capture events", "Archived thread CFIXTURE / 100.000001", "Polling only")
	c.do("POST", "/settings/check", url.Values{"workspace": {"fixture"}})
	c.expect("Your threads, kept safe", "Check now · fixture", "already archived (unchanged)")
	// Enable instant capture reopens the app-level token step for the
	// polling-only workspace, rejects another app's token and saves a good one.
	upgrade := "/settings/upgrade?step=app_token&workspace=fixture"
	c.do("GET", "/", nil)
	c.expect("Your threads, kept safe", "Enable instant capture", upgrade)
	// Only steps that declare an Upgrade can be reopened.
	for _, bad := range []string{"/settings/upgrade?step=bot_token&workspace=fixture", "/settings/upgrade?step=app_token&workspace=missing"} {
		if res, e := c.http.Get(c.base + bad); e != nil || res.StatusCode != http.StatusNotFound {
			t.Fatal(bad, res, e)
		} else {
			res.Body.Close()
		}
	}
	c.do("GET", upgrade, nil)
	c.expect("Add the app-level token", "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/general", "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/socket-mode", `data-copy="inline-1"`, ">connections:write</code>", "Save and enable")
	c.do("POST", "/settings/upgrade", url.Values{"workspace": {"fixture"}, "step": {"app_token"}, "app_token": {"xapp-other-app"}, "action": {"test"}})
	c.expect("Add the app-level token", "different Slack app (AOTHER) than your bot token (AFIXTURE)")
	if strings.Contains(c.body, "xapp-other-app") {
		t.Fatal("app token echoed")
	}
	c.do("POST", "/settings/upgrade", url.Values{"workspace": {"fixture"}, "step": {"app_token"}, "app_token": {"xapp-other-app"}, "action": {"save"}})
	c.expect("Add the app-level token", "different Slack app")
	c.do("POST", "/settings/upgrade", url.Values{"workspace": {"fixture"}, "step": {"app_token"}, "app_token": {"xapp-synthetic"}, "action": {"save"}})
	c.expect("Your threads, kept safe", "Live events")
	if strings.Contains(c.body, "Enable instant capture") {
		t.Fatal("upgrade still offered after saving the token")
	}
	cfg, e = config.LoadWithResolver(cfgPath, true, func(k string) string { v, _, _ := a.Secrets.Get(k); return v })
	if e != nil || cfg.Workspaces[0].Credentials["app_token"] != "xapp-synthetic" {
		t.Fatal("app token not saved", e)
	}
	// The viewer shows glyphs and marks trigger reactions.
	c.do("GET", "/view/fixture/CFIXTURE/100.000001", nil)
	if !strings.Contains(c.body, "reaction trigger") || !strings.Contains(c.body, ":fixture_parrot:") {
		t.Fatal("viewer lacks trigger reaction", c.body)
	}
}

func waitRunning(t *testing.T, a *web.App) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range a.Manager.List() {
			if r.Runtime.Health().State == "running" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("runtime did not start")
}
