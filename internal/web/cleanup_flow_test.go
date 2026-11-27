//go:build !noslack

package web_test

import (
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/zalando/go-keyring"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCleanupConfirmationPasswordAndDigest(t *testing.T) {
	keyring.MockInit()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	cfg := "workspaces:\n - id: fixture\n   platform: slack\n   team_id: TFIXTURE\n   channels: [CFIXTURE]\n   storage: {type: folder, path: archive}\n   trigger: {who: owner, owners: [UOWNER], reactions: [white_check_mark]}\n   cleanup: {enabled: true}\n"
	os.WriteFile(path, []byte(cfg), 0600)
	a, e := web.NewApp(web.Options{Home: home, Mode: "server", Host: "127.0.0.1", AdminPassword: "synthetic-password"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	login := httptest.NewRequest("POST", "http://127.0.0.1/login", strings.NewReader("password=synthetic-password"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lw := httptest.NewRecorder()
	a.ServeHTTP(lw, login)
	if lw.Code != 303 {
		t.Fatal(lw.Code, lw.Body.String())
	}
	cookie := lw.Result().Cookies()[0]
	send := func(method, path string, v url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	page := send("GET", "/cleanup/?workspace=fixture", nil)
	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	match := csrfRE.FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal(page.Code, page.Body.String())
	}
	csrf := match[1]
	preview := send("POST", "/cleanup/preview", url.Values{"csrf": {csrf}, "workspace": {"fixture"}})
	digestRE := regexp.MustCompile(`name="digest" value="([^"]+)"`)
	match = digestRE.FindStringSubmatch(preview.Body.String())
	if len(match) != 2 {
		t.Fatal(preview.Code, preview.Body.String())
	}
	v := url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "digest": {match[1]}, "confirm": {"wrong"}, "password": {"synthetic-password"}}
	if w := send("POST", "/cleanup/apply", v); w.Code != 400 {
		t.Fatal("typed confirmation bypass", w.Code)
	}
	v.Set("confirm", "fixture")
	v.Set("password", "wrong")
	if w := send("POST", "/cleanup/apply", v); w.Code != 401 {
		t.Fatal("password bypass", w.Code)
	}
	v.Set("password", "synthetic-password")
	os.WriteFile(path, []byte(strings.Replace(cfg, "enabled: true", "enabled: false", 1)), 0600)
	if w := send("POST", "/cleanup/apply", v); w.Code != 409 {
		t.Fatal("stale policy accepted", w.Code, w.Body.String())
	}
}
