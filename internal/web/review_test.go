package web

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthProbeAcceptsUnlistedHost(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "server", Host: "127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest(method, "http://192.0.2.10/healthz", nil))
		if w.Code != 200 || w.Body.String() != "ok\n" {
			t.Fatalf("probe: %d %q", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "http://192.0.2.10/settings/", nil))
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestSlowPluginDoesNotBlockAppRequests(t *testing.T) {
	p, ok := pluginapi.Legacy()
	if !ok {
		t.Skip("no plugin")
	}
	fake := slacktest.New(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fake.Overrides["conversations.list"] = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"ok":true,"channels":[]}`))
	}
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	a.Secrets.Desktop = false
	t.Setenv("SYNTHETIC_BOT", "xoxb-synthetic")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(a.opts.Home, "archive"))
	if e = os.WriteFile(a.opts.Config, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	a.Manager.SetPaused("fixture", true)
	if e = a.Manager.Reload(); e != nil {
		t.Fatal(e)
	}
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	req.AddCookie(cookie)
	id, _ := a.session(req)
	step := 0
	for i, s := range p.SetupFlow() {
		if s.Kind == "choose" {
			step = i
			break
		}
	}
	a.wizards[id] = &wizard{Platform: p.Manifest().ID, Step: step, Started: time.Now(), Data: pluginapi.SetupState{}, Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://127.0.0.1/setup/step", nil)
		r.AddCookie(cookie)
		a.ServeHTTP(w, r)
	}()
	<-entered
	defer func() { close(release); <-done }()
	for _, path := range []string{"/login?token=synthetic-launch", "/", "/view/"} {
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "http://127.0.0.1"+path, strings.NewReader(""))
			r.AddCookie(cookie)
			a.ServeHTTP(w, r)
			result <- w
		}()
		select {
		case w := <-result:
			if w.Code != 200 && w.Code != 303 {
				t.Fatal(path, w.Code)
			}
		case <-time.After(time.Second):
			t.Fatal("slow plugin blocked", path)
		}
	}
}

func TestSessionHasAbsoluteExpiryAndPortCookie(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch", SecureCookies: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1:18787/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	if cookie.Name != "ck_session_18787" {
		t.Fatal("desktop cookie lacks port", cookie.Name)
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:18787/", nil)
	r.AddCookie(cookie)
	id, csrf := a.session(r)
	expires := time.Now().Add(time.Minute).Unix()
	if _, e = a.db.Exec("UPDATE sessions SET expires=? WHERE id=?", expires, id); e != nil {
		t.Fatal(e)
	}
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	var got int64
	a.db.QueryRow("SELECT expires FROM sessions WHERE id=?", id).Scan(&got)
	if got != expires {
		t.Fatal("poll extended absolute expiry")
	}
	if w.Result().Cookies()[0].MaxAge > 60 {
		t.Fatal("cookie exceeded session lifetime")
	}
	other := httptest.NewRequest("GET", "http://127.0.0.1:18788/", nil)
	other.AddCookie(cookie)
	if id, _ := a.session(other); id != "" {
		t.Fatal("cookie accepted on another port")
	}
	logout := httptest.NewRequest("POST", "http://127.0.0.1:18787/logout", strings.NewReader("csrf="+csrf))
	logout.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logout.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, logout)
	cookies := w.Result().Cookies()
	if !cookies[len(cookies)-1].Secure || cookies[len(cookies)-1].MaxAge != -1 {
		t.Fatal("insecure logout cookie")
	}
}

func TestExpiredStateAndEventsAreBounded(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "server", Host: "127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	_, e = a.db.Exec("INSERT INTO sessions VALUES('expired','csrf',0); INSERT INTO setup_sessions VALUES('expired','fixture',0,'{}',0)")
	if e != nil {
		t.Fatal(e)
	}
	a.wizards["expired"] = &wizard{Started: time.Now()}
	tx, e := a.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 10020; i++ {
		if _, e = tx.Exec("INSERT INTO events(workspace,channel,root,at) VALUES('fixture','CFIXTURE',?,unixepoch())", i); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	a.purgeExpired()
	for _, table := range []string{"sessions", "setup_sessions", "events"} {
		var n int
		a.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n)
		if table == "events" {
			if n != 10000 {
				t.Fatal("events not capped", n)
			}
		} else if n != 0 {
			t.Fatal("expired rows retained", table, n)
		}
	}
	if len(a.wizards) != 0 {
		t.Fatal("expired wizard retained")
	}
}

type blockedResolver struct {
	entered, release chan struct{}
	once             sync.Once
}

func (s *blockedResolver) Get(string) (string, string, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return "", "fake", nil
}
func TestSlowReloadDoesNotBlockAppRequests(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	// A parseable workspace forces credential resolution before it fails validation.
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(a.opts.Home, "archive"))
	if _, ok := pluginapi.Legacy(); !ok {
		t.Skip("no plugin")
	}
	if e = os.WriteFile(a.opts.Config, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	s := &blockedResolver{entered: make(chan struct{}), release: make(chan struct{})}
	a.Manager.Secrets = s
	done := make(chan struct{})
	go func() { _ = a.Manager.Reload(); close(done) }()
	<-s.entered
	defer func() { close(s.release); <-done }()
	for _, path := range []string{"/login?token=synthetic-launch", "/", "/view/"} {
		result := make(chan int, 1)
		go func() {
			r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			result <- w.Code
		}()
		select {
		case code := <-result:
			if code != 200 && code != 303 {
				t.Fatal(path, code)
			}
		case <-time.After(time.Second):
			t.Fatal("reload blocked App", path)
		}
	}
}

func TestAppViewerOwnsArchiveHandles(t *testing.T) {
	if _, ok := pluginapi.Legacy(); !ok {
		t.Skip("no plugin")
	}
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	a.Secrets.Desktop = false
	t.Setenv("SYNTHETIC_BOT", "xoxb-synthetic")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(a.opts.Home, "archive"))
	if e = os.WriteFile(a.opts.Config, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	a.Manager.SetPaused("fixture", true)
	if e = a.Manager.Reload(); e != nil {
		t.Fatal(e)
	}
	source, e := storage.Open("../storage/testdata/golden")
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	key := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	snapshot, e := source.Read(context.Background(), key)
	if e != nil {
		t.Fatal(e)
	}
	running := a.Manager.List()[0]
	if _, e = running.Store.Save(context.Background(), key, *snapshot); e != nil {
		t.Fatal(e)
	}
	v := a.viewer("")
	unavailable := filepath.Join(a.opts.Home, "unavailable")
	if e = os.WriteFile(unavailable, []byte("synthetic non-directory"), 0600); e != nil {
		t.Fatal(e)
	}
	v.sources["unavailable"] = Source{path: unavailable}
	// Simulate the reload boundary after runtime retirement, before publication.
	if e = running.Store.Close(); e != nil {
		t.Fatal(e)
	}
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	for _, path := range []string{"/view/fixture/CFIXTURE/100.000001", "/", "/view/"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		want := "100.000001"
		if path == "/view/" {
			want = "fixture"
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatal("retired runtime invalidated archive response", path, w.Code)
		}
	}
}
