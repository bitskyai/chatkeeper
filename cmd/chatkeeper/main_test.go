package main

import (
	"bytes"
	"fmt"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if e := execute([]string{"version"}, &out); e != nil || !strings.Contains(out.String(), "plugins:") {
		t.Fatal(out.String(), e)
	}
}
func TestOfflineCLI(t *testing.T) {
	if _, ok := pluginapi.Legacy(); !ok {
		t.Skip("no built-in plugins")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(config, []byte(`workspaces:
- id: fixture
  team_id: TFIXTURE
  channels: [CFIXTURE]
  storage: {path: data}
`), 0600); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"check-config", "--offline", "--config", config}, {"verify", "--config", config}} {
		var out bytes.Buffer
		if e := execute(args, &out); e != nil {
			t.Fatal(args, e)
		}
		if out.Len() == 0 {
			t.Fatal("no output")
		}
	}
	if e := execute([]string{"unknown"}, &bytes.Buffer{}); e == nil {
		t.Fatal("unknown command accepted")
	}
}
func TestCheckConfigResolvesSavedCredentials(t *testing.T) {
	if _, ok := pluginapi.Legacy(); !ok {
		t.Skip("implementation excluded")
	}
	home := t.TempDir()
	t.Setenv("CHATKEEPER_HOME", home)
	os.WriteFile(filepath.Join(home, "secrets.env"), []byte("FIXTURE_BOT=xoxb-synthetic\n"), 0600)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("# UI managed\nworkspaces:\n- id: fixture\n  team_id: TFIXTURE\n  bot_token_env: FIXTURE_BOT\n  storage: {path: archive}\n"), 0600)
	var out bytes.Buffer
	if e := execute([]string{"check-config", "--mode", "server"}, &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "configuration valid: 1") {
		t.Fatal(out.String())
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"run", "--help"}, {"verify", "-h"}, {"help", "run"}, {"version", "--help"}, {"healthcheck", "--help"}} {
		var out bytes.Buffer
		if err := execute(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, want := range []string{"Usage:", "check-config", "backfill", "verify", "mcp", "version", "--mode", "--bind", "CHATKEEPER_ADMIN_PASSWORD_HASH"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v: missing %q in %s", args, want, out.String())
			}
		}
	}
}

// Catches probes that ignore HTTP failures, follow redirects, or use the wrong route.
func TestHealthcheck(t *testing.T) {
	for _, status := range []int{200, 204, 302, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/healthz" {
					t.Errorf("unexpected probe: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Location", "/login")
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := execute([]string{"healthcheck", "--url", server.URL + "/healthz"}, &bytes.Buffer{})
			if (err == nil) != (status >= 200 && status < 300) {
				t.Fatalf("status %d: %v", status, err)
			}
		})
	}
}
func TestHealthcheckUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	if err := execute([]string{"healthcheck", "--url", server.URL + "/healthz"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unreachable server accepted")
	}
}
func TestHealthcheckTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	if err := execute([]string{"healthcheck", "--url", server.URL + "/healthz", "--timeout", "20ms"}, &bytes.Buffer{}); err == nil {
		t.Fatal("stalled server accepted")
	}
}

func TestLogsCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHATKEEPER_HOME", home)
	if e := os.MkdirAll(filepath.Join(home, "logs"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(home, "logs", "chatkeeper.log"), []byte("{\"msg\":\"poll\"}\n{\"msg\":\"archive\"}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e := execute([]string{"logs", "--filter", "archive"}, &out); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.String(), "poll") || !strings.Contains(out.String(), "archive") {
		t.Fatal(out.String())
	}
}
