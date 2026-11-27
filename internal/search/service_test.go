package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

type textHooks struct{}

func (textHooks) Validate(context.Context, pluginapi.Snapshot) error { return nil }
func (textHooks) Text(_ context.Context, m pluginapi.Message, _ map[string]pluginapi.Message) (string, error) {
	s, _ := m["text"].(string)
	return s, nil
}
func (textHooks) Author(_ context.Context, m pluginapi.Message) (string, error) {
	return "Display name", nil
}
func corpus(t *testing.T) (*Service, *storage.Folder, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "archive-store")
	st, e := storage.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	fixture, e := storage.Open("../storage/testdata/golden-no-files")
	if e != nil {
		t.Fatal(e)
	}
	v, e := fixture.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	fixture.Close()
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range []struct{ ts, text string }{{"100.000001", "searchable archive café 東京"}, {"200.000001", "searchable reply"}, {"300.000001", "searchable archive archive archive"}} {
		v.Documents = nil
		v.Manifest.RootTS = x.ts
		v.Manifest.MessageCount = 1
		v.Messages = []pluginapi.Message{{"ts": x.ts, "user": "UOWNER", "text": x.text, "reply_count": 0}}
		if _, e := st.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: x.ts}, *v); e != nil {
			t.Fatal(e)
		}
	}
	s := New(home)
	t.Cleanup(func() { s.Close() })
	if e := s.Configure(context.Background(), map[string]Source{"one": {Path: path, Content: textHooks{}}}); e != nil {
		t.Fatal(e)
	}
	return s, st, home
}
func TestSearchBootstrapFiltersAndUnicode(t *testing.T) {
	s, _, _ := corpus(t)
	ctx := context.Background()
	for _, q := range []string{"archive", "cafe", "東京", "arch*", "archive OR reply"} {
		p, e := s.Search(ctx, Query{Q: q})
		if e != nil || len(p.Results) == 0 {
			t.Fatalf("%s: %+v %v", q, p, e)
		}
	}
	p, e := s.Search(ctx, Query{Q: "searchable", Workspace: "one", Channel: "CFIXTURE", Author: "UOWNER", From: "1970-01-01T00:02:00Z", To: "200.000002"})
	if e != nil || len(p.Results) != 1 || p.Results[0].RootTS != "200.000001" {
		t.Fatal(p, e)
	}
	p, e = s.Search(ctx, Query{Q: "archive", Author: "UNOBODY"})
	if e != nil || len(p.Results) != 0 {
		t.Fatal(p, e)
	}
	p, e = s.Search(ctx, Query{Q: "archive"})
	if e != nil || p.Results[0].RootTS != "300.000001" {
		t.Fatal(p, e)
	}
	th, e := s.GetThread(ctx, "one", "CFIXTURE", "100.000001")
	if e != nil || th.Messages[0]["text"] != "searchable archive café 東京" {
		t.Fatal(th, e)
	}
}
func TestPaginationAndValidation(t *testing.T) {
	s, _, _ := corpus(t)
	ctx := context.Background()
	q := Query{Q: "searchable", Limit: 1}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		p, e := s.Search(ctx, q)
		if e != nil || len(p.Results) != 1 || seen[p.Results[0].RootTS] {
			t.Fatal(p, e)
		}
		seen[p.Results[0].RootTS] = true
		if i < 2 && p.NextCursor == nil {
			t.Fatal("missing cursor")
		}
		if i == 2 && p.NextCursor != nil {
			t.Fatal("extra cursor")
		}
		if p.NextCursor != nil {
			q.After = *p.NextCursor
		}
	}
	for _, q := range []Query{{Q: ""}, {Q: `"`}, {Q: "archive", Limit: -1}, {Q: "archive", Limit: 101}, {Q: "archive", After: "invalid"}, {Q: "archive", From: "nan"}, {Q: "archive", From: "1e1000"}, {Q: "archive", From: "300", To: "100"}, {Q: "archive", Workspace: "../escape"}} {
		if _, e := s.Search(ctx, q); !errors.Is(e, ErrQuery) {
			t.Fatalf("%+v: %v", q, e)
		}
	}
	p, _ := s.Search(ctx, Query{Q: "searchable", Limit: 1})
	if _, e := s.Search(ctx, Query{Q: "archive", After: *p.NextCursor}); !errors.Is(e, ErrQuery) {
		t.Fatal(e)
	}
}
func TestRebuildMissingCorruptAndStale(t *testing.T) {
	s, st, home := corpus(t)
	ctx := context.Background()
	before, _ := s.Search(ctx, Query{Q: "archive"})
	s.Close()
	db := filepath.Join(home, "state", "index", "one.db")
	if e := os.Remove(db); e != nil {
		t.Fatal(e)
	}
	src := map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}
	if e := s.Configure(ctx, src); e != nil {
		t.Fatal(e)
	}
	after, _ := s.Search(ctx, Query{Q: "archive"})
	if !reflect.DeepEqual(before, after) {
		t.Fatal(before, after)
	}
	s.Close()
	if e := os.WriteFile(db, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := s.Configure(ctx, src); e != nil {
		t.Fatal(e)
	}
	v, _ := st.Read(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	v.Documents = nil
	v.Messages[0]["text"] = "replacement"
	if _, e := st.Save(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, *v); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if e := s.Configure(ctx, src); e != nil {
		t.Fatal(e)
	}
	p, e := s.Search(ctx, Query{Q: "replacement"})
	if e != nil || len(p.Results) != 1 {
		t.Fatal(p, e)
	}
}
func stPath(home string) string { return filepath.Join(home, "archive-store") }

func TestNeverDropIncrementalAndStaleCursor(t *testing.T) {
	s, st, _ := corpus(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	first, _ := s.Search(ctx, Query{Q: "searchable", Limit: 1})
	v, _ := st.Read(ctx, k)
	v.Documents = nil
	v.Messages[0]["text"] = "replacement"
	if _, e := st.Save(ctx, k, *v); e != nil {
		t.Fatal(e)
	}
	if e := s.Sync(ctx, "one", k); e != nil {
		t.Fatal(e)
	}
	p, e := s.Search(ctx, Query{Q: "café"})
	if e != nil || len(p.Results) != 1 || !p.Results[0].PriorVersions {
		t.Fatal(p, e)
	}
	if _, e := s.Search(ctx, Query{Q: "searchable", After: *first.NextCursor}); !errors.Is(e, ErrQuery) {
		t.Fatal("cursor survived edit", e)
	}
	v, _ = st.Read(ctx, k)
	v.Documents = nil
	v.Manifest.Schema = 2
	v.Manifest.MessageCount = 0
	v.Messages = []pluginapi.Message{}
	if _, e := st.Save(ctx, k, *v); e != nil {
		t.Fatal(e)
	}
	if e := s.Sync(ctx, "one", k); e != nil {
		t.Fatal(e)
	}
	p, e = s.Search(ctx, Query{Q: "replacement"})
	if e != nil || len(p.Results) != 1 || !p.Results[0].SourceDeleted {
		t.Fatal(p, e)
	}
	// An unchanged capture repairs a missed index write.
	if e := s.Rebuild(ctx, "one"); e != nil {
		t.Fatal(e)
	}
	if e := s.Sync(ctx, "one", k); e != nil {
		t.Fatal(e)
	}
	th, e := s.GetThread(ctx, "one", "CFIXTURE", k.RootTS)
	if e != nil || th.Messages[0]["source_deleted_at"] == nil {
		t.Fatal(th, e)
	}
}
func TestConcurrentCaptureRebuildAndQueries(t *testing.T) {
	s, st, _ := corpus(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				switch worker {
				case 0:
					v, e := st.Read(ctx, k)
					if e != nil {
						errs <- e
						return
					}
					v.Documents = nil
					v.Messages[0]["text"] = fmt.Sprintf("searchable generation %d", i)
					if _, e = st.Save(ctx, k, *v); e != nil {
						errs <- e
						return
					}
					if e = s.Sync(ctx, "one", k); e != nil {
						errs <- e
						return
					}
				case 1:
					if e := s.Rebuild(ctx, "one"); e != nil {
						errs <- e
						return
					}
				case 2:
					p, e := s.Search(ctx, Query{Q: "searchable"})
					if e != nil {
						errs <- e
						return
					}
					if len(p.Results) != 3 {
						errs <- fmt.Errorf("partial catalog: %d", len(p.Results))
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	p, e := s.Search(ctx, Query{Q: `"generation 7"`})
	if e != nil || len(p.Results) != 1 {
		t.Fatal(p, e)
	}
}
func TestRebuildOmitsCorruptAndRemovedThreads(t *testing.T) {
	s, _, home := corpus(t)
	ctx := context.Background()
	path := filepath.Join(stPath(home), "archive", "CFIXTURE")
	if e := os.RemoveAll(filepath.Join(path, "100.000001")); e != nil {
		t.Fatal(e)
	}
	damaged := filepath.Join(path, "200.000001", "messages.json")
	if e := os.WriteFile(damaged, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := s.Rebuild(ctx, "one"); e != nil {
		t.Fatal(e)
	}
	p, e := s.Search(ctx, Query{Q: "searchable"})
	if e != nil || len(p.Results) != 1 || p.Results[0].RootTS != "300.000001" {
		t.Fatal(p, e)
	}
	b, _ := os.ReadFile(damaged)
	if string(b) != "corrupt" {
		t.Fatal("changed archive")
	}
	if _, e := s.GetThread(ctx, "one", "CUNKNOWN", "100.000001"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(stPath(home), "archive", "CUNKNOWN")); !os.IsNotExist(e) {
		t.Fatal("read created directory")
	}
	if _, e := s.GetThread(ctx, "one", "CFIXTURE", "200.000001"); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}

func TestFailedConfigureRetainsSearchAndThread(t *testing.T) {
	s, st, home := corpus(t)
	ctx := context.Background()
	candidatePath := filepath.Join(home, "candidate")
	candidate, e := storage.Open(candidatePath)
	if e != nil {
		t.Fatal(e)
	}
	defer candidate.Close()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	v, e := st.Read(ctx, k)
	if e != nil {
		t.Fatal(e)
	}
	v.Documents = nil
	v.Messages[0]["text"] = "candidate secret"
	if _, e := candidate.Save(ctx, k, *v); e != nil {
		t.Fatal(e)
	}
	bad := filepath.Join(home, "bad")
	if e := os.WriteFile(bad, []byte("bad"), 0600); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 20; i++ {
		e = s.Configure(ctx, map[string]Source{"one": {Path: candidatePath, Content: textHooks{}}, "zzz": {Path: bad, Content: rejectHooks{}}})
		if e == nil {
			t.Fatal("accepted rejected configuration")
		}
		p, e := s.Search(ctx, Query{Q: "candidate"})
		if e != nil || len(p.Results) != 0 {
			t.Fatal("rejected index leaked", p, e)
		}
		th, e := s.GetThread(ctx, "one", "CFIXTURE", k.RootTS)
		if e != nil || th.Messages[0]["text"] != "searchable archive café 東京" {
			t.Fatal(th, e)
		}
	}
}

type rejectHooks struct{ textHooks }

func (rejectHooks) Validate(context.Context, pluginapi.Snapshot) error { return errors.New("rejected") }

func TestLogicalIndexDamageRebuilds(t *testing.T) {
	for _, damage := range []string{"DROP TABLE threads", "DELETE FROM threads", "DELETE FROM origin"} {
		t.Run(damage, func(t *testing.T) {
			s, _, home := corpus(t)
			s.Close()
			db, e := sql.Open("sqlite", filepath.Join(home, "state", "index", "one.db"))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.Exec(damage); e != nil {
				t.Fatal(e)
			}
			db.Close()
			if e := s.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}); e != nil {
				t.Fatal(e)
			}
			p, e := s.Search(context.Background(), Query{Q: "searchable"})
			if e != nil || len(p.Results) != 3 {
				t.Fatal(p, e)
			}
		})
	}
}
func TestQuerySyntaxErrors(t *testing.T) {
	s, _, _ := corpus(t)
	for _, q := range []string{`NEAR(foo, bad)`, `*`, `foo AND`, `(`, `unknown:foo`} {
		if _, e := s.Search(context.Background(), Query{Q: q}); !errors.Is(e, ErrQuery) {
			t.Fatalf("%s: %v", q, e)
		}
	}
}
func TestReadOnlyServiceObservesCaptureWithoutDiskWrites(t *testing.T) {
	s, st, home := corpus(t)
	before, e := os.Stat(filepath.Join(home, "state", "index", "one.db"))
	if e != nil {
		t.Fatal(e)
	}
	ro := NewReadOnly(home)
	defer ro.Close()
	if e := ro.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}); e != nil {
		t.Fatal(e)
	}
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	v, _ := st.Read(context.Background(), k)
	v.Documents = nil
	v.Messages[0]["text"] = "live replacement"
	if _, e := st.Save(context.Background(), k, *v); e != nil {
		t.Fatal(e)
	}
	if e := s.Sync(context.Background(), "one", k); e != nil {
		t.Fatal(e)
	}
	p, e := ro.Search(context.Background(), Query{Q: "replacement"})
	if e != nil || len(p.Results) != 1 {
		t.Fatal(p, e)
	}
	after, e := os.Stat(filepath.Join(home, "state", "index", "one.db"))
	if e != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("reader changed disk database", e)
	}
}
func TestExclusiveIndexWriter(t *testing.T) {
	s, _, home := corpus(t)
	other := New(home)
	defer other.Close()
	if e := other.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	p, e := s.Search(context.Background(), Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal(p, e)
	}
}

type panicHooks struct{ textHooks }

func (panicHooks) Text(context.Context, pluginapi.Message, map[string]pluginapi.Message) (string, error) {
	panic("private credential")
}

type hangHooks struct {
	textHooks
	release <-chan struct{}
}

func (h hangHooks) Text(context.Context, pluginapi.Message, map[string]pluginapi.Message) (string, error) {
	<-h.release
	return "late", nil
}
func TestPluginFailuresAreBoundedAndSanitized(t *testing.T) {
	s, _, home := corpus(t)
	s.Close()
	for _, kind := range []string{"panic", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			s := NewReadOnly(home)
			defer s.Close()
			var hooks pluginapi.ContentHooks = panicHooks{}
			release := make(chan struct{})
			defer close(release)
			if kind == "deadline" {
				hooks = hangHooks{release: release}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if e := s.Configure(ctx, map[string]Source{"one": {Path: stPath(home), Content: hooks}}); !errors.Is(e, ErrUnavailable) || strings.Contains(e.Error(), "private") {
				t.Fatal(e)
			}
			s.Close()
		})
	}
}
func TestOpaqueRootAndExplicitTime(t *testing.T) {
	s, st, _ := corpus(t)
	ctx := context.Background()
	for _, x := range []struct{ id, timestamp string }{{"opaque-one", "1970-01-01T00:05:00Z"}, {"opaque-two", ""}} {
		v, _ := st.Read(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
		v.Documents = nil
		v.Manifest.RootTS = x.id
		v.Messages = []pluginapi.Message{{"ts": x.id, "user": "UOWNER", "text": "opaque needle"}}
		if x.timestamp != "" {
			v.Messages[0]["timestamp"] = x.timestamp
		}
		if _, e := st.Save(ctx, pluginapi.Key{Channel: "CFIXTURE", RootTS: x.id}, *v); e != nil {
			t.Fatal(e)
		}
		if e := s.Sync(ctx, "one", pluginapi.Key{Channel: "CFIXTURE", RootTS: x.id}); e != nil {
			t.Fatal(e)
		}
	}
	p, e := s.Search(ctx, Query{Q: "opaque"})
	if e != nil || len(p.Results) != 2 {
		t.Fatal(p, e)
	}
	p, e = s.Search(ctx, Query{Q: "opaque", From: "299.999999", To: "300.000001"})
	if e != nil || len(p.Results) != 1 || p.Results[0].RootTS != "opaque-one" {
		t.Fatal(p, e)
	}
}
func TestTimeValidationAndPrecision(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{{"200.000001", 200000001}, {"1970-01-01T00:03:20.000001Z", 200000001}, {"1970-01-01T01:03:20.000001+01:00", 200000001}, {"1970-01-01T00:03:20.000001", 200000001}, {"2e2", 200000000}} {
		got, e := parseTime(tc.value)
		if e != nil || got != tc.want {
			t.Fatal(tc, got, e)
		}
	}
	for _, v := range []string{"nan", "1e1000000000", "1/2", "-1", "9223372036854.775808", "nonsense"} {
		if _, e := parseTime(v); !errors.Is(e, ErrQuery) {
			t.Fatal(v, e)
		}
	}
}

func TestPublicationFailureRestoresOldIndex(t *testing.T) {
	s, _, home := corpus(t)
	ctx := context.Background()
	candidate, e := openIndex(filepath.Join(home, "candidate.db"))
	if e != nil {
		t.Fatal(e)
	}
	st, e := storage.OpenReader(stPath(home))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	w := &workspace{source: Source{Path: stPath(home), Content: textHooks{}}, store: st, db: candidate}
	if e := w.rebuild(ctx, true); e != nil {
		t.Fatal(e)
	}
	other, e := memoryIndex()
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	next := map[string]*workspace{"one": w, "zzz": {source: w.source, store: st, db: other}}
	if e := s.publish(filepath.Join(home, "state", "index"), next, map[string]string{"one": filepath.Join(home, "candidate.db"), "zzz": filepath.Join(home, "missing-candidate.db")}); e == nil {
		t.Fatal("publication succeeded with missing candidate")
	}
	p, e := s.Search(ctx, Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal("old configuration unavailable", p, e)
	}
	if _, e := os.Stat(filepath.Join(home, "state", "index", "one.db")); e != nil {
		t.Fatal(e)
	}
}

func TestExtractionIdentityAndScopedReadOnly(t *testing.T) {
	s, _, home := corpus(t)
	ctx := context.Background()
	if e := s.Configure(ctx, map[string]Source{"one": {Path: stPath(home), Content: rejectHooks{}}}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("accepted rejecting adapter", e)
	}
	p, e := s.Search(ctx, Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal(p, e)
	}
	ro := NewReadOnly(home)
	defer ro.Close()
	otherPath := filepath.Join(home, "other")
	other, e := storage.Open(otherPath)
	if e != nil {
		t.Fatal(e)
	}
	other.Close()
	if e := ro.Configure(ctx, map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}, "two": {Path: otherPath, Content: textHooks{}}}); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(filepath.Join(otherPath, "archive"), filepath.Join(otherPath, "unmounted")); e != nil {
		t.Fatal(e)
	}
	p, e = ro.Search(ctx, Query{Q: "searchable", Workspace: "one"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal(p, e)
	}
}
func TestSharedCaptureUpdatesOwnerAndRejectsWrongSource(t *testing.T) {
	s, st, home := corpus(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	v, _ := st.Read(ctx, k)
	v.Documents = nil
	v.Messages[0]["text"] = "shared external capture"
	if _, e := st.Save(ctx, k, *v); e != nil {
		t.Fatal(e)
	}
	if e := SyncShared(ctx, home, "one", Source{Path: stPath(home), Content: textHooks{}}, k); e != nil {
		t.Fatal(e)
	}
	p, e := s.Search(ctx, Query{Q: "external"})
	if e != nil || len(p.Results) != 1 {
		t.Fatal(p, e)
	}
	if e := SyncShared(ctx, home, "one", Source{Path: stPath(home), Content: textHooks{}, Identity: "different"}, k); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}

type brokenManifest struct{ pluginapi.Plugin }

func (brokenManifest) Manifest() pluginapi.Manifest { panic("private credential") }
func TestSourceMetadataPanicIsSanitized(t *testing.T) {
	if _, e := FromPlugin(context.Background(), "synthetic", brokenManifest{}); !errors.Is(e, ErrUnavailable) || strings.Contains(e.Error(), "private") {
		t.Fatal(e)
	}
}
