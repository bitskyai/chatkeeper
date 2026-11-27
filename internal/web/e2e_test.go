//go:build !noslack

package web_test

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	_ "github.com/bitskyai/chatkeeper/internal/plugins/all"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/chromedp/chromedp"
	"github.com/zalando/go-keyring"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserWizard(t *testing.T) {
	if os.Getenv("CHATKEEPER_E2E") != "1" {
		t.Skip("set CHATKEEPER_E2E=1 to run headless Chrome")
	}
	keyring.MockInit()
	for _, mode := range []string{"desktop", "server"} {
		t.Run(mode, func(t *testing.T) {
			fake := slacktest.New(t)
			var authMode atomic.Int32
			fake.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer xoxb-invalid" {
					fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
					return
				}
				scopes := "channels:history,channels:read,groups:history,groups:read,reactions:read,files:read,users:read"
				if authMode.Load() == 1 {
					scopes = "channels:read"
				}
				w.Header().Set("X-OAuth-Scopes", scopes)
				fmt.Fprint(w, `{"ok":true,"team":"Fixture","team_id":"TFIXTURE","user":"fixturebot","user_id":"UBOT","bot_id":"BFIXTURE"}`)
			}
			fake.Overrides["conversations.list"] = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("cursor") == "synthetic-next" {
					fmt.Fprint(w, `{"ok":true,"channels":[{"id":"COTHER","name":"needs-invite","is_member":false}],"response_metadata":{"next_cursor":""}}`)
				} else {
					fmt.Fprint(w, `{"ok":true,"channels":[{"id":"CFIXTURE","name":"fixture","is_member":true}],"response_metadata":{"next_cursor":"synthetic-next"}}`)
				}
			}
			home := t.TempDir()
			cfgPath := filepath.Join(home, "config.yaml")
			os.WriteFile(cfgPath, []byte("# Kept by the UI\nworkspaces: []\n"), 0600)
			a, e := web.NewApp(web.Options{Home: home, Mode: mode, Host: "127.0.0.1", SetupCode: "synthetic-setup-code", LaunchToken: "synthetic-launch-token", HTTP: fake.Client()})
			if e != nil {
				t.Fatal(e)
			}
			defer a.Close()
			server := httptest.NewServer(a)
			defer server.Close()
			alloc, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.NoSandbox, chromedp.WindowSize(1280, 900))...)
			defer cancel()
			ctx, cancel := chromedp.NewContext(alloc)
			defer cancel()
			ctx, cancel = context.WithTimeout(ctx, 150*time.Second)
			defer cancel()
			run := func(actions ...chromedp.Action) {
				t.Helper()
				if e := chromedp.Run(ctx, actions...); e != nil {
					t.Fatal(e)
				}
			}
			title := func(s string) chromedp.Action {
				return chromedp.Poll(fmt.Sprintf("document.querySelector('h1')?.textContent === %q", s), nil)
			}
			click := func(selector string) {
				t.Helper()
				if _, e := chromedp.RunResponse(ctx, chromedp.Click(selector, chromedp.ByQuery)); e != nil {
					t.Fatal(e)
				}
			}
			// Continue is always the last action; Back and Test connection come first.
			next := func(want string) { click("main .actions button[type=submit]:last-of-type"); run(title(want)) }
			back := func(want string) { click(`main button[formaction="/setup/back"]`); run(title(want)) }
			has := func(text string) chromedp.Action {
				return chromedp.Poll(fmt.Sprintf("document.body.innerText.includes(%q)", text), nil)
			}
			capture := func(name string) {
				t.Helper()
				if os.Getenv("CHATKEEPER_SCREENSHOTS") == "" {
					return
				}
				var b []byte
				run(chromedp.FullScreenshot(&b, 90))
				dir := filepath.Join("..", "..", "docs", "screenshots")
				if e := os.MkdirAll(dir, 0755); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(dir, mode+"-"+name+".png"), b, 0644); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "server" {
				run(chromedp.Navigate(server.URL+"/bootstrap"), title("Welcome to ChatKeeper"))
				capture("bootstrap")
				run(chromedp.SendKeys("input[name=code]", "synthetic-setup-code"), chromedp.SendKeys("input[name=password]", "synthetic-password"))
				click("main button[type=submit]")
				run(title("What would you like to connect?"))
			} else {
				run(chromedp.Navigate(a.LaunchURL(server.URL)), title("What would you like to connect?"))
			}
			capture("platforms")
			click("form:has(input[value=slack]) button")
			run(title("Create your Slack app"), has("only shows the bot token"))
			capture("create-app")
			next("Paste your bot token")
			capture("tokens")
			// Item 3: Test connection reports itemized results without advancing.
			run(chromedp.SendKeys("input[name=bot_token]", "xoxb-invalid"))
			click(`button[value=test]`)
			run(title("Paste your bot token"), has("Bot token is invalid"))
			capture("bad-token")
			run(chromedp.SendKeys("input[name=bot_token]", "xoxb-synthetic"))
			authMode.Store(1)
			click(`button[value=test]`)
			run(has("Missing scopes"), has("files:read"))
			capture("missing-scopes")
			authMode.Store(0)
			click(`button[value=test]`)
			run(has("@fixturebot (UBOT)"), has("Fixture (TFIXTURE)"), has("Saved · leave blank"))
			capture("test-connection")
			next("Add the app-level token")
			// Item 1: the app-level token guidance deep-links the app's settings.
			run(chromedp.Poll(`document.querySelector('a[href="https://api.slack.com/apps/AFIXTURE/general"]') !== null`, nil), has("App-Level Tokens"), has("connections:write"))
			capture("app-token")
			// Item 2: Back keeps the bot token server-side.
			back("Paste your bot token")
			run(has("Saved · leave blank"))
			next("Add the app-level token")
			if mode == "desktop" {
				click(`button[value=skip]`)
				run(title("Choose channels"), has("Polling only"))
			} else {
				run(chromedp.SendKeys("input[name=app_token]", "xapp-synthetic"))
				click(`button[value=test]`)
				run(has("connected (hello received)"))
				capture("socket-test")
				next("Choose channels")
			}
			capture("channels")
			run(chromedp.SetAttributeValue("input[name=channels][value=COTHER]", "checked", "checked"))
			next("Choose channels")
			run(has("Invite the bot"))
			run(chromedp.Evaluate(`document.querySelector("input[name=channels][value=COTHER]").checked = false; document.querySelector("input[name=channels][value=CFIXTURE]").checked = true`, nil))
			next("Choose reactions")
			// Item 4: searchable glyph grid with categories and custom emoji.
			run(chromedp.Poll(`document.querySelectorAll("label.emoji").length > 1800 && document.querySelector('[data-group-section="Custom"] img') !== null`, nil))
			run(chromedp.SendKeys("input[data-picker-search]", "wastebasket"))
			run(chromedp.Poll(`document.querySelector('label.emoji:has(input[value="wastebasket"])').offsetParent !== null && document.querySelector('label.emoji:has(input[value="grinning"])').offsetParent === null`, nil))
			// Element clicks, not coordinates: filtering reflows the grid.
			tap := func(selector string) {
				run(chromedp.Evaluate(fmt.Sprintf("document.querySelector(%q).click()", selector), nil))
			}
			tap(`label.emoji:has(input[value="wastebasket"])`)
			run(chromedp.Evaluate(`document.querySelector("input[data-picker-search]").value = ""`, nil), chromedp.SendKeys("input[data-picker-search]", "parrot"))
			tap(`label.emoji:has(input[value="fixture_parrot"])`)
			run(chromedp.Poll(`document.querySelector("[data-picker-selected]").innerText.includes(":wastebasket:") && document.querySelector("[data-picker-selected]").innerText.includes(":fixture_parrot:")`, nil))
			// Starting from the default, remove white_check_mark so only the two picks remain.
			run(chromedp.Evaluate(`document.querySelector('input[name=reactions][value="white_check_mark"]').checked = false`, nil))
			capture("reaction")
			next("Connection ready")
			back("Choose reactions")
			run(chromedp.Poll(`document.querySelector('input[value="wastebasket"]').checked && document.querySelector('input[value="fixture_parrot"]').checked`, nil))
			back("Choose channels")
			run(chromedp.Poll(`document.querySelector('input[name=channels][value=CFIXTURE]').checked`, nil))
			next("Choose reactions")
			next("Connection ready")
			next("Where should we save your threads?")
			storage := filepath.Join(home, "data", "fixture")
			run(chromedp.SetValue("input[name=workspace]", "fixture"), chromedp.SetValue("input[name=storage]", "/data/chatkeeper/fixture"))
			capture("storage")
			if mode == "server" {
				next("Where should we save your threads?")
				run(has("inside the server data root"))
				if _, err := os.Stat(filepath.Join(home, "secrets.env")); !os.IsNotExist(err) {
					t.Fatal("invalid storage submission saved credentials")
				}
			}
			run(chromedp.SetValue("input[name=storage]", storage))
			next("Try your first archive")
			capture("live-test")
			fake.SetReaction("fixture_parrot")
			if mode == "desktop" {
				// Items 5 and 6: polling-only Check now explains, then archives.
				fake.MarkReply(true)
				click("#check-now")
				run(has("on a reply (101.000001)"), has("bot is a member"), has("Live events are off (polling only)"))
				capture("check-now-log")
				fake.MarkReply(false)
				fake.Mark(true)
				click("#check-now")
			} else {
				fake.Mark(true)
				deadline := time.Now().Add(5 * time.Second)
				for (!fake.Connected() || fake.Calls("conversations.history") == 0) && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if !fake.Connected() {
					t.Fatal("Socket Mode did not connect")
				}
				fake.Emit("TFIXTURE", map[string]any{"type": "reaction_added", "item": map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "100.000001"}})
			}
			run(chromedp.Poll("document.querySelector('.archive-list a') !== null", nil, chromedp.WithPollingInterval(250*time.Millisecond)))
			capture("done")
			click(".archive-list a")
			run(has("Synthetic reply"), has(":fixture_parrot: 1 ★"))
			capture("viewer")
			run(chromedp.SetValue(`form[action="/search"] input[name="q"]`, "Synthetic", chromedp.ByQuery))
			click(`form[action="/search"] button`)
			run(chromedp.Poll("document.querySelector('h1')?.textContent === 'Search archives' && document.body.innerText.includes('CFIXTURE')", nil))
			capture("search")
			click("article h2 a")
			run(has("Synthetic reply"))
			run(chromedp.Navigate(server.URL+"/"), title("Your threads, kept safe"), has("Recent capture events"), has("Archived thread CFIXTURE / 100.000001"))
			capture("dashboard")
			run(chromedp.SetValue(`form[action="/search"] input[name="q"]`, "Synthetic", chromedp.ByQuery))
			click(`form[action="/search"] button`)
			run(chromedp.Poll("document.querySelector('article h2 a') !== null", nil))

			c, e := config.LoadWithResolver(cfgPath, true, func(k string) string { v, _, _ := a.Secrets.Get(k); return v })
			if e != nil || len(c.Workspaces) != 1 {
				t.Fatal(c, e)
			}
			_, source, err := a.Secrets.Get("CHATKEEPER_66697874757265_BOT_TOKEN")
			wantSource := "file"
			if mode == "desktop" {
				wantSource = "keychain"
			}
			if err != nil || source != wantSource {
				t.Fatalf("credential backend: %s, %v", source, err)
			}
			if mode == "desktop" {
				if _, err := os.Stat(filepath.Join(home, "secrets.env")); !os.IsNotExist(err) {
					t.Fatal("keychain credentials leaked to file")
				}
			}
			b, _ := os.ReadFile(cfgPath)
			if !strings.Contains(string(b), "# Kept by the UI") {
				t.Fatal("comment lost")
			}
			db, e := sql.Open("sqlite", filepath.Join(home, "state", "app.db"))
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			rows, e := db.Query("SELECT data FROM setup_sessions")
			if e != nil {
				t.Fatal(e)
			}
			defer rows.Close()
			for rows.Next() {
				var data string
				_ = rows.Scan(&data)
				if strings.Contains(data, "xoxb-") || strings.Contains(data, "xapp-") {
					t.Fatal("wizard persisted secret")
				}
			}
		})
	}
}
