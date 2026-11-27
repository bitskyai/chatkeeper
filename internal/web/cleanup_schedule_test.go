//go:build !noslack && !windows

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	_ "github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupScheduleGatesAndPersistentActions(t *testing.T) {
	for _, mode := range []string{"default-off", "disabled", "interval", "credential", "apply"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			fake := slacktest.New(t)
			fake.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"ok":true,"team_id":"TFIXTURE","user_id":"UBOT"}`)
			}
			msg := pluginapi.Message{"ts": "100.000001", "user": "UBOT", "text": "test", "reply_count": 0, "reactions": []any{map[string]any{"name": "wastebasket", "users": []string{"UOWNER"}, "count": 1}}}
			fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{msg}})
			}
			fake.Overrides["chat.delete"] = func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"ok":true,"channel":"CFIXTURE","ts":"100.000001"}`)
			}
			t.Setenv("SCHEDULE_DELETE_TOKEN", "")
			a, e := NewApp(Options{Home: home, Host: "127.0.0.1", Mode: "desktop", HTTP: fake.Client()})
			if e != nil {
				t.Fatal(e)
			}
			defer a.Close()
			enabled, every := "true", ", every_hours: 1"
			switch mode {
			case "default-off":
				every = ""
			case "disabled":
				enabled = "false"
			case "apply":
				t.Setenv("SCHEDULE_DELETE_TOKEN", "xoxb-synthetic")
			}
			cfg := fmt.Sprintf("workspaces:\n - id: fixture\n   platform: slack\n   team_id: TFIXTURE\n   channels: [CFIXTURE]\n   storage: {type: folder, path: archive}\n   cleanup: {enabled: %s, token_env: SCHEDULE_DELETE_TOKEN, approval: {who: owner, owners: [UOWNER], reactions: [wastebasket]}%s}\n", enabled, every)
			path := filepath.Join(home, "config.yaml")
			if e = os.WriteFile(path, []byte(cfg), 0600); e != nil {
				t.Fatal(e)
			}
			now := time.Now()
			old := now.Add(-2 * time.Hour)
			if mode == "interval" {
				old = now
			}
			os.Chtimes(path, old, old)
			store, e := storage.Open(filepath.Join(home, "archive"))
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			snap := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, Platform: "slack", TeamID: "TFIXTURE", Channel: "CFIXTURE", RootTS: "100.000001", MessageCount: 1}, Messages: []pluginapi.Message{msg}, Users: map[string]pluginapi.Message{"UBOT": {"id": "UBOT"}}}
			if _, e = store.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, snap); e != nil {
				t.Fatal(e)
			}
			a.runScheduledCleanup(context.Background(), now)
			var count int
			a.db.QueryRow("SELECT COUNT(*) FROM cleanup_runs").Scan(&count)
			if mode == "default-off" || mode == "disabled" || mode == "interval" {
				if count != 0 || fake.Calls("chat.delete") != 0 {
					t.Fatal("schedule gate bypass")
				}
				return
			}
			if count != 1 {
				t.Fatal("missing persistent log", count)
			}
			var raw, status string
			a.db.QueryRow("SELECT report,status FROM cleanup_runs").Scan(&raw, &status)
			if status != "complete" {
				t.Fatal("incomplete log", status)
			}
			if mode == "credential" {
				if !strings.Contains(raw, "credential missing or invalid") || fake.Calls("chat.delete") != 0 {
					t.Fatal("missing credential gate", raw)
				}
			} else {
				if fake.Calls("chat.delete") != 1 || !strings.Contains(raw, `"status":"deleted"`) {
					t.Fatal("apply/receipt missing", raw)
				}
			}
			// Database last-run time survives scheduler invocation/restart.
			a.runScheduledCleanup(context.Background(), now)
			a.db.QueryRow("SELECT COUNT(*) FROM cleanup_runs").Scan(&count)
			if count != 1 {
				t.Fatal("interval not persisted")
			}
			if _, e = config.Load(path, false); e != nil {
				t.Fatal(e)
			}
		})
	}
}
