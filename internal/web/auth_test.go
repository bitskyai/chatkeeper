package web

import (
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecurityBoundary(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "0.0.0.0"})
	if e == nil {
		a.Close()
		t.Fatal("accepted nonloopback desktop")
	}
	home := t.TempDir()
	a, e = NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1", SetupCode: "synthetic-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, path := range []string{"/", "/setup/", "/settings/", "/view/", "/api/config"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	for _, host := range []string{"evil.invalid", "127.0.0.1"} {
		r := httptest.NewRequest("POST", "http://"+host+"/bootstrap", strings.NewReader("code=synthetic-code&password=synthetic-password"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://evil.invalid")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(host, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/bootstrap", strings.NewReader("code=synthetic-code&password=synthetic-password"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "http://127.0.0.1/bootstrap", nil)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	if _, e = filepath.Abs(home); e != nil {
		t.Fatal(e)
	}
}

func TestAdminCSRFAndSecretResponses(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	if login.Code != 303 {
		t.Fatal(login.Code)
	}
	cookie := login.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	req.AddCookie(cookie)
	_, csrf := a.session(req)
	for _, tc := range []struct {
		name, path, token, origin, host string
		want                            int
	}{{"missing", "/settings/reload", "", "", "127.0.0.1", 403}, {"wrong", "/settings/reload", "wrong", "", "127.0.0.1", 403}, {"origin", "/settings/reload", csrf, "https://evil.invalid", "127.0.0.1", 403}, {"host", "/settings/reload", csrf, "", "evil.invalid", 403}, {"valid", "/settings/reload", csrf, "", "127.0.0.1", 400}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+tc.host+tc.path, strings.NewReader("csrf="+csrfEscape(tc.token)))
			r.AddCookie(cookie)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	for _, path := range []string{"/setup/start", "/setup/step", "/settings/password", "/settings/credentials", "/settings/remove", "/settings/pause", "/logout", "/api/archives/a/b/c"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1"+path, strings.NewReader(""))
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}
func csrfEscape(s string) string { return url.QueryEscape(s) }
func TestBootstrapSurvivesDatabaseLossAndRateLimit(t *testing.T) {
	home := t.TempDir()
	a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1", AdminPassword: "synthetic-password"})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "http://127.0.0.1/login", strings.NewReader("password=wrong"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatal(i, w.Code)
		}
	}
	a.Close()
	os.Remove(filepath.Join(home, "state", "app.db"))
	a, e = NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1", SetupCode: "replacement-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/bootstrap", nil))
	if w.Code != 409 {
		t.Fatal("database loss reopened bootstrap", w.Code)
	}
}
func TestConfinedStorage(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if !confined(root, filepath.Join(root, "archive")) {
		t.Fatal("child rejected")
	}
	if confined(root, filepath.Join(root, "..", "escape")) {
		t.Fatal("traversal accepted")
	}
	if e := os.Symlink(outside, filepath.Join(root, "link")); e != nil {
		t.Skip(e)
	}
	if confined(root, filepath.Join(root, "link", "archive")) {
		t.Fatal("symlink accepted")
	}
}
func TestInvalidExistingConfigCannotBootstrap(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("workspaces: [malformed]\n"), 0600)
	a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1", SetupCode: "synthetic-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/bootstrap", nil))
	if w.Code != 403 {
		t.Fatal("existing config exposed bootstrap", w.Code)
	}
}
func TestWizardDataRejectsCredentials(t *testing.T) {
	z := wizard{Data: pluginapi.SetupState{}, Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic-private"}}
	if z.merge(json.RawMessage(`{"team_id":"TFIXTURE","value":"xoxb-synthetic-private"}`)) == nil {
		t.Fatal("accepted secret in wizard state")
	}
	if e := z.merge(json.RawMessage(`{"team_id":"TFIXTURE","bot_token":"other","password":"other"}`)); e != nil {
		t.Fatal(e)
	}
	if len(z.Data) != 1 {
		t.Fatal("secret-shaped fields persisted")
	}
}
func TestSchemaFormsConvertTypedInput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean","default":true},"limit":{"type":"integer","minimum":1},"ratio":{"type":"number"},"names":{"type":"array","items":{"type":"string"}},"secret":{"type":"string","x-secret":true}},"required":["limit"]}`)
	r := httptest.NewRequest("POST", "http://127.0.0.1", strings.NewReader("enabled=on&limit=4&ratio=1.5&names=one%0Atwo&secret=synthetic-secret"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	data, creds := formInput(r, schema)
	if creds["secret"] != "synthetic-secret" || strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("secret handling")
	}
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	names := got["names"].([]any)
	if len(names) != 2 || got["limit"] != float64(4) || got["enabled"] != true {
		t.Fatal(string(data))
	}
}
func TestAdminSessionsExpireAndAreRevoked(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	r.AddCookie(cookie)
	id, _ := a.session(r)
	if id == "" {
		t.Fatal("no session")
	}
	// An existing session can have less than twelve hours remaining. Refresh
	// must preserve its absolute expiry, regardless of scheduler delays.
	expires := time.Now().Add(10 * time.Minute).Unix()
	if _, e := a.db.Exec("UPDATE sessions SET expires=? WHERE id=?", expires, id); e != nil {
		t.Fatal(e)
	}
	before := time.Now().Unix()
	refreshed := httptest.NewRecorder()
	a.ServeHTTP(refreshed, r)
	after := time.Now().Unix()
	cookies := refreshed.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("active session cookie not refreshed")
	}
	remaining := int64(cookies[0].MaxAge)
	if remaining < expires-after || remaining > expires-before {
		t.Fatalf("cookie lifetime %d does not preserve session expiry %d (request %d..%d)", remaining, expires, before, after)
	}
	var storedExpiry int64
	if e := a.db.QueryRow("SELECT expires FROM sessions WHERE id=?", id).Scan(&storedExpiry); e != nil || storedExpiry != expires {
		t.Fatal("refresh changed absolute session expiry", storedExpiry, e)
	}
	_, _ = a.db.Exec("UPDATE sessions SET expires=0")
	if id, _ := a.session(r); id != "" {
		t.Fatal("expired session accepted")
	}
}
