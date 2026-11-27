//go:build live && !windows

package slack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/cleanup"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Retain this gate's root for the post-cleanup reply. The real driver deletes
// its two replies; the wrapper refuses the root before any source request.
type gateKeepRoot struct {
	pluginapi.CleanupDriver
	root string
}

func (d gateKeepRoot) SetGuard(g func() error) {
	d.CleanupDriver.(interface{ SetGuard(func() error) }).SetGuard(g)
}
func (d gateKeepRoot) Delete(ctx context.Context, m pluginapi.ArchiveManifest, msg pluginapi.Message) error {
	if msg["ts"] == d.root {
		return &pluginapi.CleanupError{Status: "refused", Reason: "gate retains its root for the new-reply check"}
	}
	return d.CleanupDriver.Delete(ctx, m, msg)
}

func TestRealCleanupNewReplyGate(t *testing.T) {
	file := os.Getenv("CHATKEEPER_GATE_TOKEN_FILE")
	channel := os.Getenv("CHATKEEPER_GATE_CHANNEL")
	if file == "" || channel == "" {
		t.Skip("explicit real-workspace credential and channel required")
	}
	if channel != "C0CK1CY9EB1" {
		t.Fatal("gate channel not authorized")
	}
	info, e := os.Stat(file)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file must be 0600")
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		t.Fatal("credential unavailable")
	}
	token := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(token, "xoxb-") {
		t.Fatal("gate requires bot credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 60 * time.Second}
	api := func(method string, p url.Values) map[string]any {
		t.Helper()
		req, e := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/"+method, strings.NewReader(p.Encode()))
		if e != nil {
			t.Fatal("request unavailable")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, e := client.Do(req)
		if e != nil {
			t.Fatal("gate transport failed")
		}
		defer res.Body.Close()
		b, e := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		if e != nil {
			t.Fatal("gate read failed")
		}
		var doc map[string]any
		if json.Unmarshal(b, &doc) != nil || doc["ok"] != true {
			t.Fatalf("gate API %s failed (%d, %v)", method, res.StatusCode, doc["error"])
		}
		return doc
	}
	me := api("auth.test", nil)
	user := me["user_id"].(string)
	team := me["team_id"].(string)
	prefix := fmt.Sprintf("[chatkeeper-m5-gate] new-reply %d", time.Now().UnixNano())
	root := ""
	allowed := map[string]bool{}
	post := func(n int) string {
		p := url.Values{"channel": {channel}, "text": {fmt.Sprintf("%s message %d", prefix, n)}}
		if root != "" {
			p.Set("thread_ts", root)
		}
		doc := api("chat.postMessage", p)
		ts := doc["ts"].(string)
		allowed[ts] = true
		time.Sleep(1100 * time.Millisecond)
		return ts
	}
	root = post(1)
	post(2)
	post(3)
	api("reactions.add", url.Values{"channel": {channel}, "timestamp": {root}, "name": {"white_check_mark"}})
	settings, _ := json.Marshal(map[string]any{"team_id": team})
	ws := pluginapi.Workspace{ID: "gate", Platform: "slack", Settings: settings, Storage: t.TempDir(), Channels: []string{channel}, Trigger: pluginapi.Trigger{Who: "owner", Owners: []string{user}, Reactions: []string{"white_check_mark"}}, Cleanup: pluginapi.CleanupPolicy{Approval: &pluginapi.Trigger{Who: "owner", Owners: []string{user}, Reactions: []string{"white_check_mark"}}, Enabled: true}}
	p := slack.Plugin{}
	env := pluginapi.Env{HTTP: client, Credentials: pluginapi.Credentials{"bot_token": token, "cleanup_token": token}}
	session, e := p.NewSession(ctx, ws, env)
	if e != nil {
		t.Fatal("session unavailable")
	}
	s, e := storage.Open(ws.Storage)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	runtime := core.New(ws, p, env, s)
	key := pluginapi.Key{Channel: channel, RootTS: root}
	if _, e = runtime.Refresh(ctx, session, key); e != nil {
		t.Fatal(e)
	}
	snap, e := s.Read(ctx, key)
	if e != nil || len(snap.Messages) != 3 {
		t.Fatal("archive unavailable", e)
	}
	for _, m := range snap.Messages {
		if m["user"] != user || !allowed[m["ts"].(string)] || !strings.HasPrefix(m["text"].(string), prefix) {
			t.Fatal("gate refuses foreign message")
		}
	}
	d, e := p.NewCleanupDriver(ctx, ws, env)
	if e != nil {
		t.Fatal("driver unavailable")
	}
	partial := cleanup.Run(ctx, ws, s, gateKeepRoot{d, root}, true)
	if len(partial.Threads) != 1 || partial.Threads[0].Status != "refused" {
		t.Fatal(partial)
	}
	viewer := web.New(map[string]web.Source{"gate": {Store: s, Content: p.Content()}}, "")
	page := httptest.NewRecorder()
	viewer.ServeHTTP(page, httptest.NewRequest("GET", "/view/gate/"+channel+"/"+root, nil))
	if page.Code != 200 || strings.Count(page.Body.String(), "Deleted at source:") != 2 {
		t.Fatal("viewer missing cleanup markers", page.Code)
	}
	newTS := post(4)
	if changed, e := runtime.Refresh(ctx, session, key); e != nil || !changed {
		t.Fatal("new reply refresh failed", e)
	}
	snap, e = s.Read(ctx, key)
	if e != nil || len(snap.Messages) != 4 || len(snap.SourceDeletedAt) != 2 || snap.Messages[3]["ts"] != newTS {
		t.Fatal("refresh lost reply or deleted markers", e)
	}
	for _, m := range snap.Messages {
		if !allowed[m["ts"].(string)] || m["user"] != user || !strings.HasPrefix(m["text"].(string), prefix) {
			t.Fatal("gate refuses foreign message")
		}
	}
	finished := cleanup.Run(ctx, ws, s, d, true)
	if finished.Threads[0].Status != "deleted" {
		t.Fatal(finished)
	}
	snap, e = s.Read(ctx, key)
	if e != nil || len(snap.SourceDeletedAt) != 4 {
		t.Fatal("final receipts missing", e)
	}
	t.Log("LIVE NEW REPLY: cleanup=2 replies viewer=2 deleted markers post=1 refresh=4 archived/2 marked reconcile=success final cleanup=4/4 own prefixed bot messages")
}
