package slack_test

import (
	"bytes"
	"context"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type polled struct {
	report  core.CheckReport
	store   *storage.Folder
	runtime *core.Runtime
	log     *syncBuffer
}

func (p polled) result() string {
	for _, ch := range p.report.Channels {
		for _, root := range ch.Roots {
			if root.RootTS == "100.000001" {
				return root.Result
			}
		}
	}
	return ""
}
func (p polled) view(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	web.New(map[string]web.Source{"fixture": {Store: p.store, Content: slack.Hooks{}}}, "").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/view/fixture/CFIXTURE/100.000001", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return w.Body.String()
}

// pollOnce runs a polling-only (bot token) runtime against the fake server and
// returns the Check now report for the marked thread.
func pollOnce(t *testing.T, fake *slacktest.Server, token string) polled {
	t.Helper()
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	log := &syncBuffer{}
	ws := pluginapi.Workspace{ID: "fixture", Platform: "slack", Channels: []string{"CFIXTURE"}, Trigger: pluginapi.Trigger{Reactions: []string{"white_check_mark"}, Who: "owner", Owners: []string{"UOWNER"}}, PollInterval: time.Hour, Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
	runtime := core.New(ws, slack.Plugin{}, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": token}, HTTP: fake.Client(), Log: slog.New(slog.NewTextHandler(log, nil))}, store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtime.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("runtime did not stop")
		}
	})
	eventually(t, func() bool { return runtime.Health().State == "running" })
	// Mark only after the first automatic poll so Check now performs the capture.
	eventually(t, func() bool {
		for _, a := range runtime.Activity() {
			if a.Kind == "poll" {
				return true
			}
		}
		return false
	})
	fake.Mark(true)
	checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
	defer checkCancel()
	return polled{report: runtime.CheckNow(checkCtx), store: store, runtime: runtime, log: log}
}

func file(id string, extra map[string]any) map[string]any {
	f := map[string]any{"id": id, "name": id + ".bin", "size": 5, "mode": "hosted", "url_private_download": "https://files.slack.com/file?id=" + id}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

var bulletRoot = map[string]any{"ts": "100.000001", "user": "UOWNER", "text": "PR ready <@UMENTION>", "blocks": []any{map[string]any{"type": "rich_text", "block_id": "b1", "elements": []any{
	map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": "PR ready "}, map[string]any{"type": "user", "user_id": "UMENTION"}}},
	map[string]any{"type": "rich_text_list", "style": "bullet", "indent": 0, "border": 0, "elements": []any{
		map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": "first change", "style": map[string]any{"bold": true}}}},
		map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "emoji", "name": "tv", "unicode": "1f4fa"}}},
	}},
}}}}

// A realistic thread: bulleted rich text, an integration reply without a user,
// a file reply with reactions, and a mention. It must archive with no gaps.
func TestArchiveTolerantThreadShapes(t *testing.T) {
	fake := slacktest.New(t)
	fake.SetThread(bulletRoot,
		map[string]any{"ts": "101.000001", "thread_ts": "100.000001", "subtype": "bot_message", "bot_id": "BCI", "username": "ci-bot", "bot_profile": map[string]any{"id": "BCI", "name": "CI Bot"}, "text": "build passed", "attachments": []any{map[string]any{"title": nil, "text": "checks green", "fields": []any{map[string]any{"title": "Status", "value": nil}}, "actions": []any{map[string]any{"type": "button", "style": "primary", "text": "Open"}}}}},
		map[string]any{"ts": "102.000001", "thread_ts": "100.000001", "user": "UAUTHOR", "subtype": "thread_broadcast", "text": "looks good", "reactions": []any{map[string]any{"name": "wastebasket", "count": 1, "users": []any{"UAUTHOR"}}}, "files": []any{file("FGOOD", nil)}},
		map[string]any{"ts": "103.000001", "thread_ts": "100.000001", "user": "UOWNER", "text": "edited later", "edited": map[string]any{"user": "UOWNER", "ts": "103.500000"}},
	)
	p := pollOnce(t, fake, "xoxb-synthetic")
	if got := p.result(); got != "archived" {
		t.Fatalf("result %q; hints %v", got, p.report.Hints)
	}
	v, e := p.store.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	if e != nil || v == nil || v.Manifest.MessageCount != 4 || len(v.Manifest.Files) != 1 || len(v.Manifest.Gaps) != 0 {
		t.Fatalf("%v %#v", e, v)
	}
	page := p.view(t)
	for _, want := range []string{"first change", "ci-bot", "checks green", "looks good", "Fixture mention"} {
		if !strings.Contains(page, want) {
			t.Fatalf("viewer lacks %q", want)
		}
	}
}

// Each unfetchable file or author is recorded as a gap; the thread is kept.
func TestArchiveRecordsPerItemGaps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []any
		override map[string]func(http.ResponseWriter, *http.Request)
		want     []string
		noFetch  bool
	}{
		{name: "html sign-in page", files: []any{file("FHTML", nil)}, override: map[string]func(http.ResponseWriter, *http.Request){"download": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!DOCTYPE html><html>sign in</html>"))
		}}, want: []string{"FHTML", "HTML page", "files:read"}},
		{name: "redirect to workspace", files: []any{file("FREDIR", nil)}, override: map[string]func(http.ResponseWriter, *http.Request){"download": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://fixture.slack.com/?redir=%2Ffiles-pri%2Fx", http.StatusFound)
		}}, want: []string{"FREDIR", "redirected to fixture.slack.com", "files:read"}},
		{name: "download error", files: []any{file("FGONE", nil)}, override: map[string]func(http.ResponseWriter, *http.Request){"download": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}}, want: []string{"FGONE", "HTTP 404"}},
		{name: "unreadable file kinds", files: []any{file("FLIMIT", map[string]any{"mode": "hidden_by_limit"}), file("FEXT", map[string]any{"mode": "external", "is_external": true}), file("FDEL", map[string]any{"mode": "tombstone"}), file("FDENY", map[string]any{"file_access": "access_denied"}), map[string]any{"name": "no id"}}, want: []string{"5 items not captured", "hidden by the workspace plan", "external file", "deleted"}, noFetch: true},
		{name: "author missing scope", override: map[string]func(http.ResponseWriter, *http.Request){"users.info": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope","needed":"users:read","provided":"channels:history,reactions:read"}`))
		}}, want: []string{"user UAUTHOR", "missing_scope", "needed: users:read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := slacktest.New(t)
			for k, v := range tc.override {
				fake.Overrides[k] = v
			}
			reply := map[string]any{"ts": "101.000001", "thread_ts": "100.000001", "user": "UAUTHOR", "text": "with files", "user_profile": map[string]any{"display_name": "Profile author"}}
			if tc.files != nil {
				reply["files"] = tc.files
			}
			fake.SetThread(map[string]any{"ts": "100.000001", "user": "UOWNER", "text": "root"}, reply)
			p := pollOnce(t, fake, "xoxb-synthetic")
			got := p.result()
			if !strings.HasPrefix(got, "archived; ") {
				t.Fatalf("result %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) && !strings.Contains(p.view(t), want) {
					t.Fatalf("%q lacks %q", got, want)
				}
			}
			if tc.noFetch && fake.Calls("download") != 0 {
				t.Fatal("unreadable file was fetched")
			}
			page := p.view(t)
			if !strings.Contains(page, "Not captured") || !strings.Contains(page, "with files") {
				t.Fatal(page)
			}
			if tc.name == "author missing scope" && (!strings.Contains(page, "Profile author") || !strings.Contains(page, "Fix: add users:read")) {
				t.Fatal("placeholder author or fix hint missing", page)
			}
			if !strings.Contains(p.log.String(), "thread archived with missing items") {
				t.Fatal(p.log.String())
			}
		})
	}
}

// A failure where nothing safe can be stored names its cause everywhere: the
// Check now result and hints, recent activity, health and the server log.
func TestArchiveFailureShowsCause(t *testing.T) {
	fake := slacktest.New(t)
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope","needed":"groups:history","provided":"channels:history,users:read"}`))
	}
	p := pollOnce(t, fake, "xoxb-synthetic")
	want := "archive failed: plugin failure: Slack API conversations.replies error: missing_scope (needed: groups:history; token has: channels:history,users:read). Fix: add groups:history under OAuth & Permissions"
	if got := p.result(); !strings.HasPrefix(got, want) {
		t.Fatalf("result %q", got)
	}
	hinted := false
	for _, h := range p.report.Hints {
		hinted = hinted || strings.Contains(h, "Fix: add groups:history")
	}
	if !hinted {
		t.Fatal(p.report.Hints)
	}
	if keys, _ := p.store.Keys(context.Background()); len(keys) != 0 {
		t.Fatal("failed thread stored", keys)
	}
	if !strings.Contains(p.runtime.Health().LastError, "missing_scope") {
		t.Fatal(p.runtime.Health())
	}
	recorded := false
	for _, a := range p.runtime.Activity() {
		recorded = recorded || a.Kind == "error" && strings.Contains(a.Text, "needed: groups:history")
	}
	if !recorded {
		t.Fatal(p.runtime.Activity())
	}
	if log := p.log.String(); !strings.Contains(log, "thread archive failed") || !strings.Contains(log, "needed: groups:history") || strings.Contains(log, "xoxb-synthetic") {
		t.Fatal(log)
	}
}

// Rate limiting is transient: the thread fails (nothing partial stored) and
// the cause says so.
func TestArchiveRateLimitedFileFailsThread(t *testing.T) {
	fake := slacktest.New(t)
	fake.Overrides["download"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	p := pollOnce(t, fake, "xoxb-synthetic-ratelimited")
	if got := p.result(); !strings.Contains(got, "archive failed") || !strings.Contains(got, "rate limited") {
		t.Fatalf("result %q", got)
	}
	if keys, _ := p.store.Keys(context.Background()); len(keys) != 0 {
		t.Fatal("partial thread stored", keys)
	}
}
