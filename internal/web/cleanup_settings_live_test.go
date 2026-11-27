//go:build live && !noslack && !windows

package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/chromedp/chromedp"
	"github.com/zalando/go-keyring"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only creates/deletes this invocation's tagged bot posts. All workspace identity
// and credentials come from explicit local environment; none are fixtures.
func TestRealCleanupSettingsUI(t *testing.T) {
	tokenPath, channel := os.Getenv("CHATKEEPER_GATE_TOKEN_FILE"), os.Getenv("CHATKEEPER_GATE_CHANNEL")
	if tokenPath == "" || channel == "" {
		t.Skip("explicit live-test credential file and channel required")
	}
	info, e := os.Stat(tokenPath)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("token file must be private")
	}
	b, e := os.ReadFile(tokenPath)
	if e != nil {
		t.Fatal("cannot read credential")
	}
	token := strings.TrimSpace(string(b))
	if !strings.HasPrefix(token, "xoxb-") {
		t.Fatal("bot-only gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	api := func(method string, p url.Values) map[string]any {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/"+method, strings.NewReader(p.Encode()))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("API transport failed")
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil || doc["ok"] != true {
			t.Fatalf("%s rejected: %v", method, doc["error"])
		}
		return doc
	}
	me := api("auth.test", nil)
	team, user := me["team_id"].(string), me["user_id"].(string)
	// Fail before creating source posts if the deletion credential cannot pass UI validation.
	settings, _ := json.Marshal(map[string]any{"team_id": team})
	ws := pluginapi.Workspace{ID: "live-cleanup", Platform: "slack", Settings: settings, Channels: []string{channel}, Trigger: pluginapi.Trigger{Who: "any", Reactions: []string{"white_check_mark"}}}
	env := pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": token}, HTTP: http.DefaultClient}
	if e = (slack.Plugin{}).TestCleanupCredential(ctx, ws, env, token, false); e != nil {
		t.Fatal(e)
	}
	prefix := fmt.Sprintf("[chatkeeper-cleanupui] %d", time.Now().UnixNano())
	targets := map[string]bool{}
	root := ""
	for i := 0; i < 2; i++ {
		p := url.Values{"channel": {channel}, "text": {fmt.Sprintf("%s message %d", prefix, i+1)}}
		if root != "" {
			p.Set("thread_ts", root)
		}
		doc := api("chat.postMessage", p)
		ts := doc["ts"].(string)
		targets[ts] = true
		if root == "" {
			root = ts
		}
	}
	api("reactions.add", url.Values{"channel": {channel}, "timestamp": {root}, "name": {"white_check_mark"}})
	api("reactions.add", url.Values{"channel": {channel}, "timestamp": {root}, "name": {"wastebasket"}})
	home := t.TempDir()
	archive := filepath.Join(home, "archive")
	ws.Storage = archive
	session, e := (slack.Plugin{}).NewSession(ctx, ws, env)
	if e != nil {
		t.Fatal(e)
	}
	r, e := session.Root(ctx, channel, root)
	if e != nil {
		t.Fatal(e)
	}
	snap, e := session.Snapshot(ctx, channel, r, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(snap.Messages) != len(targets) {
		t.Fatal("unexpected source inventory")
	}
	for _, m := range snap.Messages {
		if !targets[fmt.Sprint(m["ts"])] || m["user"] != user || !strings.HasPrefix(fmt.Sprint(m["text"]), prefix) {
			t.Fatal("refused foreign message")
		}
	}
	store, e := storage.Open(archive)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	key := pluginapi.Key{Channel: channel, RootTS: root}
	if _, e = store.Save(ctx, key, snap); e != nil {
		t.Fatal(e)
	}
	cfg := fmt.Sprintf("workspaces:\n - id: live-cleanup\n   platform: slack\n   team_id: %s\n   bot_token_env: CHATKEEPER_LIVE_UI_BOT\n   channels: [%s]\n   poll_interval: 86400\n   storage: {type: folder, path: %q}\n   trigger: {who: any, reactions: [white_check_mark]}\n", team, channel, archive)
	keyring.MockInit()
	t.Setenv("CHATKEEPER_LIVE_UI_BOT", token)
	a, e := web.NewApp(web.Options{Home: home, Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-live-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	// Configure a paused capture runtime so this live test's archive contains
	// exclusively the messages verified above, never pre-existing channel threads.
	a.Manager.SetPaused("live-cleanup", true)
	if e = os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	if e = a.Manager.Reload(); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	alloc, stop := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.NoSandbox)...)
	defer stop()
	browser, stop := chromedp.NewContext(alloc)
	defer stop()
	run := func(actions ...chromedp.Action) {
		t.Helper()
		if err := chromedp.Run(browser, actions...); err != nil {
			t.Fatal("browser action failed", err)
		}
	}
	run(chromedp.Navigate(srv.URL+"/login?token=synthetic-live-launch"), chromedp.Navigate(srv.URL+"/settings/cleanup?workspace=live-cleanup"), chromedp.WaitVisible(`input[name="enabled"]`), chromedp.Click(`input[name="enabled"]`), chromedp.Click(`input[name="owners"][value="`+user+`"]`), chromedp.SetValue(`input[name="min_age_hours"]`, "0"), chromedp.Click(`input[name="allow_zero_age"]`), chromedp.SetValue(`input[name="cleanup_token"]`, token), chromedp.Click(`button[value="test"]`), chromedp.WaitVisible(`[role="status"]`))
	var notice string
	run(chromedp.Text(`[role="status"]`, &notice))
	if !strings.Contains(notice, "Credential verified") {
		t.Fatal("credential test failed")
	}
	run(chromedp.Click(`button[value="save"]`), chromedp.WaitVisible(`//p[@role="status" and contains(., "Cleanup settings saved")]`, chromedp.BySearch), chromedp.Navigate(srv.URL+"/cleanup/?workspace=live-cleanup"), chromedp.Click(`form[action="/cleanup/preview"] button`), chromedp.WaitVisible(`form[action="/cleanup/apply"]`))
	var text string
	run(chromedp.Text("main", &text))
	if !strings.Contains(text, "dry-run") {
		t.Fatal("preview did not authorize tagged posts")
	}
	run(chromedp.SetValue(`input[name="confirm"]`, "live-cleanup"), chromedp.Click(`form[action="/cleanup/apply"] button`))
	deadline := time.Now().Add(60 * time.Second)
	for {
		after, err := store.Read(ctx, key)
		if err == nil && len(after.SourceDeletedAt) == len(targets) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deletion receipts missing")
		}
		time.Sleep(500 * time.Millisecond)
	}
	after, e := store.Read(ctx, key)
	if e != nil || len(after.Messages) != len(targets) {
		t.Fatal("archive not retained")
	}
	for _, ts := range []string{root} {
		doc := api("conversations.history", url.Values{"channel": {channel}, "oldest": {ts}, "latest": {ts}, "inclusive": {"true"}, "limit": {"1"}})
		messages, _ := doc["messages"].([]any)
		if len(messages) != 0 {
			t.Fatal("source root remains")
		}
	}
	// No real workspace IDs or token values are included in test receipts.
	t.Log("LIVE PASS: UI tested/saved bot-only credential, wastebasket approval, explicit immediate deletion; preview/apply deleted 2 own tagged posts; archive retained 2 messages with deletion markers")
}
