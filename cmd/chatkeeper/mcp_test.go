package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpFixture(t *testing.T) (string, string) {
	t.Helper()
	if _, ok := pluginapi.Legacy(); !ok {
		t.Skip("built-in excluded")
	}
	home := t.TempDir()
	cfg := filepath.Join(home, "config.yaml")
	if e := os.WriteFile(cfg, []byte("workspaces:\n- id: fixture\n  team_id: TFIXTURE\n  storage: {path: archive-store}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	src, e := storage.Open("../../internal/storage/testdata/golden-no-files")
	if e != nil {
		t.Fatal(e)
	}
	v, e := src.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	src.Close()
	if e != nil {
		t.Fatal(e)
	}
	dst, e := storage.Open(filepath.Join(home, "archive-store"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = dst.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, *v)
	dst.Close()
	if e != nil {
		t.Fatal(e)
	}
	return home, cfg
}
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("CHATKEEPER_MCP_HELPER") != "1" {
		return
	}
	if e := execute([]string{"mcp", "--config", os.Getenv("CHATKEEPER_MCP_CONFIG")}, os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	os.Exit(0)
}
func helperCommand(ctx context.Context, home, cfg string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPHelperProcess$")
	cmd.Env = append(os.Environ(), "CHATKEEPER_MCP_HELPER=1", "CHATKEEPER_HOME="+home, "CHATKEEPER_MCP_CONFIG="+cfg, "SLACK_BOT_TOKEN=", "SLACK_APP_TOKEN=", "CHATKEEPER_API_TOKEN=invalid-unused-token")
	return cmd
}
func exerciseMCP(t *testing.T, ctx context.Context, transport sdk.Transport) {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "fixture-client", Version: "1"}, nil)
	session, e := client.Connect(ctx, transport, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	if session.InitializeResult().ServerInfo.Name != "chatkeeper" {
		t.Fatal(session.InitializeResult())
	}
	tools, e := session.ListTools(ctx, &sdk.ListToolsParams{})
	if e != nil {
		t.Fatal(e)
	}
	names := []string{}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatal(tool)
		}
	}
	sort.Strings(names)
	want := []string{"get_thread", "list_workspaces", "search_threads"}
	if _, stdio := transport.(*sdk.CommandTransport); stdio {
		// Local stdio agents also get the read-only setup tools.
		want = []string{"get_thread", "list_workspaces", "search_threads", "setup_status", "validate_setup"}
		res, e := session.CallTool(ctx, &sdk.CallToolParams{Name: "setup_status", Arguments: map[string]any{}})
		if e != nil || res.IsError {
			t.Fatal("setup_status", e, res)
		}
		b, _ := json.Marshal(res.StructuredContent)
		if !strings.Contains(string(b), `"id":"fixture"`) || !strings.Contains(string(b), `"credentials":{`) || strings.Contains(string(b), "xoxb-") {
			t.Fatal("setup_status", string(b))
		}
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatal(names)
	}
	for _, tc := range []struct {
		name     string
		args     map[string]any
		isError  bool
		contains string
	}{
		{"search_threads", map[string]any{"q": "Synthetic", "workspace": "fixture", "limit": 1}, false, "100.000001"},
		{"search_threads", map[string]any{"q": "Synthetic", "author": "UOWNER"}, false, "100.000001"},
		{"get_thread", map[string]any{"workspace": "fixture", "channel": "CFIXTURE", "ts": "100.000001"}, false, "Synthetic reply"},
		{"list_workspaces", map[string]any{}, false, "fixture"},
		{"search_threads", map[string]any{"q": `"`}, true, ""},
		{"search_threads", map[string]any{"q": "archive", "limit": 0}, true, ""},
		{"get_thread", map[string]any{"workspace": "fixture", "channel": "CFIXTURE", "ts": "999.000001"}, true, ""},
	} {
		r, e := session.CallTool(ctx, &sdk.CallToolParams{Name: tc.name, Arguments: tc.args})
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(r)
		if r.IsError != tc.isError || !strings.Contains(string(b), tc.contains) {
			t.Fatalf("%s: %s", tc.name, b)
		}
		if strings.Contains(string(b), "archive-store") || strings.Contains(string(b), "invalid-unused-token") {
			t.Fatal("secret/path leak", string(b))
		}
	}
}
func TestMCPStdioClient(t *testing.T) {
	home, cfg := mcpFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := helperCommand(ctx, home, cfg)
	cmd.Stderr = &stderr
	defer func() {
		if t.Failed() {
			t.Log(stderr.String())
		}
	}()
	exerciseMCP(t, ctx, &sdk.CommandTransport{Command: cmd})
}

type bearerTransport struct{ base http.RoundTripper }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer synthetic-api-credential")
	return b.base.RoundTrip(r)
}
func TestMCPHTTPClient(t *testing.T) {
	home, cfg := mcpFixture(t)
	t.Setenv("CHATKEEPER_API_TOKEN", "synthetic-api-credential")
	a, e := web.NewApp(web.Options{Home: home, Config: cfg, Host: "127.0.0.1", Mode: "server"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	// Offline fixture: explicitly configure the shared service; runtime credentials
	// are absent and no live adapter is initialized.
	plugin, _ := pluginapi.Legacy()
	if e := a.Manager.Search.Configure(context.Background(), map[string]search.Source{"fixture": {Path: filepath.Join(home, "archive-store"), Content: plugin.Content()}}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exerciseMCP(t, ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{http.DefaultTransport}}, DisableStandaloneSSE: true})
	for _, origin := range []string{"https://untrusted.invalid", "null"} {
		r, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer synthetic-api-credential")
		r.Header.Set("Origin", origin)
		resp, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatal(resp.StatusCode)
		}
	}
}

type transcriptStep struct {
	Request   json.RawMessage `json:"request"`
	Expected  any             `json:"expected"`
	ToolNames []string        `json:"tool_names"`
}

func subset(t *testing.T, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("want object, got %v", got)
		}
		for k, v := range w {
			subset(t, v, g[k])
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			t.Fatalf("array: %v / %v", w, got)
		}
		for i := range w {
			subset(t, w[i], g[i])
		}
	default:
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("want %v got %v", want, got)
		}
	}
}
func TestMCPGoldenStdioTranscript(t *testing.T) {
	home, cfg := mcpFixture(t)
	b, e := os.ReadFile("../../internal/mcp/testdata/transcript.json")
	if e != nil {
		t.Fatal(e)
	}
	var steps []transcriptStep
	if e := json.Unmarshal(b, &steps); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := helperCommand(ctx, home, cfg)
	in, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		in.Close()
		if e := cmd.Wait(); e != nil {
			t.Errorf("process: %v %s", e, stderr.String())
		}
	}()
	reader := bufio.NewReader(out)
	for _, step := range steps {
		if _, e := fmt.Fprintln(in, string(step.Request)); e != nil {
			t.Fatal(e)
		}
		if step.Expected == nil && step.ToolNames == nil {
			continue
		}

		var response map[string]any
		for {
			line, e := reader.ReadBytes('\n')
			if e != nil {
				t.Fatalf("read: %v %s", e, stderr.String())
			}
			if e := json.Unmarshal(line, &response); e != nil {
				t.Fatalf("stdout not protocol: %s", line)
			}
			if response["id"] != nil {
				break
			}
			// The SDK may deliver its debounced initial tool-list notification before
			// the response. JSON-RPC permits such interleaving on persistent transports.
			if response["method"] != "notifications/tools/list_changed" {
				t.Fatalf("unexpected server notification: %s", line)
			}
		}

		if step.Expected != nil {
			subset(t, step.Expected, response)
		}
		if step.ToolNames != nil {
			tools := response["result"].(map[string]any)["tools"].([]any)
			names := []string{}
			for _, raw := range tools {
				tool := raw.(map[string]any)
				names = append(names, tool["name"].(string))
				subset(t, map[string]any{"readOnlyHint": true, "destructiveHint": false}, tool["annotations"])
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, append(append([]string{}, step.ToolNames...), "setup_status", "validate_setup")) {
				t.Fatal(names)
			}
		}
	}
}

func TestMCPGoldenHTTPTranscript(t *testing.T) {
	home, cfg := mcpFixture(t)
	t.Setenv("CHATKEEPER_API_TOKEN", "synthetic-api-credential")
	a, e := web.NewApp(web.Options{Home: home, Config: cfg, Host: "127.0.0.1", Mode: "server"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	plugin, _ := pluginapi.Legacy()
	if e := a.Manager.Search.Configure(context.Background(), map[string]search.Source{"fixture": {Path: filepath.Join(home, "archive-store"), Content: plugin.Content()}}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	data, e := os.ReadFile("../../internal/mcp/testdata/transcript.json")
	if e != nil {
		t.Fatal(e)
	}
	var steps []transcriptStep
	if e := json.Unmarshal(data, &steps); e != nil {
		t.Fatal(e)
	}
	for _, step := range steps {
		r, e := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(step.Request))
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2025-06-18")
		r.Header.Set("Authorization", "Bearer synthetic-api-credential")
		resp, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if step.Expected == nil && step.ToolNames == nil {
			if resp.StatusCode != 202 {
				t.Fatal(resp.StatusCode, string(body))
			}
			continue
		}
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode, string(body))
		}
		var response map[string]any
		if e := json.Unmarshal(body, &response); e != nil {
			t.Fatal(e, string(body))
		}
		if step.Expected != nil {
			subset(t, step.Expected, response)
		}
		if step.ToolNames != nil {
			result, ok := response["result"].(map[string]any)
			if !ok {
				t.Fatal(string(body))
			}
			tools, ok := result["tools"].([]any)
			if !ok {
				t.Fatal(string(body))
			}
			names := []string{}
			for _, raw := range tools {
				tool := raw.(map[string]any)
				names = append(names, tool["name"].(string))
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, step.ToolNames) {
				t.Fatal(names)
			}
		}
	}
}
func TestStdioDoesNotCreateStateOrChangeArchive(t *testing.T) {
	home, cfg := mcpFixture(t)
	snapshot := func() map[string]string {
		out := map[string]string{}
		e := filepath.WalkDir(home, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			key, _ := filepath.Rel(home, path)
			if d.IsDir() {
				out[key] = fmt.Sprint(info.Mode())
				return nil
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			out[key] = fmt.Sprintf("%s:%x", info.Mode(), sha256.Sum256(b))
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	before := snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exerciseMCP(t, ctx, &sdk.CommandTransport{Command: helperCommand(ctx, home, cfg)})
	after := snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stdio wrote configured home: before %v after %v", before, after)
	}
}

func TestStdioObservesExternalCapture(t *testing.T) {
	home, cfg := mcpFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "fixture-client", Version: "1"}, nil)
	session, e := client.Connect(ctx, &sdk.CommandTransport{Command: helperCommand(ctx, home, cfg)}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	st, e := storage.Open(filepath.Join(home, "archive-store"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	v, e := st.Read(ctx, k)
	if e != nil {
		t.Fatal(e)
	}
	v.Documents = nil
	v.Messages[0]["text"] = "Replacement captured after initialize"
	if _, e := st.Save(ctx, k, *v); e != nil {
		t.Fatal(e)
	}
	result, e := session.CallTool(ctx, &sdk.CallToolParams{Name: "search_threads", Arguments: map[string]any{"q": "Replacement"}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
	data, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(data), "100.000001") {
		t.Fatal("stdio snapshot is stale", string(data))
	}
}
