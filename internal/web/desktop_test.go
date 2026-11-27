package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func desktopRequest(a *App, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestDesktopStatusAuthAndShape(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, tc := range []struct {
		method, path, token string
		code                int
	}{
		{"GET", "/api/desktop/status", "", 401},
		{"GET", "/api/desktop/status", "wrong", 401},
		{"POST", "/api/desktop/status", "synthetic-launch", 405},
		{"GET", "/api/desktop/pause", "synthetic-launch", 405},
		{"POST", "/api/desktop/pause", "synthetic-launch", 400},
		{"GET", "/api/desktop/unknown", "synthetic-launch", 404},
		{"GET", "/api/desktop/status", "synthetic-launch", 200},
	} {
		w := desktopRequest(a, tc.method, tc.path, tc.token, "")
		if w.Code != tc.code {
			t.Fatalf("%s %s %q: %d %s", tc.method, tc.path, tc.token, w.Code, w.Body)
		}
	}
	// A browser session must not unlock the launcher API (no CSRF surface).
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	r := httptest.NewRequest("GET", "http://127.0.0.1/api/desktop/status", nil)
	r.AddCookie(login.Result().Cookies()[0])
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("session cookie accepted", w.Code)
	}
	var s desktopStatus
	w = desktopRequest(a, "GET", "/api/desktop/status", "synthetic-launch", "")
	if e = json.Unmarshal(w.Body.Bytes(), &s); e != nil {
		t.Fatal(e)
	}
	if s.State != "setup" || len(s.Workspaces) != 0 || s.Recent == nil {
		t.Fatalf("%+v", s)
	}
	// Foreign Host headers stay rejected (DNS rebinding).
	r = httptest.NewRequest("GET", "http://attacker.example/api/desktop/status", nil)
	r.Header.Set("Authorization", "Bearer synthetic-launch")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign host", w.Code)
	}
}

func TestDesktopAPIAbsentInServerMode(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "server", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if w := desktopRequest(a, "GET", "/api/desktop/status", "synthetic-launch", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestDesktopStatusWorkspacesRecentAndPause(t *testing.T) {
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
	now := time.Now().Unix()
	for i, v := range []struct {
		root string
		at   int64
	}{{"1.000001", now - 600}, {"2.000001", now - 500}, {"1.000001", now - 10}, {"3.000001", now - 400}, {"4.000001", now - 300}, {"5.000001", now - 200}, {"6.000001", now - 100}} {
		if _, e = a.db.Exec("INSERT INTO events(workspace,channel,root,at) VALUES('fixture','CFIXTURE',?,?)", v.root, v.at); e != nil {
			t.Fatal(i, e)
		}
	}
	w := desktopRequest(a, "GET", "/api/desktop/status", "synthetic-launch", "")
	body := w.Body.String()
	if strings.Contains(body, "xoxb") || strings.Contains(body, a.opts.Home) || strings.Contains(body, "synthetic-launch") {
		t.Fatal("secret or path leaked", body)
	}
	var s desktopStatus
	if e = json.Unmarshal(w.Body.Bytes(), &s); e != nil {
		t.Fatal(e)
	}
	if s.State != "paused" || !s.Paused || len(s.Workspaces) != 1 || s.Workspaces[0].ID != "fixture" || !s.Workspaces[0].Paused {
		t.Fatalf("%+v", s)
	}
	if len(s.Recent) != 5 || s.Recent[0].Root != "1.000001" || s.Recent[1].Root != "6.000001" || s.Recent[0].Path != "/view/fixture/CFIXTURE/1.000001" {
		t.Fatalf("recent not deduplicated newest-first: %+v", s.Recent)
	}
	if s.LastCapture == "" {
		t.Fatal("missing last capture")
	}
	// Pausing an already paused set is a persisted no-op.
	if w = desktopRequest(a, "POST", "/api/desktop/pause", "synthetic-launch", `{"paused":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestDesktopLoginNextIsConfined(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for next, want := range map[string]string{
		"":                             "/",
		"/view/fixture/CFIXTURE/1.000": "/view/fixture/CFIXTURE/1.000",
		"//evil.example/":              "/",
		"https://evil.example/":        "/",
		"/\\evil.example":              "/",
		"/api/desktop/status":          "/",
		"/settings/":                   "/settings/",
	} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch&next="+url.QueryEscape(next), nil))
		if w.Code != 303 || w.Header().Get("Location") != want {
			t.Fatalf("%q -> %d %q", next, w.Code, w.Header().Get("Location"))
		}
	}
}
