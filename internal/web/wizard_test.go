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
	c.expect("Create your Slack app", "only shows the bot token")
	c.do("POST", "/setup/step", nil)

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
	c.expect("Add the app-level token", "App-Level Tokens", "connections:write", "https://api.slack.com/apps/AFIXTURE/general", "Continue with polling only")
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
	c.expect("Try your first archive", ":wastebasket: or :fixture_parrot:", "Polling only", "Check now")

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
	c.expect("Try your first archive", "#fixture", "CFIXTURE", "bot is a member", "thread roots scanned", "on a reply (101.000001)", "Live events are off (polling only)")
	fake.MarkReply(false)
	fake.Mark(true)
	c.do("POST", "/setup/check", nil)
	c.expect("Your first thread is saved!", ":fixture_parrot: ×1", "archived", "Recent capture events")
	// Back from the live page returns to storage with the saved values.
	c.do("POST", "/setup/back", nil)
	c.expect("Where should we save your threads?", `value="fixture"`)
	c.do("POST", "/setup/step", url.Values{"workspace": {"fixture"}, "storage": {storage}})
	if !strings.Contains(c.body, `id="check-now"`) {
		t.Fatal("re-saving the same workspace did not return to the live test")
	}
	c.do("POST", "/setup/finish", nil)
	c.expect("Your threads, kept safe", "Recent capture events", "Archived thread CFIXTURE / 100.000001", "Polling only")
	c.do("POST", "/settings/check", url.Values{"workspace": {"fixture"}})
	c.expect("Your threads, kept safe", "Check now · fixture", "already archived (unchanged)")
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
