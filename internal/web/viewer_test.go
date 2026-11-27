package web

import (
	"context"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestViewerSecurity(t *testing.T) {
	source, e := storage.Open("../storage/testdata/golden")
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	v, e := source.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	if e != nil {
		t.Fatal(e)
	}
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	v.Documents = nil
	v.Messages[0]["text"] = "<script>alert(1)</script> <@UMENTION>"
	if _, e = store.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, *v); e != nil {
		t.Fatal(e)
	}
	h := New(map[string]Source{"fixture": {Store: store, Content: slack.Hooks{}}}, "synthetic-bearer")
	for _, tc := range []struct {
		path, method, auth string
		status             int
		contains           string
	}{
		{"/healthz", "GET", "", 200, "ok"}, {"/view/fixture/CFIXTURE/100.000001", "GET", "", 401, ""}, {"/view/fixture/CFIXTURE/100.000001", "GET", "Bearer wrong", 401, ""}, {"/view/fixture/CFIXTURE/100.000001", "GET", "Bearer synthetic-bearer", 200, "&lt;script&gt;"}, {"/view/fixture/", "GET", "Bearer synthetic-bearer", 200, "100.000001"}, {"/view/fixture/CFIXTURE/100.000001/files/FFILE.bin", "GET", "Bearer synthetic-bearer", 200, "test"}, {"/view/fixture/CFIXTURE/100.000001", "POST", "Bearer synthetic-bearer", 405, ""}, {"/view/unknown/", "GET", "Bearer synthetic-bearer", 404, ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatalf("%s %d %s", tc.path, w.Code, w.Body)
		}
		if w.Code == 200 && strings.HasPrefix(tc.path, "/view") {
			if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'none'") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
				t.Fatal("missing CSP")
			}
			if strings.Contains(w.Body.String(), "<script>") {
				t.Fatal("XSS")
			}
		}
	}
}
func TestViewerRejectsCorrupt(t *testing.T) {
	dir := t.TempDir()
	s, _ := storage.Open(dir)
	defer s.Close()
	src, _ := storage.Open("../storage/testdata/golden")
	defer src.Close()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	v, e := src.Read(context.Background(), k)
	if e != nil {
		t.Fatal(e)
	}
	s.Save(context.Background(), k, *v)
	os.WriteFile(filepath.Join(dir, "archive", k.Channel, k.RootTS, "files/FFILE.bin"), []byte("wrong"), 0600)
	w := httptest.NewRecorder()
	New(map[string]Source{"fixture": {Store: s, Content: slack.Hooks{}}}, "").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/view/fixture/CFIXTURE/100.000001", nil))
	if w.Code == 200 {
		t.Fatal("corrupt page served")
	}
}
func TestGlobalMentionPrecedence(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "newest global", true: "thread wins"}[own], func(t *testing.T) {
			dir := t.TempDir()
			store, _ := storage.Open(dir)
			defer store.Close()
			src, _ := storage.Open("../storage/testdata/golden")
			defer src.Close()
			k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
			v, e := src.Read(context.Background(), k)
			if e != nil {
				t.Fatal(e)
			}
			v.Documents = nil
			v.Messages[0]["text"] = "ask <@UX|stale> <@UBAD> <@UPROF> <@UNUM>"
			v.RawUsers["UBAD"] = "not a mapping"
			v.Users["UPROF"] = pluginapi.Message{"id": "UPROF", "profile": "bad", "name": "fixture-handle"}
			v.Users["UNUM"] = pluginapi.Message{"id": "UNUM", "profile": map[string]any{"display_name": 7}, "real_name": "Fixture fallback"}
			if own {
				v.Users["UX"] = pluginapi.Message{"id": "UX", "profile": map[string]any{"display_name": "Own fixture"}}
			}
			if _, e = store.Save(context.Background(), k, *v); e != nil {
				t.Fatal(e)
			}
			for _, ts := range []string{"300.000001", "200.000001"} {
				other := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, TeamID: "TFIXTURE", Channel: "CSECOND", RootTS: ts, Permalink: "https://fixture.slack.com/archives/CSECOND/p" + strings.ReplaceAll(ts, ".", ""), MessageCount: 1, Files: []pluginapi.Attachment{}}, Messages: []pluginapi.Message{{"ts": ts, "user": "UOWNER", "text": "Synthetic other", "reply_count": 0}}, Users: map[string]pluginapi.Message{"UOWNER": v.Users["UOWNER"], "UX": {"id": "UX", "profile": map[string]any{"display_name": map[string]string{"300.000001": "<b>Newest fixture</b>", "200.000001": "Older fixture"}[ts]}}}, Blobs: map[string]pluginapi.Blob{}}
				if _, e = store.Save(context.Background(), pluginapi.Key{Channel: "CSECOND", RootTS: ts}, other); e != nil {
					t.Fatal(e)
				}
			}
			w := httptest.NewRecorder()
			New(map[string]Source{"fixture": {Store: store, Content: slack.Hooks{}}}, "").ServeHTTP(w, httptest.NewRequest("GET", "/view/fixture/CFIXTURE/100.000001", nil))
			want := "@&lt;b&gt;Newest fixture&lt;/b&gt;"
			if own {
				want = "@Own fixture"
			}
			for _, text := range []string{want, "@UBAD", "@fixture-handle", "@Fixture fallback"} {
				if w.Code != 200 || !strings.Contains(w.Body.String(), text) {
					t.Fatal(w.Code, text, w.Body.String())
				}
			}
			if strings.Contains(w.Body.String(), "stale") || strings.Contains(w.Body.String(), "<b>Newest") {
				t.Fatal("unsafe or stale name")
			}
		})
	}
}

func TestBindAndRemovalStub(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "viewer.invalid"} {
		if _, e := NewForBind(nil, "", host); e == nil {
			t.Fatal("unauthenticated public bind accepted")
		}
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		if _, e := NewForBind(nil, "", host); e != nil {
			t.Fatal(e)
		}
	}
	h, e := NewForBind(nil, "synthetic-bearer", "0.0.0.0")
	if e != nil {
		t.Fatal(e)
	}
	for _, auth := range []string{"", "Bearer synthetic-bearer"} {
		req := httptest.NewRequest("DELETE", "/api/archives/fixture/CFIXTURE/100.000001", nil)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		want := 401
		if auth != "" {
			want = 405
			if w.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal(w.Header())
			}
		}
		if w.Code != want {
			t.Fatal(w.Code, want)
		}
	}
}

type countingStore struct {
	storage.Store
	reads int
}

func (s *countingStore) Read(ctx context.Context, k pluginapi.Key) (*pluginapi.Snapshot, error) {
	s.reads++
	return s.Store.Read(ctx, k)
}
func TestMentionCacheAvoidsRepeatedArchiveScans(t *testing.T) {
	store, e := storage.Open("../storage/testdata/golden")
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	count := &countingStore{Store: store}
	h := New(map[string]Source{"fixture": {Store: count, Content: slack.Hooks{}}}, "")
	request := func() {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/view/fixture/CFIXTURE/100.000001", nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	request()
	before := count.reads
	request()
	if count.reads-before != 1 {
		t.Fatal("repeated request rescanned global snapshots", count.reads-before)
	}
}

type blockedCatalog struct {
	storage.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockedCatalog) Keys(context.Context) ([]pluginapi.Key, error) {
	close(s.entered)
	<-s.release
	return nil, nil
}
func TestMentionScanDoesNotBlockOtherWorkspaces(t *testing.T) {
	store, e := storage.Open("../storage/testdata/golden")
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	s := &blockedCatalog{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	a, e := NewApp(Options{Home: t.TempDir(), Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	login := httptest.NewRecorder()
	a.ServeHTTP(login, httptest.NewRequest("GET", "http://127.0.0.1/login?token=synthetic-launch", nil))
	cookie := login.Result().Cookies()[0]
	v := New(map[string]Source{"slow": {Store: s, Content: slack.Hooks{}}, "ready": {Store: store, Content: slack.Hooks{}}}, "")
	a.viewerCache = map[string]*Viewer{"": v}
	a.viewerGeneration = a.Manager.Generation()
	request := func(workspace string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://127.0.0.1/view/"+workspace+"/CFIXTURE/100.000001", nil)
		r.AddCookie(cookie)
		a.ServeHTTP(w, r)
		return w.Code
	}
	done := make(chan int, 1)
	go func() { done <- request("slow") }()
	<-s.entered
	other := make(chan int, 1)
	go func() { other <- request("ready") }()
	select {
	case code := <-other:
		close(s.release)
		if code != 200 || <-done != 200 {
			t.Fatal("viewer failed", code)
		}
	case <-time.After(time.Second):
		close(s.release)
		<-done
		<-other
		t.Fatal("archive scan blocked another workspace through App")
	}
}

func TestViewerLabelsSourceDeletedMessage(t *testing.T) {
	ctx := context.Background()
	src, err := storage.Open("../storage/testdata/golden")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	key := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	snapshot, err := src.Read(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot.Documents = nil
	snapshot.Manifest.Schema = 2
	snapshot.Messages[1]["source_deleted_at"] = "2026-11-27T02:00:00Z"
	snapshot.Messages[1]["text"] = "Retained deleted reply"
	if _, err = store.Save(ctx, key, *snapshot); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	New(map[string]Source{"fixture": {Store: store, Content: slack.Hooks{}}}, "").ServeHTTP(w, httptest.NewRequest("GET", "/view/fixture/CFIXTURE/100.000001", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, want := range []string{"Deleted at source", "2026-11-27T02:00:00Z", "Retained deleted reply"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %q in %s", want, w.Body.String())
		}
	}
	if strings.Count(w.Body.String(), "Deleted at source") != 1 {
		t.Fatal("label must apply only to deleted message")
	}
}

// Exact-only viewer matching used to omit the marker for a toned reaction.
func TestViewerSkinToneTrigger(t *testing.T) {
	ctx := context.Background()
	src, err := storage.Open("../storage/testdata/golden")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	key := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	snapshot, err := src.Read(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Documents = nil
	snapshot.Messages[0]["reactions"] = []any{map[string]any{"name": "+1::skin-tone-3", "count": 1, "users": []any{"UOWNER"}}}
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Save(ctx, key, *snapshot); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		trigger string
		marked  bool
	}{{"+1", true}, {"+1::skin-tone-3", true}, {"+1::skin-tone-2", false}, {"bookmark", false}} {
		t.Run(tc.trigger, func(t *testing.T) {
			w := httptest.NewRecorder()
			New(map[string]Source{"fixture": {Store: store, Content: slack.Hooks{}, Triggers: []string{tc.trigger}}}, "").ServeHTTP(w, httptest.NewRequest("GET", "/view/fixture/CFIXTURE/100.000001", nil))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if got := strings.Contains(w.Body.String(), `class="reaction trigger"`); got != tc.marked {
				t.Fatalf("trigger marker=%v want %v: %s", got, tc.marked, w.Body.String())
			}
		})
	}
}

func TestConnectionChecksRedactCredentials(t *testing.T) {
	const secret = "xoxb-synthetic-never-render"
	pg := page{Kind: "step", Title: "Connection", Checks: []pluginapi.Check{{Label: "Workspace " + secret, Detail: "API returned " + secret, OK: true}}}
	app := &App{}
	app.redactPage(&pg, pluginapi.Credentials{"bot_token": secret})
	w := httptest.NewRecorder()
	app.render(w, pg)
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("connection diagnostics echoed credential", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[redacted]") {
		t.Fatal("diagnostics were omitted instead of redacted")
	}
}
