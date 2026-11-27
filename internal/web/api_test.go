package web

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRESTAuthValidationAndOpenAPI(t *testing.T) {
	t.Setenv("CHATKEEPER_API_TOKEN", "synthetic-api-credential")
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "server", Host: "127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, path := range []string{"/api/search?q=archive", "/api/openapi.json", "/api/threads/one/CFIXTURE/100.000001", "/mcp"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			r := httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s %d %s", method, path, w.Code, w.Body)
			}
		}
	}
	for _, tc := range []struct {
		path, method string
		code         int
	}{
		{"/api/search?q=archive", "GET", 200}, {"/api/openapi.json", "GET", 200}, {"/api/search", "GET", 400}, {"/api/search?q=%22", "GET", 400}, {"/api/search?q=a&limit=0", "GET", 400}, {"/api/search?q=a&limit=hello", "GET", 400}, {"/api/search?q=a&after=bad", "GET", 400}, {"/api/search?q=a&from=bad", "GET", 400}, {"/api/search?q=a&q=b", "GET", 400}, {"/api/search?q=a&unknown=x", "GET", 400}, {"/api/threads/one/CFIXTURE/999.000001", "GET", 404}, {"/api/search?q=archive", "POST", 405},
	} {
		t.Run(tc.path+tc.method, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1"+tc.path, nil)
			r.Header.Set("Authorization", "bEaReR synthetic-api-credential")
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			var obj map[string]any
			if json.Unmarshal(w.Body.Bytes(), &obj) != nil {
				t.Fatal("not JSON", w.Body)
			}
			if strings.Contains(w.Body.String(), a.opts.Home) || strings.Contains(w.Body.String(), "synthetic-api-credential") {
				t.Fatal("secret/path leaked")
			}
		})
	}
	// Browser session is accepted for REST; MCP remains bearer-only.
	rr := httptest.NewRequest("GET", "http://127.0.0.1/login", nil)
	rw := httptest.NewRecorder()
	if e := a.newSession(rw, rr); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/search?q=archive", "/search?q=archive", "/mcp"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		r.AddCookie(rw.Result().Cookies()[0])
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		want := http.StatusOK
		if path == "/mcp" {
			want = 401
		}
		if w.Code != want {
			t.Fatalf("session %s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestConfiguredBearerAndProxyOrigin(t *testing.T) {
	for _, kind := range []string{"custom", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			cfg := filepath.Join(home, "config.yaml")
			t.Setenv("CHATKEEPER_API_TOKEN", "stale-default")
			token := "custom-synthetic-credential"
			configText := "server: {token_env: CUSTOM_API_TOKEN}\nworkspaces: []\n"
			t.Setenv("CUSTOM_API_TOKEN", token)
			if kind == "legacy" {
				t.Setenv("CHATKEEPER_API_TOKEN", "")
				t.Setenv("THREADKEEP_API_TOKEN", token)
				configText = "workspaces: []\n"
			}
			if e := os.WriteFile(cfg, []byte(configText), 0600); e != nil {
				t.Fatal(e)
			}
			a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1", SecureCookies: true})
			if e != nil {
				t.Fatal(e)
			}
			defer a.Close()
			for _, path := range []string{"/api/search?q=archive", "/view/"} {
				for _, credential := range []string{token, "stale-default"} {
					r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
					r.Header.Set("Authorization", "Bearer "+credential)
					w := httptest.NewRecorder()
					a.ServeHTTP(w, r)
					want := 200
					if credential != token {
						want = 401
					}
					if w.Code != want {
						t.Fatal(path, credential, w.Code, w.Body)
					}
				}
			}
			for _, origin := range []string{"https://127.0.0.1", "https://127.0.0.1/path", "https://127.0.0.1?x=1"} {
				r := httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Origin", origin)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Accept", "application/json, text/event-stream")
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				want := 200
				if origin != "https://127.0.0.1" {
					want = 403
				}
				if w.Code != want {
					t.Fatal(origin, w.Code, w.Body)
				}
			}
		})
	}
}

func TestRESTArchiveQueriesPaginationAndSearchHTML(t *testing.T) {
	plugin, ok := pluginapi.Legacy()
	if !ok {
		t.Skip("built-in excluded")
	}
	t.Setenv("SYNTHETIC_BOT", "xoxb-synthetic")
	t.Setenv("CHATKEEPER_API_TOKEN", "synthetic-api-credential")
	home := t.TempDir()
	a, e := NewApp(Options{Home: home, Mode: "server", Host: "127.0.0.1"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	path := filepath.Join(home, "data")
	st, e := storage.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	fixture, e := storage.Open("../storage/testdata/golden-no-files")
	if e != nil {
		t.Fatal(e)
	}
	v, e := fixture.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	fixture.Close()
	if e != nil {
		t.Fatal(e)
	}
	for _, ts := range []string{"100.000001", "200.000001"} {
		v.Documents = nil
		v.Manifest.RootTS = ts
		v.Messages[0]["ts"] = ts
		v.Messages[1]["thread_ts"] = ts
		v.Messages[1]["ts"] = strings.Replace(ts, "00.", "01.", 1)
		if _, e := st.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: ts}, *v); e != nil {
			t.Fatal(e)
		}
	}
	data := fmt.Sprintf("workspaces:\n- id: one\n  platform: %s\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", plugin.Manifest().ID, path)
	if e := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	a.Manager.SetPaused("one", true)
	if e := a.Manager.Reload(); e != nil {
		t.Fatal(e)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		r.Header.Set("Authorization", "Bearer synthetic-api-credential")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	w := get("/api/search?q=Synthetic&author=UAUTHOR&limit=1")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var p search.Page
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil || len(p.Results) != 1 || p.NextCursor == nil {
		t.Fatal(p, e)
	}
	first := p.Results[0].RootTS
	w = get("/api/search?q=Synthetic&author=UAUTHOR&limit=1&after=" + url.QueryEscape(*p.NextCursor))
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil || len(p.Results) != 1 || p.NextCursor != nil || p.Results[0].RootTS == first {
		t.Fatal(w.Code, p, e)
	}
	for _, tc := range []struct {
		path     string
		code     int
		contains string
	}{{"/api/search?q=Synthetic&workspace=one&channel=CFIXTURE&from=100&to=101", 200, "100.000001"}, {"/api/threads/one/CFIXTURE/100.000001", 200, "Synthetic reply"}, {"/search?q=Synthetic&limit=1", 200, "Next page"}, {"/search?q=caf%C3%A9", 200, "CFIXTURE"}, {"/api/search?q=NEAR(foo,%20bad)", 400, "error"}, {"/api/search?q=*", 400, "error"}} {
		w := get(tc.path)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatal(tc, w.Code, w.Body)
		}
	}
	w = get("/api/threads/one/CFIXTURE/100.000001")
	if strings.Contains(w.Body.String(), "Blobs") || strings.Contains(w.Body.String(), home) {
		t.Fatal("internal archive data leaked")
	}
	// Removal affects query scope but leaves archive files intact.
	if e := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("workspaces: []\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := a.Manager.Reload(); e != nil {
		t.Fatal(e)
	}
	w = get("/api/search?q=Synthetic")
	if !strings.Contains(w.Body.String(), `"results":[]`) {
		t.Fatal(w.Body)
	}
	if _, e := os.Stat(filepath.Join(path, "archive", "CFIXTURE", "100.000001", "manifest.json")); e != nil {
		t.Fatal(e)
	}
}
