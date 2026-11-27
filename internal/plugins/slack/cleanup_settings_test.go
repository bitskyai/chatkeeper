package slack

import (
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type settingsTransport struct{ url string }

func (t settingsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.URL.Scheme = "http"
	c.URL.Host = strings.TrimPrefix(t.url, "http://")
	return http.DefaultTransport.RoundTrip(c)
}
func TestCleanupCredentialSettings(t *testing.T) {
	team, admin, scopes := "TFIXTURE", true, "chat:write,users:read,channels:history,groups:history"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth.test" {
			w.Header().Set("X-OAuth-Scopes", scopes)
			w.Write([]byte(`{"ok":true,"team_id":"` + team + `","user_id":"UOWNER"}`))
			return
		}
		if admin {
			w.Write([]byte(`{"ok":true,"user":{"id":"UOWNER","is_admin":true}}`))
		} else {
			w.Write([]byte(`{"ok":true,"user":{"id":"UOWNER"}}`))
		}
	}))
	defer srv.Close()
	env := pluginapi.Env{HTTP: &http.Client{Transport: settingsTransport{srv.URL}}}
	ws := pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
	test := func(token string, others bool, want string) {
		t.Helper()
		e := (Plugin{}).TestCleanupCredential(context.Background(), ws, env, token, others)
		if want == "" {
			if e != nil {
				t.Fatal(e)
			}
		} else if e == nil || !strings.Contains(e.Error(), want) {
			t.Fatalf("want %q, got %v", want, e)
		}
	}
	test("xoxp-synthetic", true, "")
	test("xoxb-synthetic", true, "admin/owner user token")
	team = "TOTHER"
	test("xoxp-synthetic", false, "workspace")
	team = "TFIXTURE"
	admin = false
	test("xoxp-synthetic", true, "admin/owner")
	admin = true
	scopes = "users:read"
	test("xoxp-synthetic", false, "chat:write")
	test("secret", false, "xoxp-")
}

func TestDeleteApprovalEventsRefreshArchive(t *testing.T) {
	ws := pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`), Trigger: pluginapi.Trigger{Reactions: []string{"white_check_mark"}}, Cleanup: pluginapi.CleanupPolicy{Approval: &pluginapi.Trigger{Who: "owner", Owners: []string{"UOWNER"}, Reactions: []string{"wastebasket"}}}}
	session, e := (Plugin{}).NewSession(context.Background(), ws, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}})
	if e != nil {
		t.Fatal(e)
	}
	s := session.(*Session)
	if !s.triggers["white_check_mark"] || !s.triggers["wastebasket"] {
		t.Fatal("approval reaction cannot refresh archive")
	}
	if len(ws.Trigger.Reactions) != 1 || ws.Trigger.Reactions[0] != "white_check_mark" {
		t.Fatal("archive trigger changed")
	}
}
