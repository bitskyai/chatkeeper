//go:build !noslack

package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func TestCaptureSearchEditDeletionAndNoopRepair(t *testing.T) {
	home := t.TempDir()
	fake := slacktest.New(t)
	fake.Mark(true)
	t.Setenv("SYNTHETIC_BOT", "xoxb-synthetic")
	a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	var state atomic.Int32
	root := func() map[string]any {
		text := "Original needle"
		if state.Load() > 0 {
			text = "Replacement needle"
		}
		return map[string]any{"ts": "100.000001", "user": "UOWNER", "text": text, "reply_count": 0, "reactions": []any{map[string]any{"name": "mark", "count": 1, "users": []string{"UOWNER"}}}}
	}
	fake.Overrides["reactions.get"] = func(w http.ResponseWriter, r *http.Request) {
		if state.Load() == 2 {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "message", "channel": "CFIXTURE", "message": root()})
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{root()}})
	}
	cfg := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  trigger: {reactions: [mark]}\n  storage: {path: %q}\n", filepath.Join(home, "data"))
	if e := os.WriteFile(a.opts.Config, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	a.Manager.SetPaused("fixture", true)
	if e := a.Manager.Reload(); e != nil {
		t.Fatal(e)
	}
	running := a.Manager.List()[0]
	ctx := context.Background()
	session, e := running.Entry.Plugin.NewSession(ctx, running.Entry.Workspace, running.Runtime.Env)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Stop(ctx)
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	refresh := func() {
		t.Helper()
		if _, e := running.Runtime.Refresh(ctx, session, k); e != nil {
			t.Fatal(e)
		}
	}
	assert := func(q string, history, deleted bool) {
		t.Helper()
		p, e := a.Manager.Search.Search(ctx, search.Query{Q: q, Author: "UOWNER"})
		if e != nil || len(p.Results) != 1 || p.Results[0].PriorVersions != history || p.Results[0].SourceDeleted != deleted {
			t.Fatal(q, p, e)
		}
	}
	refresh()
	assert("Original", false, false)
	state.Store(1)
	refresh()
	assert("Replacement", true, false)
	assert("Original", true, false)
	// Deliberately damage only the derived row. An unchanged capture must repair it.
	db, e := sql.Open("sqlite", filepath.Join(home, "state", "index", "fixture.db"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("DELETE FROM threads"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	refresh()
	assert("Replacement", true, false)
	state.Store(2)
	refresh()
	assert("Replacement", true, true)
}
