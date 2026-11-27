package web

import (
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRejectsDuplicateIDAndSeparatesCredentialNames(t *testing.T) {
	p, ok := pluginapi.Legacy()
	if !ok {
		t.Skip("no implementation")
	}
	home := t.TempDir()
	a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	_ = a.Manager.Pause("a-b")
	_ = a.Manager.Pause("a_b")
	commit := func(id, token string) *httptest.ResponseRecorder {
		z := &wizard{Platform: p.Manifest().ID, Step: len(p.SetupFlow()), Data: pluginapi.SetupState{"team_id": []byte(`"TFIXTURE"`), "channels": []byte(`"auto"`), "trigger": []byte(`{"reactions":["white_check_mark"],"who":"any"}`)}, Credentials: pluginapi.Credentials{"bot_token": token}}
		values := url.Values{"workspace": {id}, "storage": {filepath.Join(home, "data", id)}}
		r := httptest.NewRequest("POST", "http://127.0.0.1/setup/step", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = r.ParseForm()
		w := httptest.NewRecorder()
		a.storageStep(w, r, "fixture-session", "csrf", z, p)
		return w
	}
	if w := commit("a-b", "xoxb-first"); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := commit("a_b", "xoxb-second"); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	entries := a.Manager.List()
	if len(entries) != 2 {
		t.Fatal(len(entries))
	}
	for _, r := range entries {
		want := "xoxb-first"
		if r.Entry.Workspace.ID == "a_b" {
			want = "xoxb-second"
		}
		if r.Entry.Credentials["bot_token"] != want {
			t.Fatal("credential collision")
		}
	}
	if w := commit("a-b", "xoxb-replacement"); w.Code != 200 || !strings.Contains(w.Body.String(), "already in use") {
		t.Fatal("duplicate accepted", w.Code)
	}
	for _, r := range a.Manager.List() {
		if r.Entry.Workspace.ID == "a-b" && r.Entry.Credentials["bot_token"] != "xoxb-first" {
			t.Fatal("duplicate overwrote credentials")
		}
	}
}
func TestEnvironmentCredentialsAreWriteOnly(t *testing.T) {
	p, ok := pluginapi.Legacy()
	if !ok {
		t.Skip("no implementation")
	}
	var cs struct {
		Properties map[string]struct {
			Env string `json:"x-env-default"`
		}
	}
	_ = json.Unmarshal(p.CredentialSchema(), &cs)
	for k, prop := range cs.Properties {
		if k == "bot_token" {
			t.Setenv(prop.Env, "xoxb-environment-synthetic")
		}
	}
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	r := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	r.AddCookie(cookie)
	id, csrf := a.session(r)
	body := url.Values{"csrf": {csrf}, "platform": {p.Manifest().ID}}
	r = httptest.NewRequest("POST", "http://127.0.0.1/setup/start", strings.NewReader(body.Encode()))
	r.AddCookie(cookie)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	a.wizards[id].Step = 1
	r = httptest.NewRequest("GET", "http://127.0.0.1/setup/step", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "xoxb-environment-synthetic") || !strings.Contains(w.Body.String(), "Provided by environment") {
		t.Fatal("credential field leaked or asked for environment value")
	}
	var data string
	if e = a.db.QueryRow("SELECT data FROM setup_sessions WHERE id=?", id).Scan(&data); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(data, "xoxb-") {
		t.Fatal("secret persisted")
	}
}

func TestSetupUnknownCredentialReturnsError(t *testing.T) {
	p, ok := pluginapi.Legacy()
	if !ok {
		t.Skip("no implementation")
	}
	home := t.TempDir()
	a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	z := &wizard{Platform: p.Manifest().ID, Data: pluginapi.SetupState{"team_id": []byte(`"TFIXTURE"`)}, Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "unexpected": "synthetic-value"}, References: map[string]string{}}
	r := httptest.NewRequest("POST", "http://127.0.0.1/setup/step", strings.NewReader(url.Values{"workspace": {"fixture"}, "storage": {filepath.Join(home, "data", "fixture")}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	w := httptest.NewRecorder()
	a.storageStep(w, r, "synthetic-session", "csrf", z, p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Could not save workspace") {
		t.Fatal("unknown credential did not return safe error", w.Code, w.Body.String())
	}
}
