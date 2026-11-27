//go:build !noslack

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
)

func TestFreshAndConcurrentBackfillIndex(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprint(concurrent), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CHATKEEPER_HOME", home)
			t.Setenv("SYNTHETIC_BOT", "xoxb-synthetic")
			fake := slacktest.New(t)
			fake.Mark(true)
			path := filepath.Join(home, "data")
			cfg := filepath.Join(home, "config.yaml")
			data := fmt.Sprintf("workspaces:\n- id: fixture\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  channels: [CFIXTURE]\n  trigger: {reactions: [white_check_mark]}\n  storage: {path: %q}\n", path)
			if e := os.WriteFile(cfg, []byte(data), 0600); e != nil {
				t.Fatal(e)
			}
			var owner *web.App
			if concurrent {
				var e error
				owner, e = web.NewApp(web.Options{Home: home, Config: cfg, Host: "127.0.0.1", Mode: "server", HTTP: fake.Client()})
				if e != nil {
					t.Fatal(e)
				}
				defer owner.Close()
				owner.Manager.SetPaused("fixture", true)
				if e := owner.Manager.Reload(); e != nil {
					t.Fatal(e)
				}
			}
			// The command's HTTP client is injectable so this exercises the actual CLI
			// lifecycle against a synthetic endpoint rather than live source credentials.
			before := backfillHTTP
			backfillHTTP = fake.Client()
			defer func() { backfillHTTP = before }()
			var out bytes.Buffer
			if e := execute([]string{"backfill", "--config", cfg, "--mode", "server"}, &out); e != nil {
				t.Fatal(e)
			}
			st, e := storage.Open(path)
			if e != nil {
				t.Fatal(e)
			}
			keys, e := st.Keys(context.Background())
			st.Close()
			if e != nil || len(keys) != 1 {
				t.Fatal(keys, e)
			}
			if concurrent {
				p, e := owner.Manager.Search.Search(context.Background(), search.Query{Q: "Synthetic"})
				if e != nil || len(p.Results) != 1 {
					t.Fatal("server index stale after CLI capture", p, e)
				}
			} else {
				if _, e := os.Stat(filepath.Join(home, "state", "index", "fixture.db")); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
