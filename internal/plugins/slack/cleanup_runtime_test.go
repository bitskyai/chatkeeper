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
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCleanupThenRuntimeRefreshAndBackfillNeverDrops(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) { cleanupRuntime(t, partial) })
	}
}
func cleanupRuntime(t *testing.T, partial bool) {
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
		if partial && deletes == 1 {
			w.WriteHeader(403)
			return
		}
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
	ws := pluginapi.Workspace{ID: "fixture", Platform: "slack", Storage: t.TempDir(), Channels: []string{"CFIXTURE"}, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`), Trigger: pluginapi.Trigger{Who: "owner", Owners: []string{"UOWNER"}, Reactions: []string{"white_check_mark"}}, Cleanup: pluginapi.CleanupPolicy{Approval: &pluginapi.Trigger{Who: "owner", Owners: []string{"UOWNER"}, Reactions: []string{"white_check_mark"}}, Enabled: true}}
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
	index := search.New(t.TempDir())
	defer index.Close()
	if e = index.Configure(ctx, map[string]search.Source{"fixture": {Path: ws.Storage, Content: p.Content()}}); e != nil {
		t.Fatal(e)
	}
	d, e := p.NewCleanupDriver(ctx, ws, env)
	if e != nil {
		t.Fatal(e)
	}
	report := cleanup.Run(ctx, ws, s, d, true, func(cleanup.Report) {
		if err := index.Sync(ctx, "fixture", k); err != nil {
			t.Error(err)
		}
	})
	if (!partial && report.Threads[0].Status != "deleted") || (partial && report.Threads[0].Status != "refused") {
		t.Fatal(report)
	}
	want := 3
	if partial {
		want = 1
	}
	viewer := web.New(map[string]web.Source{"fixture": {Store: s, Content: p.Content()}}, "")
	page := httptest.NewRecorder()
	viewer.ServeHTTP(page, httptest.NewRequest("GET", "/view/fixture/CFIXTURE/100.000001", nil))
	if page.Code != 200 || strings.Count(page.Body.String(), "Deleted at source:") != want {
		t.Fatalf("viewer missing markers: %d %s", page.Code, page.Body.String())
	}
	for _, op := range []string{"search", "sync", "rebuild", "fallback"} {
		if op == "sync" {
			e = index.Sync(ctx, "fixture", k)
		}
		if op == "rebuild" {
			e = index.Rebuild(ctx, "fixture")
		}
		if e != nil {
			t.Fatal(e)
		}
		reader := index
		if op == "fallback" {
			reader = search.NewReadOnly()
			defer reader.Close()
			if e = reader.Configure(ctx, map[string]search.Source{"fixture": {Path: ws.Storage, Content: p.Content()}}); e != nil {
				t.Fatal(e)
			}
		}
		found, err := reader.Search(ctx, search.Query{Q: "root"})
		if err != nil || len(found.Results) != 1 || !found.Results[0].SourceDeleted {
			t.Fatalf("search missing marker (%s): %+v %v", op, found, err)
		}
	}
	if partial {
		mu.Lock()
		msgs = append(msgs, pluginapi.Message{"ts": "103.000001", "thread_ts": k.RootTS, "user": "UBOT", "text": "new reply after cleanup"})
		msgs[0]["reply_count"] = len(msgs) - 1
		mu.Unlock()
	}
	if _, e = runtime.Refresh(ctx, session, k); e != nil {
		t.Fatal("refresh", e)
	}
	if _, e = runtime.Backfill(ctx, session); e != nil {
		t.Fatal("backfill", e)
	}
	after, e := s.Read(ctx, k)
	count := 3
	if partial {
		count = 4
	}
	if e != nil || len(after.Messages) != count || len(after.SourceDeletedAt) != want {
		t.Fatal("archive lost content/markers", e)
	}
	if partial {
		if after.Messages[3]["text"] != "new reply after cleanup" || after.Messages[2]["source_deleted_at"] == nil || after.CleanupStatus != "unknown/pending" {
			t.Fatalf("reconciliation failed: %+v", after)
		}
		rerun := cleanup.Run(ctx, ws, s, d, true)
		// The live-like transport still refuses additional deletes, but reconciliation
		// must reach the driver rather than wedge on the old journal or foreign reply.
		if rerun.Threads[0].Status != "refused" {
			t.Fatal(rerun)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if (!partial && (deletes != 3 || len(msgs) != 0)) || (partial && (deletes != 1 || len(msgs) != 3)) {
		t.Fatal("source delete count", deletes)
	}
}
