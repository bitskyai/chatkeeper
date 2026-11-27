//go:build !noslack && !windows

package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/zalando/go-keyring"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCleanupApplySurvivesRequestCancellation(t *testing.T) {
	keyring.MockInit()
	t.Setenv("CHATKEEPER_CLEANUP_TOKEN", "xoxb-synthetic")
	t.Setenv("SLACK_BOT_TOKEN", "")
	home := t.TempDir()
	dir := filepath.Join(home, "archive")
	s, e := storage.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	msgs := []pluginapi.Message{{"ts": "100.000001", "user": "UBOT", "text": "root", "reply_count": 0, "reactions": []any{map[string]any{"name": "white_check_mark", "users": []string{"UOWNER"}, "count": 1}}}}
	snap := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, Platform: "slack", TeamID: "TFIXTURE", Channel: "CFIXTURE", RootTS: "100.000001", MessageCount: 1}, Messages: msgs, Users: map[string]pluginapi.Message{"UBOT": {"id": "UBOT"}}}
	if _, e = s.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, snap); e != nil {
		t.Fatal(e)
	}
	cfg := fmt.Sprintf("workspaces:\n - id: fixture\n   platform: slack\n   team_id: TFIXTURE\n   channels: [CFIXTURE]\n   storage: {type: folder, path: %q}\n   trigger: {who: owner, owners: [UOWNER], reactions: [white_check_mark]}\n   cleanup: {enabled: true, approval: {who: owner, owners: [UOWNER], reactions: [white_check_mark]}}\n", dir)
	if e = os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	var a *web.App
	fake := slacktest.New(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fake.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"ok":true,"team_id":"TFIXTURE","user_id":"UBOT"}`)
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": msgs})
	}
	fake.Overrides["chat.delete"] = func(w http.ResponseWriter, r *http.Request) {
		if !a.Manager.IsSuspended("fixture") {
			t.Error("manual deletion ran alongside capture")
		}
		fmt.Fprint(w, `{"ok":true,"channel":"CFIXTURE","ts":"100.000001"}`)
	}
	a, e = web.NewApp(web.Options{Home: home, Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	// Always unblock the worker, including assertion failures.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	send := func(ctx context.Context, method, path string, v url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(v.Encode())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	ctx := context.Background()
	page := send(ctx, "GET", "/cleanup/?workspace=fixture", nil)
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal(page.Body.String())
	}
	csrf := match[1]
	preview := send(ctx, "POST", "/cleanup/preview", url.Values{"csrf": {csrf}, "workspace": {"fixture"}})
	match = regexp.MustCompile(`name="digest" value="([^"]+)"`).FindStringSubmatch(preview.Body.String())
	if len(match) != 2 {
		t.Fatal(preview.Body.String())
	}
	request, cancel := context.WithCancel(ctx)
	applied := send(request, "POST", "/cleanup/apply", url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "confirm": {"fixture"}, "digest": {match[1]}})
	cancel()
	if applied.Code != 303 {
		t.Fatal(applied.Code, applied.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background driver did not start")
	}
	page = send(ctx, "GET", "/cleanup/?workspace=fixture", nil)
	if !strings.Contains(page.Body.String(), "Cleanup running") {
		t.Fatal(page.Body.String())
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		page = send(ctx, "GET", "/cleanup/?workspace=fixture", nil)
		if !strings.Contains(page.Body.String(), "Cleanup running") && strings.Contains(page.Body.String(), ">deleted<") {
			return
		}
	}
	t.Fatal("background cleanup did not finish", page.Body.String())
}
