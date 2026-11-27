package slack_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func TestEndToEnd(t *testing.T) {
	for _, realtime := range []bool{true, false} {
		t.Run(map[bool]string{true: "socket", false: "polling"}[realtime], func(t *testing.T) {
			fake := slacktest.New(t)
			store, e := storage.Open(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			creds := pluginapi.Credentials{"bot_token": "xoxb-synthetic"}
			if realtime {
				creds["app_token"] = "xapp-synthetic"
			}
			ws := pluginapi.Workspace{ID: "fixture", Platform: "slack", Channels: []string{"CFIXTURE"}, Trigger: pluginapi.Trigger{Reactions: []string{"white_check_mark"}, Who: "owner", Owners: []string{"UOWNER"}}, PollInterval: 20 * time.Millisecond, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
			runtime := core.New(ws, slack.Plugin{}, pluginapi.Env{Credentials: creds, HTTP: fake.Client()}, store)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); runtime.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("runtime did not stop")
				}
			}()
			eventually(t, func() bool { return fake.Calls("conversations.history") > 0 })
			if realtime {
				eventually(t, func() bool { return fake.Connected() })
			}
			keys, e := store.Keys(ctx)
			if e != nil || len(keys) != 0 {
				t.Fatal("unmarked root captured", keys, e)
			}
			fake.Mark(true)
			if realtime {
				fake.Emit("TFIXTURE", map[string]any{"type": "reaction_added", "item": map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "100.000001"}})
			}
			eventually(t, func() bool { keys, e := store.Keys(ctx); return e == nil && len(keys) == 1 })
			snapshot, e := store.Read(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
			if e != nil || snapshot == nil || snapshot.Manifest.MessageCount != 2 {
				t.Fatal(snapshot, e)
			}
			w := httptest.NewRecorder()
			web.New(map[string]web.Source{"fixture": {Store: store, Content: slack.Hooks{}}}, "").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/view/fixture/CFIXTURE/100.000001", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "Synthetic root") || !strings.Contains(w.Body.String(), "Fixture mention") {
				t.Fatal(w.Code, w.Body)
			}
			if realtime {
				eventually(t, func() bool { return fake.Acknowledgements() > 0 })
			}
			fake.AssertCredentials(t)
			if runtime.Health().Realtime != realtime {
				t.Fatal(runtime.Health())
			}
		})
	}
}
func TestSocketFiltersAndAckFullQueue(t *testing.T) {
	fake := slacktest.New(t)
	ws := pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
	s, e := (slack.Plugin{}).NewSession(context.Background(), ws, pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "app_token": "xapp-synthetic"}})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	events := make(chan pluginapi.Event, 10)
	go func() {
		done <- s.Start(ctx, func(event pluginapi.Event) bool {
			if event.Kind != "backfill" {
				events <- event
			}
			return false
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("socket did not stop")
		}
	}()
	eventually(t, fake.Connected)
	raw := map[string]any{"type": "reaction_added", "item": map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "100.000001"}}
	fake.Emit("TFOREIGN", raw)
	fake.Emit("TFIXTURE", raw)
	eventually(t, func() bool { return fake.Acknowledgements() == 2 })
	select {
	case e := <-events:
		if e.Kind != "reaction" {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	select {
	case e := <-events:
		t.Fatal("foreign event", e)
	default:
	}
}
func TestCompleteSnapshotAndReuse(t *testing.T) {
	fake := slacktest.New(t)
	fake.Mark(true)
	ws := pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
	s, e := (slack.Plugin{}).NewSession(context.Background(), ws, pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}})
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if _, e = s.VerifyIdentity(ctx); e != nil {
		t.Fatal(e)
	}
	root, e := s.Root(ctx, "CFIXTURE", "100.000001")
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.Snapshot(ctx, "CFIXTURE", root, nil)
	if e != nil {
		t.Fatal(e)
	}
	store, _ := storage.Open(t.TempDir())
	defer store.Close()
	if _, e = store.Save(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: root.TS}, v); e != nil {
		t.Fatal(e)
	}
	prev, e := store.Read(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: root.TS})
	if e != nil {
		t.Fatal(e)
	}
	before := fake.Calls("download")
	if _, e = s.Snapshot(ctx, "CFIXTURE", root, prev); e != nil {
		t.Fatal(e)
	}
	if fake.Calls("download") != before {
		t.Fatal("cached file redownloaded")
	}
	b, _ := json.Marshal(v.Manifest)
	if !strings.Contains(string(b), "sha256") {
		t.Fatal("missing manifest")
	}
}

// Exercise the real Slack adapter, core refresh and disk archive together.
func TestNeverDropLifecycle(t *testing.T) {
	fake := slacktest.New(t)
	state := "initial"
	root := func() map[string]any {
		count := 1
		if state == "reply-deleted" {
			count = 0
		}
		return map[string]any{"ts": "100.000001", "user": "UOWNER", "text": "Synthetic root", "reply_count": count, "reactions": []any{map[string]any{"name": "mark", "count": 1, "users": []string{"UOWNER"}}}}
	}
	fake.Overrides["reactions.get"] = func(w http.ResponseWriter, r *http.Request) {
		if state == "root-deleted" {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "message", "channel": "CFIXTURE", "message": root()})
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		messages := []any{root()}
		if state != "reply-deleted" {
			text := "Synthetic reply"
			if state == "edited" {
				text = "Edited synthetic reply"
			}
			messages = append(messages, map[string]any{"ts": "101.000001", "thread_ts": "100.000001", "user": "UAUTHOR", "text": text})
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
	}
	ws := pluginapi.Workspace{ID: "fixture", Channels: []string{"CFIXTURE"}, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`), Trigger: pluginapi.Trigger{Reactions: []string{"mark"}}}
	env := pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}}
	session, e := (slack.Plugin{}).NewSession(context.Background(), ws, env)
	if e != nil {
		t.Fatal(e)
	}
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	runtime := core.New(ws, slack.Plugin{}, env, store)
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	refresh := func() *pluginapi.Snapshot {
		t.Helper()
		if _, e := runtime.Refresh(context.Background(), session, k); e != nil {
			t.Fatal(e)
		}
		v, e := store.Read(context.Background(), k)
		if e != nil || v == nil {
			t.Fatal(e)
		}
		if e := (slack.Hooks{}).Validate(context.Background(), *v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	initial := refresh()
	prior, _ := storage.Canonical(initial.Messages[1])
	hash := sha256.Sum256(prior)
	state = "reply-deleted"
	deleted := refresh()
	if len(deleted.Messages) != 2 || deleted.Messages[1]["source_deleted_at"] == nil {
		t.Fatal("reply dropped")
	}
	marker := deleted.Messages[1]["source_deleted_at"]
	if refresh().Messages[1]["source_deleted_at"] != marker {
		t.Fatal("deletion time reset")
	}
	state = "initial"
	reappeared := refresh()
	if reappeared.Messages[1]["source_deleted_at"] != nil || reappeared.Messages[1]["source_events"] == nil {
		t.Fatal("silent reappearance")
	}
	state = "edited"
	edited := refresh()
	versions := edited.Messages[1]["versions"].([]any)
	version := versions[0].(map[string]any)
	if version["sha256"] != hex.EncodeToString(hash[:]) {
		t.Fatal("old content hash lost", version)
	}
	state = "root-deleted"
	gone := refresh()
	for _, m := range gone.Messages {
		if m["source_deleted_at"] == nil {
			t.Fatal("missing root/reply marker")
		}
	}
	state = "edited"
	restored := refresh()
	if restored.Messages[0]["source_deleted_at"] != nil || len(restored.Messages) != 2 {
		t.Fatal("root not restored")
	}
	if e := store.RemoveArchive(context.Background(), k); e != nil {
		t.Fatal(e)
	}
	if v, e := store.Read(context.Background(), k); e != nil || v != nil {
		t.Fatal("explicit removal failed")
	}
}

func TestSocketDeletionRefreshNeverDrops(t *testing.T) {
	fake := slacktest.New(t)
	fake.Mark(true)
	var phase atomic.Int32
	root := func() map[string]any {
		count := 1
		if phase.Load() == 1 {
			count = 0
		}
		return map[string]any{"ts": "100.000001", "user": "UOWNER", "text": "Synthetic root", "reply_count": count, "reactions": []any{map[string]any{"name": "mark", "count": 1, "users": []string{"UOWNER"}}}}
	}
	fake.Overrides["conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{root()}})
	}
	fake.Overrides["reactions.get"] = func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 2 {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "message", "channel": "CFIXTURE", "message": root()})
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		messages := []any{root()}
		if phase.Load() != 1 {
			messages = append(messages, map[string]any{"ts": "101.000001", "thread_ts": "100.000001", "user": "UAUTHOR", "text": "Synthetic reply"})
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
	}
	ws := pluginapi.Workspace{ID: "fixture", Channels: []string{"CFIXTURE"}, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`), Trigger: pluginapi.Trigger{Reactions: []string{"mark"}}, PollInterval: time.Hour}
	env := pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "app_token": "xapp-synthetic"}}
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	runtime := core.New(ws, slack.Plugin{}, env, store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	eventually(t, func() bool { v, e := store.Read(ctx, k); return e == nil && v != nil && fake.Connected() })
	phase.Store(1)
	fake.Emit("TFIXTURE", map[string]any{"type": "message", "subtype": "message_deleted", "channel": "CFIXTURE", "previous_message": map[string]any{"ts": "101.000001", "thread_ts": "100.000001"}})
	eventually(t, func() bool {
		v, e := store.Read(ctx, k)
		return e == nil && v != nil && len(v.Messages) == 2 && v.Messages[1]["source_deleted_at"] != nil
	})
	phase.Store(2)
	fake.Emit("TFIXTURE", map[string]any{"type": "message", "subtype": "message_deleted", "channel": "CFIXTURE", "previous_message": map[string]any{"ts": "100.000001"}})
	eventually(t, func() bool {
		v, e := store.Read(ctx, k)
		return e == nil && v != nil && len(v.Messages) == 2 && v.Messages[0]["source_deleted_at"] != nil
	})
	phase.Store(0)
	fake.Emit("TFIXTURE", map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "100.000001"})
	eventually(t, func() bool {
		v, e := store.Read(ctx, k)
		return e == nil && v != nil && v.Messages[0]["source_deleted_at"] == nil && v.Messages[0]["source_events"] != nil
	})
}
