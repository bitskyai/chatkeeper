//go:build live && !windows

package slack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type stripAuth struct {
	base http.RoundTripper
	path string
}

func (s stripAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	if s.path != "" && r.URL.Path == s.path {
		r = r.Clone(r.Context())
		r.Header.Del("Authorization")
	}
	return s.base.RoundTrip(r)
}

// TestRealArchiveShapes posts one bot-owned thread (bulleted rich text with a
// mention, a button reply and an attachment reply, reactions on replies),
// archives it in polling-only mode, then forces a failure and checks that the
// cause is readable. Every message it creates is deleted afterwards.
func TestRealArchiveShapes(t *testing.T) {
	tokenPath := os.Getenv("CHATKEEPER_GATE_TOKEN_FILE")
	channel := os.Getenv("CHATKEEPER_GATE_CHANNEL")
	if tokenPath == "" || channel == "" {
		t.Skip("explicit real-workspace credentials and channel required")
	}
	if os.Getenv("CHATKEEPER_GATE_CONFIRM") != channel {
		t.Fatal("set CHATKEEPER_GATE_CONFIRM to the gate channel to authorize posting there")
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 60 * time.Second}
	api := func(method string, p map[string]any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(p)
		req, e := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/"+method, strings.NewReader(string(body)))
		if e != nil {
			t.Fatal("request creation failed")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		res, e := client.Do(req)
		if e != nil {
			t.Fatal("gate API transport failed")
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil || doc["ok"] != true {
			t.Fatalf("gate API %s failed (status %d, code %v)", method, res.StatusCode, doc["error"])
		}
		return doc
	}
	me := api("auth.test", map[string]any{})
	team, bot := me["team_id"].(string), me["user_id"].(string)
	prefix := fmt.Sprintf("[chatkeeper-errdetail] %d", time.Now().UnixNano())
	created := []string{}
	defer func() {
		for i := len(created) - 1; i >= 0; i-- {
			api("chat.delete", map[string]any{"channel": channel, "ts": created[i]})
		}
		t.Logf("cleaned up %d messages", len(created))
	}()
	root := api("chat.postMessage", map[string]any{"channel": channel, "text": prefix + " root <@" + bot + ">", "blocks": []any{map[string]any{"type": "rich_text", "elements": []any{
		map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": prefix + " root for "}, map[string]any{"type": "user", "user_id": bot}}},
		map[string]any{"type": "rich_text_list", "style": "bullet", "indent": 0, "elements": []any{
			map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": "first bullet", "style": map[string]any{"bold": true}}}},
			map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "emoji", "name": "tv"}, map[string]any{"type": "text", "text": " second bullet"}}},
		}},
	}}}})["ts"].(string)
	created = append(created, root)
	button := api("chat.postMessage", map[string]any{"channel": channel, "thread_ts": root, "text": prefix + " button reply", "blocks": []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": prefix + " button reply"}},
		map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "style": "primary", "text": map[string]any{"type": "plain_text", "text": "Open PR"}, "url": "https://example.com/pr/1"}}},
	}})["ts"].(string)
	created = append(created, button)
	attachment := api("chat.postMessage", map[string]any{"channel": channel, "thread_ts": root, "text": prefix + " attachment reply", "attachments": []any{map[string]any{"color": "good", "title": "checks green", "text": "all passed", "fields": []any{map[string]any{"title": "Status", "value": "ok", "short": true}}}}})["ts"].(string)
	created = append(created, attachment)
	api("reactions.add", map[string]any{"channel": channel, "timestamp": button, "name": "white_check_mark"})
	api("reactions.add", map[string]any{"channel": channel, "timestamp": attachment, "name": "wastebasket"})
	api("reactions.add", map[string]any{"channel": channel, "timestamp": root, "name": "tv"})
	key := pluginapi.Key{Channel: channel, RootTS: root}

	run := func(name string, transport http.RoundTripper) (string, *storage.Folder, string) {
		store, e := storage.Open(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { store.Close() })
		log := &syncBuffer{}
		ws := pluginapi.Workspace{ID: "live", Platform: "slack", Channels: []string{channel}, Trigger: pluginapi.Trigger{Reactions: []string{"tv"}, Who: "any", Match: "any"}, PollInterval: time.Hour, Settings: pluginapi.Settings(`{"team_id":"` + team + `"}`)}
		rt := core.New(ws, slack.Plugin{}, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": token}, HTTP: &http.Client{Timeout: 60 * time.Second, Transport: transport}, Log: slog.New(slog.NewTextHandler(log, nil))}, store)
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); rt.Run(runCtx) }()
		defer func() { stop(); <-done }()
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) && rt.Health().State != "running" {
			time.Sleep(50 * time.Millisecond)
		}
		checkCtx, checkCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer checkCancel()
		report := rt.CheckNow(checkCtx)
		result := ""
		for _, ch := range report.Channels {
			for _, r := range ch.Roots {
				if r.RootTS == root {
					result = r.Result
				}
			}
		}
		t.Logf("%s: Check now result: %s", name, result)
		if strings.Contains(log.String(), token) {
			t.Fatal("token leaked into the log")
		}
		return result, store, log.String()
	}

	result, store, _ := run("polling", http.DefaultTransport)
	v, e := store.Read(ctx, key)
	if e != nil || v == nil {
		t.Fatalf("thread not archived: %q %v", result, e)
	}
	manifest, _ := json.Marshal(v.Manifest)
	if v.Manifest.MessageCount != 3 || strings.Contains(string(manifest), `"gaps"`) {
		t.Fatalf("unexpected archive: %s", manifest)
	}
	replyReactions := 0
	for _, m := range v.Messages[1:] {
		if rs, ok := m["reactions"].([]any); ok {
			replyReactions += len(rs)
		}
	}
	w := httptest.NewRecorder()
	web.New(map[string]web.Source{"live": {Store: store, Content: slack.Hooks{}}}, "").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/view/live/"+channel+"/"+root, nil))
	page := w.Body.String()
	for _, want := range []string{"first bullet", "second bullet", "Open PR", "checks green", "attachment reply"} {
		if !strings.Contains(page, want) {
			t.Fatalf("viewer (%d) lacks %q", w.Code, want)
		}
	}
	if replyReactions != 2 {
		t.Fatalf("reply reactions %d", replyReactions)
	}
	t.Logf("archived %d messages, %d reply reactions, viewer OK", v.Manifest.MessageCount, replyReactions)

	// Forced failure: the replies read is sent without credentials, so the real
	// API rejects it; the cause must be readable and nothing stored.
	result, store, log := run("forced failure", stripAuth{base: http.DefaultTransport, path: "/api/conversations.replies"})
	if !strings.Contains(result, "archive failed: plugin failure:") || !strings.Contains(result, "conversations.replies") || !strings.Contains(result, "not_authed") {
		t.Fatalf("cause not readable: %q", result)
	}
	if !strings.Contains(log, "thread archive failed") || !strings.Contains(log, "not_authed") {
		t.Fatal("server log lacks the cause")
	}
	if v, _ := store.Read(ctx, key); v != nil {
		t.Fatal("failed capture stored")
	}
}
