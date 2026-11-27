package slack_test

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"testing"
	"time"
)

func runCapture(t *testing.T, fake *slacktest.Server, realtime bool, trigger []string, interval time.Duration) (storage.Store, func()) {
	t.Helper()
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	creds := pluginapi.Credentials{"bot_token": "xoxb-synthetic"}
	if realtime {
		creds["app_token"] = "xapp-synthetic"
	}
	ws := pluginapi.Workspace{ID: "fixture", Platform: "slack", Channels: []string{"CFIXTURE"}, Trigger: pluginapi.Trigger{Reactions: trigger, Who: "any", Match: "any"}, PollInterval: interval, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
	runtime := core.New(ws, slack.Plugin{}, pluginapi.Env{Credentials: creds, HTTP: fake.Client()}, store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtime.Run(ctx) }()
	return store, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("runtime did not stop")
		}
		store.Close()
	}
}

func keyCount(store storage.Store) int {
	keys, e := store.Keys(context.Background())
	if e != nil {
		return -1
	}
	return len(keys)
}

// TestSocketReplyReactionArchivesThread: in Socket Mode a trigger reaction on a
// reply is mapped to its thread root (one cached lookup) and archives the thread.
func TestSocketReplyReactionArchivesThread(t *testing.T) {
	fake := slacktest.New(t)
	store, stop := runCapture(t, fake, true, []string{"bookmark"}, time.Hour)
	defer stop()
	eventually(t, func() bool { return fake.Connected() && fake.Calls("conversations.history") > 0 })
	fake.SetReaction("bookmark")
	// A non-trigger reaction on the reply costs no root lookup.
	fake.Emit("TFIXTURE", map[string]any{"type": "reaction_added", "reaction": "eyes", "item": map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "101.000001"}})
	eventually(t, func() bool { return fake.Acknowledgements() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := fake.Calls("conversations.replies/root"); n != 0 {
		t.Fatal("lookup for non-trigger reaction", n)
	}
	fake.MarkReply(true)
	fake.Emit("TFIXTURE", map[string]any{"type": "reaction_added", "reaction": "bookmark", "item": map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "101.000001"}})
	eventually(t, func() bool { return keyCount(store) == 1 })
	snapshot, e := store.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	if e != nil || snapshot == nil || snapshot.Manifest.MessageCount != 2 {
		t.Fatal(snapshot, e)
	}
	// The reply-to-root mapping is cached.
	fake.Emit("TFIXTURE", map[string]any{"type": "reaction_removed", "reaction": "bookmark", "item": map[string]any{"type": "message", "channel": "CFIXTURE", "ts": "101.000001"}})
	eventually(t, func() bool { return fake.Acknowledgements() >= 3 })
	time.Sleep(50 * time.Millisecond)
	if n := fake.Calls("conversations.replies/root"); n != 1 {
		t.Fatal("root lookups", n)
	}
}

// TestPollingIgnoresReplyReactions: polling reads only root reactions (one
// history call per channel per poll), so a reply reaction alone never archives.
func TestPollingIgnoresReplyReactions(t *testing.T) {
	fake := slacktest.New(t)
	fake.SetReaction("bookmark")
	fake.MarkReply(true)
	store, stop := runCapture(t, fake, false, []string{"bookmark"}, 10*time.Millisecond)
	defer stop()
	eventually(t, func() bool { return fake.Calls("conversations.history") > 3 })
	if keyCount(store) != 0 || fake.Calls("conversations.replies") != 0 {
		t.Fatal("polling archived or read replies for a reply-only reaction", keyCount(store), fake.Calls("conversations.replies"))
	}
}

// TestSkinToneReactionsMatchBaseName: "+1::skin-tone-3" on the root matches a
// configured "+1"; a configured tone matches only that tone.
func TestSkinToneReactionsMatchBaseName(t *testing.T) {
	for _, tc := range []struct {
		trigger, reaction string
		archived          bool
	}{
		{"+1", "+1::skin-tone-3", true},
		{"+1::skin-tone-3", "+1::skin-tone-3", true},
		{"+1::skin-tone-2", "+1::skin-tone-3", false},
	} {
		t.Run(tc.trigger+" vs "+tc.reaction, func(t *testing.T) {
			fake := slacktest.New(t)
			fake.SetReaction(tc.reaction)
			fake.Mark(true)
			store, stop := runCapture(t, fake, false, []string{tc.trigger}, 10*time.Millisecond)
			defer stop()
			if tc.archived {
				eventually(t, func() bool { return keyCount(store) == 1 })
				return
			}
			eventually(t, func() bool { return fake.Calls("conversations.history") > 3 })
			if keyCount(store) != 0 {
				t.Fatal("tone mismatch archived")
			}
		})
	}
	if _, e := (slack.Plugin{}).ParseWorkspace(context.Background(), []byte(`{"team_id":"TFIXTURE","trigger":{"reactions":["+1::skin-tone-3","+1"]}}`), nil); e != nil {
		t.Fatal("skin-tone reaction rejected", e)
	}
	if _, e := (slack.Plugin{}).ParseWorkspace(context.Background(), []byte(`{"team_id":"TFIXTURE","trigger":{"reactions":["+1::skin-tone-9"]}}`), nil); e == nil {
		t.Fatal("invalid tone accepted")
	}
}

func TestReplyRecoveryReadsAllPagesAndReportsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			fake := slacktest.New(t)
			fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm()
				if r.Form.Get("cursor") == "" {
					fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"100.000001","reply_count":201}],"response_metadata":{"next_cursor":"next"}}`)
					return
				}
				if fail {
					fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
					return
				}
				fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"101.000001","thread_ts":"100.000001","reactions":[{"name":"bookmark","users":["UOWNER"],"count":1}]}]}`)
			}
			ws := pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
			s, e := (slack.Plugin{}).NewSession(context.Background(), ws, pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}})
			if e != nil {
				t.Fatal(e)
			}
			replies, e := s.(pluginapi.ReplyInspector).ReplyReactions(context.Background(), []pluginapi.RootRef{{Channel: "CFIXTURE", RootTS: "100.000001", Replies: 201}}, []string{"bookmark"})
			if fail {
				if e == nil {
					t.Fatal("reply recovery swallowed a failed page")
				}
				return
			}
			if e != nil || len(replies) != 1 || replies[0].TS != "101.000001" {
				t.Fatal("recovery missed reply after first page", replies, e)
			}
		})
	}
}

func TestFullReplyRecoveryBoundsEachThreadRead(t *testing.T) {
	fake := slacktest.New(t)
	fake.Overrides["conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"messages":[`)
		for i := 100; i < 110; i++ {
			if i > 100 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"ts":"%d.000001","reply_count":1}`, i)
		}
		fmt.Fprint(w, `]}`)
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		r.ParseForm()
		fmt.Fprintf(w, `{"ok":true,"messages":[{"ts":%q}]}`, r.Form.Get("ts"))
	}
	ws := pluginapi.Workspace{ID: "fixture", Platform: "slack", Channels: []string{"CFIXTURE"}, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`), Trigger: pluginapi.Trigger{Who: "any", Reactions: []string{"bookmark"}}}
	env := pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "app_token": "xapp-synthetic"}}
	s, e := (slack.Plugin{}).NewSession(context.Background(), ws, env)
	if e != nil {
		t.Fatal(e)
	}
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	runtime := core.New(ws, slack.Plugin{}, env, store)
	runtime.HookTimeout = 100 * time.Millisecond
	if _, e := runtime.Backfill(context.Background(), s); e != nil {
		t.Fatal("full recovery timed out across individually healthy thread reads", e)
	}
}
