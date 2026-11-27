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
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// This opt-in gate creates one root and two replies. Its archive contains only
// messages created by this invocation; no pre-existing source message is targeted.
func TestRealCleanupGate(t *testing.T) {
	tokenPath := os.Getenv("CHATKEEPER_GATE_TOKEN_FILE")
	channel := os.Getenv("CHATKEEPER_GATE_CHANNEL")
	if tokenPath == "" || channel == "" {
		t.Skip("explicit real-workspace credentials and channel required")
	}
	if channel != "C0CK1CY9EB1" {
		t.Fatal("gate channel not authorized")
	}
	info, e := os.Stat(tokenPath)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file must be 0600")
	}
	b, e := os.ReadFile(tokenPath)
	if e != nil {
		t.Fatal("credential unavailable")
	}
	token := strings.TrimSpace(string(b))
	if !strings.HasPrefix(token, "xoxb-") {
		t.Fatal("gate requires bot credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 60 * time.Second}
	api := func(method string, p url.Values) map[string]any {
		t.Helper()
		req, e := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/"+method, strings.NewReader(p.Encode()))
		if e != nil {
			t.Fatal("request creation failed")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, e := client.Do(req)
		if e != nil {
			t.Fatal("gate API transport failed")
		}
		defer res.Body.Close()
		body, e := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		if e != nil {
			t.Fatal("gate read failed")
		}
		var doc map[string]any
		if json.Unmarshal(body, &doc) != nil || doc["ok"] != true {
			t.Fatalf("gate API %s failed (status %d, code %v)", method, res.StatusCode, doc["error"])
		}
		return doc
	}
	me := api("auth.test", nil)
	team := me["team_id"].(string)
	user := me["user_id"].(string)
	targets := []string{}
	root := ""
	prefix := fmt.Sprintf("[chatkeeper-m5-gate] %d", time.Now().UnixNano())
	for i := 0; i < 3; i++ {
		p := url.Values{"channel": {channel}, "text": {fmt.Sprintf("%s message %d", prefix, i+1)}}
		if root != "" {
			p.Set("thread_ts", root)
		}
		doc := api("chat.postMessage", p)
		ts := doc["ts"].(string)
		if i == 0 {
			root = ts
		}
		targets = append(targets, ts)
		time.Sleep(1100 * time.Millisecond)
	}
	api("reactions.add", url.Values{"channel": {channel}, "timestamp": {root}, "name": {"white_check_mark"}})
	settings, _ := json.Marshal(map[string]any{"team_id": team})
	ws := pluginapi.Workspace{ID: "gate", Platform: "slack", Settings: settings, Storage: t.TempDir(), Channels: []string{channel}, Trigger: pluginapi.Trigger{Who: "owner", Owners: []string{user}, Reactions: []string{"white_check_mark"}}, Cleanup: pluginapi.CleanupPolicy{Enabled: true, MinAge: 0}}
	plugin := slack.Plugin{}
	session, e := plugin.NewSession(ctx, ws, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": token}, HTTP: client})
	if e != nil {
		t.Fatal("session unavailable")
	}
	r, e := session.Root(ctx, channel, root)
	if e != nil {
		t.Fatal("cannot read posted root")
	}
	snap, e := session.Snapshot(ctx, channel, r, nil)
	if e != nil {
		t.Fatal("cannot archive posted thread", e)
	}
	if len(snap.Messages) != 3 {
		t.Fatal("unexpected message inventory")
	}
	for _, m := range snap.Messages {
		if m["user"] != user || !slices.Contains(targets, m["ts"].(string)) || !strings.HasPrefix(m["text"].(string), prefix) {
			t.Fatal("gate refused foreign message")
		}
	}
	s, e := storage.Open(ws.Storage)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	key := pluginapi.Key{Channel: channel, RootTS: root}
	if _, e = s.Save(ctx, key, snap); e != nil {
		t.Fatal(e)
	}
	before, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	preview := cleanup.Run(ctx, ws, s, nil, false)
	if len(preview.Threads) != 1 || len(preview.Threads[0].Results) != 3 || preview.Threads[0].Status != "dry-run" {
		t.Fatal("dry-run failed")
	}
	for _, r := range preview.Threads[0].Results {
		if !slices.Contains(targets, r.TS) {
			t.Fatal("foreign dry-run target")
		}
	}
	d, e := plugin.NewCleanupDriver(ctx, ws, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}, HTTP: client})
	if e != nil {
		t.Fatal("driver unavailable")
	}
	report := cleanup.Run(ctx, ws, s, d, true)
	if len(report.Threads) != 1 || report.Threads[0].Status != "deleted" {
		t.Fatalf("apply blocked: %s", report.Threads[0].Reason)
	}
	after, e := s.Read(ctx, key)
	if e != nil || len(after.Messages) != 3 || len(after.SourceDeletedAt) != 3 {
		t.Fatal("archive retention/markers failed", e)
	}
	for name, raw := range before.Documents {
		if string(after.Documents[name]) != string(raw) {
			t.Fatal("archive hash changed")
		}
	}
	rerun := cleanup.Run(ctx, ws, s, d, true)
	if rerun.Threads[0].Status != "cleaned" {
		t.Fatal("rerun failed")
	}
	path := filepath.Join(ws.Storage, "archive", channel, root, "messages.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(path, []byte("[]"), 0600)
	tampered := cleanup.Run(ctx, ws, s, d, true)
	if tampered.Threads[0].Status != "blocked" {
		t.Fatal("tampered snapshot accepted")
	}
	os.WriteFile(path, raw, 0600)
	liveRoot, e := session.Root(ctx, channel, root)
	if e != nil || liveRoot.TS != "" {
		t.Fatal("source root still present or unverified")
	}
	empty := pluginapi.Snapshot{Manifest: before.Manifest, Users: before.Users, Blobs: map[string]pluginapi.Blob{}}
	empty.Manifest.Schema = 2
	empty.Manifest.MessageCount = 0
	if _, e = s.Save(ctx, key, empty); e != nil {
		t.Fatal("empty refresh failed", e)
	}
	roots, e := session.History(ctx, channel, root)
	if e != nil {
		t.Fatal("backfill read failed", e)
	}
	for _, r := range roots {
		if r.RootTS == root {
			t.Fatal("deleted root found by backfill")
		}
	}
	runtime := core.New(ws, plugin, pluginapi.Env{HTTP: client}, s)
	if _, e = runtime.Refresh(ctx, session, key); e != nil {
		t.Fatal("runtime refresh failed", e)
	}
	if _, e = runtime.Backfill(ctx, gateHistory{Session: session, root: root}); e != nil {
		t.Fatal("runtime backfill failed", e)
	}
	final, e := s.Read(ctx, key)
	if e != nil || len(final.Messages) != 3 || len(final.SourceDeletedAt) != 3 {
		t.Fatal("refresh/backfill lost archive", e)
	}
	t.Log("LIVE GATE: preview=3 apply=3/3 retained=3/3 marked=3/3 hashes=unchanged rerun=0 tamper=blocked refresh/backfill=3/3")
}

// A live gate may inspect only the root it just created when exercising backfill.
type gateHistory struct {
	pluginapi.Session
	root string
}

func (g gateHistory) History(ctx context.Context, ch, oldest string) ([]pluginapi.RootRef, error) {
	refs, e := g.Session.History(ctx, ch, g.root)
	if e != nil {
		return nil, e
	}
	out := []pluginapi.RootRef{}
	for _, r := range refs {
		if r.RootTS == g.root {
			out = append(out, r)
		}
	}
	return out, nil
}
