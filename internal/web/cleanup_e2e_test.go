//go:build !noslack

package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/chromedp/chromedp"
	"github.com/zalando/go-keyring"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrowserCleanupPreviewApply(t *testing.T) {
	if os.Getenv("CHATKEEPER_E2E") != "1" {
		t.Skip("set CHATKEEPER_E2E=1 for Chrome")
	}
	keyring.MockInit()
	t.Setenv("CHATKEEPER_CLEANUP_TOKEN", "xoxb-synthetic")
	t.Setenv("SLACK_BOT_TOKEN", "")
	fake := slacktest.New(t)
	home := t.TempDir()
	dir := filepath.Join(home, "archive")
	s, e := storage.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	msgs := []pluginapi.Message{{"ts": "1000000000.000001", "user": "UBOT", "text": "root", "reply_count": 2, "reactions": []any{map[string]any{"name": "white_check_mark", "users": []any{"UOWNER"}, "count": 1}}}, {"ts": "1000000001.000001", "thread_ts": "1000000000.000001", "user": "UBOT", "text": "reply"}, {"ts": "1000000002.000001", "thread_ts": "1000000000.000001", "user": "UBOT", "text": "last"}}
	snapshot := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, Platform: "slack", TeamID: "TFIXTURE", Channel: "CFIXTURE", RootTS: "1000000000.000001", MessageCount: 3}, Messages: msgs, Users: map[string]pluginapi.Message{"UBOT": {"id": "UBOT"}}, Blobs: map[string]pluginapi.Blob{}}
	if _, e = s.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "1000000000.000001"}, snapshot); e != nil {
		t.Fatal(e)
	}
	cfg := fmt.Sprintf("workspaces:\n  - id: fixture\n    platform: slack\n    team_id: TFIXTURE\n    channels: [CFIXTURE]\n    storage: {type: folder, path: %q}\n    trigger: {who: owner, owners: [UOWNER], reactions: [white_check_mark]}\n    cleanup: {enabled: false}\n", dir)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0600)
	var mu sync.Mutex
	deletes := 0
	fake.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"team_id":"TFIXTURE","user_id":"UBOT"}`)
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": msgs})
	}
	fake.Overrides["chat.delete"] = func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		ts := r.FormValue("ts")
		for i, m := range msgs {
			if m["ts"] == ts {
				msgs = append(msgs[:i], msgs[i+1:]...)
				deletes++
				break
			}
		}
		if len(msgs) > 0 {
			msgs[0]["reply_count"] = len(msgs) - 1
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "CFIXTURE", "ts": ts})
	}
	a, e := web.NewApp(web.Options{Home: home, Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	server := httptest.NewServer(a)
	defer server.Close()
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.NoSandbox)...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	run := func(actions ...chromedp.Action) {
		t.Helper()
		if e := chromedp.Run(ctx, actions...); e != nil {
			t.Fatal(e)
		}
	}
	click := func(selector string) {
		t.Helper()
		if _, e := chromedp.RunResponse(ctx, chromedp.Click(selector, chromedp.ByQuery)); e != nil {
			t.Fatal(e)
		}
	}
	run(chromedp.Navigate(server.URL+"/login?token=synthetic-launch"), chromedp.Navigate(server.URL+"/cleanup/?workspace=fixture"), chromedp.WaitVisible("input[name=enabled]"), chromedp.Click("input[name=enabled]"))
	click("form[action='/cleanup/enable'] button")
	click("form[action='/cleanup/preview'] button")
	run(chromedp.WaitVisible("table"), chromedp.WaitVisible("input[name=confirm]"))
	var body string
	run(chromedp.Text("main", &body))
	if strings.Count(body, "dry-run") < 3 {
		t.Fatal("preview missing targets", body)
	}
	mu.Lock()
	count := deletes
	mu.Unlock()
	if count != 0 {
		t.Fatal("preview deleted source")
	}
	run(chromedp.SendKeys("input[name=confirm]", "fixture"))
	click("form[action='/cleanup/apply'] button")
	run(chromedp.Text("main", &body))
	if !strings.Contains(body, "deleted") {
		t.Fatal(body)
	}
	mu.Lock()
	count = deletes
	mu.Unlock()
	if count != 3 {
		t.Fatal("delete count", count)
	}
	snap, e := s.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "1000000000.000001"})
	if e != nil || len(snap.Messages) != 3 || len(snap.SourceDeletedAt) != 3 {
		t.Fatal("browser cleanup lost archive", e)
	}
}
