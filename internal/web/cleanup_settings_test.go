//go:build !noslack

package web_test

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/chromedp/chromedp"
	"github.com/zalando/go-keyring"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func cleanupSettingsFixture(t *testing.T) (*web.App, string, *http.Cookie, func(string, string, url.Values) *httptest.ResponseRecorder) {
	t.Helper()
	keyring.MockInit()
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-synthetic")
	fake := slacktest.New(t)
	fake.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "chat:write,users:read,channels:history,groups:history")
		fmt.Fprint(w, `{"ok":true,"team_id":"TFIXTURE","user_id":"UOWNER","bot_id":"BFIXTURE"}`)
	}
	fake.Overrides["users.list"] = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"members":[{"id":"UOWNER","name":"fixture","profile":{"display_name":"Owner <script>alert(1)</script>"}}]}`)
	}
	fake.Overrides["users.info"] = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"user":{"id":"UOWNER","is_owner":true}}`)
	}
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	cfg := "workspaces:\n - id: fixture\n   platform: slack\n   team_id: TFIXTURE\n   app_id: AFIXTURE\n   channels: [CFIXTURE]\n   storage: {type: folder, path: archive}\n   trigger: {who: any, reactions: [white_check_mark]}\n"
	if e := os.WriteFile(path, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	a, e := web.NewApp(web.Options{Home: home, Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(a.Close)
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	send := func(method, path string, v url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	return a, path, cookie, send
}
func TestCleanupSettingsSafetyAndRoundTrip(t *testing.T) {
	a, path, _, send := cleanupSettingsFixture(t)
	page := send("GET", "/settings/cleanup?workspace=fixture", nil)
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatal(page.Code, body)
	}
	for _, want := range []string{"Delete from Slack after archiving", "value=\"24\"", "wastebasket", "https://app.slack.com/app-settings/TFIXTURE/AFIXTURE/oauth", "Owner &lt;script&gt;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("unescaped member name")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)[1]
	v := url.Values{"workspace": {"fixture"}, "enabled": {"true"}, "reactions": {"wastebasket"}, "owners": {"UOWNER"}, "scope": {"thread"}, "min_age_hours": {"0"}, "every_hours": {"24"}, "cleanup_token": {"xoxb-synthetic-private"}}
	if w := send("POST", "/settings/cleanup", v); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	v.Set("csrf", csrf)
	w := send("POST", "/settings/cleanup", v)
	if !strings.Contains(w.Body.String(), "0 requires Delete right away") {
		t.Fatal(w.Code, w.Body.String())
	}
	v.Set("allow_zero_age", "true")
	v.Set("owners", "UINJECTED")
	w = send("POST", "/settings/cleanup", v)
	if !strings.Contains(w.Body.String(), "pick yourself") {
		t.Fatal(w.Code, w.Body.String())
	}
	v.Set("owners", "UOWNER")
	v.Set("action", "test")
	w = send("POST", "/settings/cleanup", v)
	if !strings.Contains(w.Body.String(), "Credential verified") {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "xoxb-synthetic-private") {
		t.Fatal("test echoed credential")
	}
	v.Set("action", "save")
	w = send("POST", "/settings/cleanup", v)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	cfg, e := config.Load(path, false)
	if e != nil {
		t.Fatal(e)
	}
	ws := cfg.Workspaces[0].Workspace
	if !ws.Cleanup.Enabled || ws.Cleanup.MinAge != 0 || ws.Cleanup.AllowOthers || !ws.Cleanup.OwnOnly || ws.Cleanup.Approval == nil || ws.Cleanup.Approval.Owners[0] != "UOWNER" || ws.Trigger.Reactions[0] != "white_check_mark" || ws.Trigger.Who != "any" || ws.Cleanup.Every != 0 {
		t.Fatalf("bad roundtrip: %+v", ws)
	}
	secret, _, e := a.Secrets.Get(ws.Cleanup.TokenEnv)
	if e != nil || secret != "xoxb-synthetic-private" {
		t.Fatal("credential not persisted", e)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "xoxb-") {
		t.Fatal("token serialized in config")
	}
	w = send("GET", "/settings/cleanup?workspace=fixture", nil)
	if strings.Contains(w.Body.String(), "xoxb-synthetic-private") {
		t.Fatal("saved credential rendered")
	}
	if e = os.Chmod(path, 0400); e != nil {
		t.Fatal(e)
	}
	w = send("GET", "/settings/cleanup?workspace=fixture", nil)
	if !strings.Contains(w.Body.String(), "Read-only") {
		t.Fatal("managed note absent")
	}
	if w = send("POST", "/settings/cleanup", v); w.Code != 409 {
		t.Fatal("managed config writable", w.Code)
	}
}
func TestBrowserCleanupSettings(t *testing.T) {
	if os.Getenv("CHATKEEPER_E2E") != "1" {
		t.Skip("set CHATKEEPER_E2E=1")
	}
	a, path, _, _ := cleanupSettingsFixture(t)
	server := httptest.NewServer(a)
	defer server.Close()
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.NoSandbox)...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	e := chromedp.Run(ctx, chromedp.Navigate(server.URL+"/login?token=synthetic-launch"), chromedp.Navigate(server.URL+"/settings/cleanup?workspace=fixture"), chromedp.WaitVisible(`input[name="enabled"]`), chromedp.Click(`input[name="enabled"]`), chromedp.Click(`input[name="owners"][value="UOWNER"]`), chromedp.SetValue(`input[name="cleanup_token"]`, "xoxb-synthetic-browser"), chromedp.SetValue(`input[name="min_age_hours"]`, "0"), chromedp.Click(`input[name="allow_zero_age"]`), chromedp.Click(`button[value="test"]`), chromedp.WaitVisible(`[role="status"]`), chromedp.SetValue(`input[name="cleanup_token"]`, "xoxb-synthetic-browser"), chromedp.Click(`button[value="save"]`), chromedp.WaitVisible(`//p[@role="status" and contains(., "Cleanup settings saved")]`, chromedp.BySearch))
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := config.Load(path, false)
	if e != nil || !cfg.Workspaces[0].Workspace.Cleanup.Enabled {
		t.Fatal("browser settings not saved", e)
	}
	var rendered string
	if e = chromedp.Run(ctx, chromedp.OuterHTML("html", &rendered)); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(rendered, "xoxb-synthetic-browser") {
		t.Fatal("credential rendered")
	}
}

func TestCleanupSettingsFailureKeepsPolicyAndOfflineDisable(t *testing.T) {
	a, path, _, send := cleanupSettingsFixture(t)
	page := send("GET", "/settings/cleanup?workspace=fixture", nil)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())[1]
	before, _ := os.ReadFile(path)
	// A broken credential-store metadata file rejects storage, including keychain.
	if e := os.WriteFile(filepath.Join(filepath.Dir(path), "secrets.backends.json"), []byte("invalid"), 0600); e != nil {
		t.Fatal(e)
	}
	v := url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "enabled": {"true"}, "reactions": {"wastebasket"}, "owners": {"UOWNER"}, "scope": {"thread"}, "min_age_hours": {"24"}, "every_hours": {"24"}, "cleanup_token": {"xoxb-synthetic-new"}, "action": {"save"}}
	w := send("POST", "/settings/cleanup", v)
	if !strings.Contains(w.Body.String(), "Could not save cleanup settings") {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed credential write published policy")
	}
	// Stopping is possible with no approvers, unusable credential storage or token.
	for _, action := range []string{"disable", "stop_schedule"} {
		w = send("POST", "/settings/cleanup", url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "action": {action}})
		if w.Code != 303 {
			t.Fatal("cannot stop", action, w.Code, w.Body.String())
		}
	}
	_ = a
}

func TestCleanupSettingsPreviewWarnings(t *testing.T) {
	_, path, _, send := cleanupSettingsFixture(t)
	b, _ := os.ReadFile(path)
	b = append(b, []byte("   cleanup: {enabled: true, approval: {who: owner, owners: [], reactions: []}}\n")...)
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	page := send("GET", "/cleanup/?workspace=fixture", nil)
	for _, want := range []string{"No delete approval reaction configured", "No cleanup owners configured", "Deletion credential missing", "Fix in Settings"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatal("missing warning", want, page.Code, page.Body.String())
		}
	}
}

func TestCleanupCredentialDraftLongWorkspace(t *testing.T) {
	_, path, _, send := cleanupSettingsFixture(t)
	id := strings.Repeat("w", 64)
	b, _ := os.ReadFile(path)
	b = []byte(strings.Replace(string(b), "id: fixture", "id: "+id, 1))
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	page := send("GET", "/settings/cleanup?workspace="+id, nil)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())[1]
	v := url.Values{"csrf": {csrf}, "workspace": {id}, "enabled": {"true"}, "reactions": {"wastebasket"}, "owners": {"UOWNER"}, "scope": {"thread"}, "min_age_hours": {"24"}, "every_hours": {"24"}, "cleanup_token": {"xoxb-synthetic-draft"}, "action": {"test"}}
	w := send("POST", "/settings/cleanup", v)
	if !strings.Contains(w.Body.String(), "Credential verified") {
		t.Fatal(w.Code, w.Body.String())
	}
	draft := regexp.MustCompile(`name="credential_draft" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
	v.Set("credential_draft", draft)
	v.Set("cleanup_token", "")
	v.Set("action", "save")
	w = send("POST", "/settings/cleanup", v)
	if w.Code != 303 {
		t.Fatal("Test→Save with blank token failed", w.Code, w.Body.String())
	}
}

func TestScheduleRequiresTypedConfirmation(t *testing.T) {
	_, path, _, send := cleanupSettingsFixture(t)
	page := send("GET", "/settings/cleanup?workspace=fixture", nil)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())[1]
	v := url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "enabled": {"true"}, "automatic": {"true"}, "every_hours": {"1"}, "min_age_hours": {"24"}, "scope": {"thread"}, "owners": {"UOWNER"}, "reactions": {"wastebasket"}, "cleanup_token": {"xoxb-synthetic"}}
	before, _ := os.ReadFile(path)
	w := send("POST", "/settings/cleanup", v)
	after, _ := os.ReadFile(path)
	if w.Code == 303 || string(before) != string(after) {
		t.Fatal("schedule bypassed confirmation")
	}
	v.Set("confirm", "fixture")
	if w = send("POST", "/settings/cleanup", v); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	cfg, e := config.Load(path, false)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Workspaces[0].Workspace.Cleanup.Every != time.Hour {
		t.Fatal("schedule not saved")
	}
	since := cfg.Workspaces[0].Workspace.Cleanup.ScheduleSince
	v.Set("every_hours", "2")
	v.Del("confirm")
	if w = send("POST", "/settings/cleanup", v); w.Code == 303 {
		t.Fatal("changed schedule skipped confirmation")
	}
	v.Set("confirm", "fixture")
	if w = send("POST", "/settings/cleanup", v); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	cfg, e = config.Load(path, false)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Workspaces[0].Workspace.Cleanup.Every != 2*time.Hour || !cfg.Workspaces[0].Workspace.Cleanup.ScheduleSince.After(since) {
		t.Fatal("changed schedule did not reset first interval")
	}
}

func TestUncheckedEnableSavesOtherEditsAndReplacesDraft(t *testing.T) {
	a, path, _, send := cleanupSettingsFixture(t)
	page := send("GET", "/settings/cleanup?workspace=fixture", nil)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())[1]
	v := url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "enabled": {"true"}, "every_hours": {"24"}, "min_age_hours": {"24"}, "scope": {"thread"}, "owners": {"UOWNER"}, "reactions": {"wastebasket"}, "cleanup_token": {"xoxb-synthetic"}, "action": {"test"}}
	w := send("POST", "/settings/cleanup", v)
	draft := regexp.MustCompile(`name="credential_draft" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
	v.Set("credential_draft", draft)
	w = send("POST", "/settings/cleanup", v)
	next := regexp.MustCompile(`name="credential_draft" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
	if secret, _, err := a.Secrets.Get(draft); err != nil || secret != "" {
		t.Fatal("old draft retained", err)
	}
	v.Set("credential_draft", next)
	v.Set("cleanup_token", "")
	v.Set("action", "save")
	if w = send("POST", "/settings/cleanup", v); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	v.Del("enabled")
	v.Set("scope", "messages")
	v.Set("min_age_hours", "48")
	if w = send("POST", "/settings/cleanup", v); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	cfg, err := config.Load(path, false)
	if err != nil {
		t.Fatal(err)
	}
	ws := cfg.Workspaces[0].Workspace
	if ws.Cleanup.Enabled || ws.Cleanup.Scope != "messages" || ws.Cleanup.MinAge != 48*time.Hour {
		t.Fatal("unchecked save discarded edits", ws.Cleanup)
	}
}

func TestFailedConfigPublishRemovesCredential(t *testing.T) {
	_, path, _, send := cleanupSettingsFixture(t)
	page := send("GET", "/settings/cleanup?workspace=fixture", nil)
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())[1]
	if err := os.Mkdir(path+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	v := url.Values{"csrf": {csrf}, "workspace": {"fixture"}, "enabled": {"true"}, "every_hours": {"24"}, "min_age_hours": {"24"}, "scope": {"thread"}, "owners": {"UOWNER"}, "reactions": {"wastebasket"}, "cleanup_token": {"xoxb-synthetic"}, "action": {"save"}}
	w := send("POST", "/settings/cleanup", v)
	if w.Code == 303 {
		t.Fatal("expected config publication failure")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "secrets.backends.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "_DRAFT_") {
		t.Fatal("failed publication orphaned credential")
	}
}
