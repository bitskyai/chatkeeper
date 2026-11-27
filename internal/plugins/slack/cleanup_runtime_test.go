//go:build !windows

package slack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/cleanup"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"sync"
	"testing"
)

func TestCleanupThenRuntimeRefreshAndBackfillNeverDrops(t *testing.T) {
	ctx := context.Background()
	fake := slacktest.New(t)
	var mu sync.Mutex
	msgs := []pluginapi.Message{{"ts": "100.000001", "user": "UBOT", "text": "root", "reply_count": 2, "reactions": []any{map[string]any{"name": "white_check_mark", "users": []string{"UOWNER"}, "count": 1}}}, {"ts": "101.000001", "thread_ts": "100.000001", "user": "UBOT", "text": "reply"}, {"ts": "102.000001", "thread_ts": "100.000001", "user": "UBOT", "text": "last"}}
	fake.Overrides["reactions.get"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if len(msgs) == 0 {
			fmt.Fprint(w, `{"ok":false,"error":"message_not_found"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "message", "channel": "CFIXTURE", "message": msgs[0]})
	}
	fake.Overrides["conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		roots := []pluginapi.Message{}
		if len(msgs) > 0 {
			roots = append(roots, msgs[0])
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": roots})
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": msgs})
	}
	fake.Overrides["users.info"] = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"user":{"id":"UBOT","profile":{"display_name":"Fixture bot"}}}`)
	}
	deletes := 0
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
	ws := pluginapi.Workspace{ID: "fixture", Platform: "slack", Storage: t.TempDir(), Channels: []string{"CFIXTURE"}, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`), Trigger: pluginapi.Trigger{Who: "owner", Owners: []string{"UOWNER"}, Reactions: []string{"white_check_mark"}}, Cleanup: pluginapi.CleanupPolicy{Enabled: true}}
	s, e := storage.Open(ws.Storage)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	env := pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "cleanup_token": "xoxb-synthetic"}, HTTP: fake.Client()}
	p := slack.Plugin{}
	session, e := p.NewSession(ctx, ws, env)
	if e != nil {
		t.Fatal(e)
	}
	runtime := core.New(ws, p, env, s)
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	if changed, e := runtime.Refresh(ctx, session, k); e != nil || !changed {
		t.Fatal("initial refresh", e)
	}
	before, _ := s.Read(ctx, k)
	d, e := p.NewCleanupDriver(ctx, ws, env)
	if e != nil {
		t.Fatal(e)
	}
	report := cleanup.Run(ctx, ws, s, d, true)
	if report.Threads[0].Status != "deleted" {
		t.Fatal(report)
	}
	if _, e = runtime.Refresh(ctx, session, k); e != nil {
		t.Fatal("refresh", e)
	}
	if _, e = runtime.Backfill(ctx, session); e != nil {
		t.Fatal("backfill", e)
	}
	after, e := s.Read(ctx, k)
	if e != nil || len(after.Messages) != 3 || len(after.SourceDeletedAt) != 3 {
		t.Fatal("archive shrank", e)
	}
	for name, b := range before.Documents {
		if string(after.Documents[name]) != string(b) {
			t.Fatal("original object changed", name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 3 || len(msgs) != 0 {
		t.Fatal("source delete count", deletes)
	}
}
