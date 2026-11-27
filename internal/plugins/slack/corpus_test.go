package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func TestFormatCorpus(t *testing.T) {
	paths, err := filepath.Glob("../../../testdata/slack/corpus/*.json")
	if err != nil || len(paths) < 100 {
		t.Fatalf("missing corpus: %d %v", len(paths), err)
	}
	for _, path := range paths {
		for _, hostile := range []bool{false, true} {
			name := filepath.Base(path)
			if hostile {
				name += "/all-strings-xss"
			}
			t.Run(name, func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if hostile {
					var source any
					if e := json.Unmarshal(raw, &source); e != nil {
						t.Fatal(e)
					}
					var inject func(any) any
					inject = func(v any) any {
						switch n := v.(type) {
						case string:
							return `corpusneedle <script>alert("all-fields")</script><img src=x onerror=alert(1)>`
						case []any:
							for i, v := range n {
								n[i] = inject(v)
							}
						case map[string]any:
							for k, v := range n {
								n[k] = inject(v)
							}
						}
						return v
					}
					raw, err = json.MarshalIndent(inject(source), "", "  ")
					if err != nil {
						t.Fatal(err)
					}
				}
				var response struct {
					Messages []pluginapi.Message `json:"messages"`
				}
				if err = json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				fake := slacktest.New(t)
				fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Write(raw)
				}
				ws := pluginapi.Workspace{ID: "fixture", Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}
				s, err := (slack.Plugin{}).NewSession(context.Background(), ws, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}, HTTP: fake.Client()})
				if err != nil {
					t.Fatal(err)
				}
				snap, err := s.Snapshot(context.Background(), "CFIXTURE", pluginapi.Root{TS: "100.000001", Raw: response.Messages[0]}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if (hostile || strings.Contains(path, "future_unknown") || strings.Contains(path, "list-limit")) && len(snap.Manifest.Gaps) == 0 {
					t.Fatal("missing per-item unrendered note")
				}
				dir := t.TempDir()
				store, err := storage.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				key := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
				if _, err = store.Save(context.Background(), key, snap); err != nil {
					t.Fatal(err)
				}
				saved, err := store.Read(context.Background(), key)
				if err != nil {
					t.Fatal(err)
				}
				preserved := false
				for name, b := range saved.Documents {
					if strings.HasPrefix(name, "raw/") && bytes.Equal(b, raw) {
						preserved = true
					}
				}
				if !preserved {
					t.Fatal("source response bytes changed")
				}
				view := httptest.NewRecorder()
				web.New(map[string]web.Source{"fixture": {Store: store, Content: slack.Hooks{}}}, "").ServeHTTP(view, httptest.NewRequest("GET", "/view/fixture/CFIXTURE/100.000001", nil))
				if view.Code != 200 || strings.Contains(view.Body.String(), "<script>alert") || strings.Contains(view.Body.String(), "<img src=x") {
					t.Fatalf("unsafe viewer: status %d", view.Code)
				}
				text, err := (slack.Hooks{}).Text(context.Background(), saved.Messages[0], saved.Users)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(text, "corpusneedle") {
					idx := search.New(t.TempDir())
					defer idx.Close()
					if err = idx.Configure(context.Background(), map[string]search.Source{"fixture": {Path: dir, Content: slack.Hooks{}, Identity: "slack@corpus"}}); err != nil {
						t.Fatal(err)
					}
					result, err := idx.Search(context.Background(), search.Query{Q: "corpusneedle"})
					if err != nil || len(result.Results) == 0 {
						t.Fatalf("search missing: %+v %v", result, err)
					}
				}
			})
		}
	}
}

func FuzzMessageDecoder(f *testing.F) {
	paths, _ := filepath.Glob("../../../testdata/slack/corpus/*.json")
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			f.Fatal(e)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var v any
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if d.Decode(&v) != nil {
			return
		}
		m, _ := v.(map[string]any)
		if a, ok := m["messages"].([]any); ok && len(a) > 0 {
			m, _ = a[0].(map[string]any)
		}
		_, _ = (slack.Hooks{}).Text(context.Background(), m, nil)
		_, _, _ = slack.Content(m)
		_ = slack.Event(m)
		_ = slack.Name(m)
	})
}
