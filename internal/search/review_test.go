package search

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Holding a capture's archive lock must not stop indexed stdio searches.
func TestReadOnlyQueryDoesNotWaitForCapture(t *testing.T) {
	_, st, home := corpus(t)
	ro := NewReadOnly(home)
	defer ro.Close()
	if e := ro.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}); e != nil {
		t.Fatal(e)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- st.Catalog(context.Background(), func(_ storage.CatalogVisitor) error { close(entered); <-release; return nil })
	}()
	<-entered
	defer func() { close(release); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p, e := ro.Search(ctx, Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatalf("query blocked behind capture: %v %v", p, e)
	}
}

func TestEmptyThreadInsert(t *testing.T) {
	s, _, _ := corpus(t)
	w := s.workspaces["one"]
	tx, e := w.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e := w.insert(context.Background(), tx, pluginapi.Key{Channel: "CFIXTURE", RootTS: "400.000001"}, &pluginapi.Snapshot{}); e != nil {
		t.Fatal(e)
	}
}

func TestChineseSubstringSearch(t *testing.T) {
	s, st, _ := corpus(t)
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	v, e := st.Read(context.Background(), k)
	if e != nil {
		t.Fatal(e)
	}
	v.Documents = nil
	v.Messages[0]["text"] = "欢迎使用聊天记录搜索工具"
	if _, e := st.Save(context.Background(), k, *v); e != nil {
		t.Fatal(e)
	}
	if e := s.Sync(context.Background(), "one", k); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"聊天记录", "搜索", "聊"} {
		p, e := s.Search(context.Background(), Query{Q: q})
		if e != nil || len(p.Results) != 1 {
			t.Errorf("%s: %v %v", q, p, e)
		}
	}
}

type pausedHooks struct {
	textHooks
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *pausedHooks) Text(ctx context.Context, m pluginapi.Message, u map[string]pluginapi.Message) (string, error) {
	h.once.Do(func() { close(h.entered) })
	select {
	case <-h.release:
		return h.textHooks.Text(ctx, m, u)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestCaptureProceedsDuringReadOnlyFallback(t *testing.T) {
	_, st, home := corpus(t)
	ro := NewReadOnly() // no disk index, exercise the optimistic fallback
	defer ro.Close()
	hooks := &pausedHooks{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- ro.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: hooks}})
	}()
	<-hooks.entered
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Read and Save use the same exclusive lock as a capture publication.
	v, e := st.Read(ctx, k)
	if e == nil {
		v.Documents = nil
		_, e = st.Save(ctx, k, *v)
	}
	close(hooks.release)
	configureErr := <-done
	if e != nil {
		t.Fatalf("capture blocked by stdio fallback: %v", e)
	}
	if configureErr != nil {
		t.Fatal(configureErr)
	}
}

func TestReadOnlyDiskRejectsWritesAndHandlesCorruptIndex(t *testing.T) {
	s, _, home := corpus(t)
	// Exercise a checkpointed disk reader on every OS; Windows uses a private
	// in-memory fallback while the writer owns a live WAL index.
	s.Close()
	ro := NewReadOnly(home)
	defer ro.Close()
	sources := map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}
	if e := ro.Configure(context.Background(), sources); e != nil {
		t.Fatal(e)
	}
	probe, unlock, err := readIndex(context.Background(), filepath.Join(home, "state", "index", "one.db"), sources["one"].origin())
	if err != nil {
		t.Fatal(err)
	}
	_, err = probe.Exec("DELETE FROM threads")
	probe.Close()
	if unlock != nil {
		unlock()
	}
	if err == nil {
		t.Fatal("read-only index accepted write")
	}
	ro.Close()
	s.Close()
	if e := os.WriteFile(filepath.Join(home, "state", "index", "one.db"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := ro.Configure(context.Background(), sources); e != nil {
		t.Fatal(e)
	}
	p, e := ro.Search(context.Background(), Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal(p, e)
	}
	b, e := os.ReadFile(filepath.Join(home, "state", "index", "one.db"))
	if e != nil || string(b) != "corrupt" {
		t.Fatal("fallback changed index", e)
	}
}

func TestOfflineReadOnlyDoesNotCreateSidecars(t *testing.T) {
	s, _, home := corpus(t)
	s.Close()
	dir := filepath.Join(home, "state", "index")
	before, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	ro := NewReadOnly(home)
	defer ro.Close()
	if e := ro.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}); e != nil {
		t.Fatal(e)
	}
	p, e := ro.Search(context.Background(), Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal(p, e)
	}
	after, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	names := func(entries []os.DirEntry) []string {
		out := []string{}
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	if !reflect.DeepEqual(names(before), names(after)) {
		t.Fatalf("sidecars created: %v -> %v", names(before), names(after))
	}
}

func TestWorkspaceRankingPagination(t *testing.T) {
	s, _, home := corpus(t)
	src := Source{Path: stPath(home), Content: textHooks{}}
	if e := s.Configure(context.Background(), map[string]Source{"one": src, "two": src}); e != nil {
		t.Fatal(e)
	}
	q := Query{Q: "searchable", Limit: 1}
	workspaces := []string{}
	for i := 0; i < 7; i++ {
		p, e := s.Search(context.Background(), q)
		if e != nil {
			t.Fatal(e)
		}
		for _, h := range p.Results {
			workspaces = append(workspaces, h.Workspace)
		}
		if p.NextCursor == nil {
			break
		}
		q.After = *p.NextCursor
	}
	if !reflect.DeepEqual(workspaces, []string{"one", "one", "one", "two", "two", "two"}) {
		t.Fatal(workspaces)
	}
}

func TestReadOnlySubprocessDoesNotWriteLiveIndex(t *testing.T) {
	if home := os.Getenv("CHATKEEPER_TEST_RO_HOME"); home != "" {
		ro := NewReadOnly(home)
		defer ro.Close()
		if e := ro.Configure(context.Background(), map[string]Source{"one": {Path: stPath(home), Content: textHooks{}}}); e != nil {
			t.Fatal(e)
		}
		p, e := ro.Search(context.Background(), Query{Q: "searchable"})
		if e != nil || len(p.Results) != 3 {
			t.Fatal(p, e)
		}
		return
	}
	_, _, home := corpus(t)
	dir := filepath.Join(home, "state", "index")
	snapshot := func() map[string]string {
		entries, e := os.ReadDir(dir)
		if e != nil {
			t.Fatal(e)
		}
		out := map[string]string{}
		for _, entry := range entries {
			if entry.Name() == ".writer.lock" || entry.Name() == ".publication.lock" {
				// Windows byte-range locks prohibit ReadFile on a live lock.
				// Verify the empty lock's existence and metadata instead.
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				if info.Size() != 0 {
					t.Fatal("nonempty lock file", entry.Name())
				}
				out[entry.Name()] = fmt.Sprintf("%s:%d:%s", info.Mode(), info.Size(), info.ModTime())
				continue
			}
			b, e := os.ReadFile(filepath.Join(dir, entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			out[entry.Name()] = string(b)
		}
		return out
	}
	before := snapshot()
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	command := exec.Command(binary, "-test.run=^TestReadOnlySubprocessDoesNotWriteLiveIndex$")
	command.Env = append(os.Environ(), "CHATKEEPER_TEST_RO_HOME="+home)
	if output, e := command.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, output)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("stdio reader changed live database or sidecars")
	}
}

func TestReadOnlyFallsBackOnCorruptFTSPostings(t *testing.T) {
	s, _, home := corpus(t)
	ro := NewReadOnly(home)
	defer ro.Close()
	src := Source{Path: stPath(home), Content: textHooks{}}
	if _, e := s.workspaces["one"].db.Exec("UPDATE threads_data SET block=x'FF' WHERE id>10"); e != nil {
		t.Fatal(e)
	}
	// Checkpoint the damaged index so Windows exercises the same disk path;
	// live-WAL reads deliberately use its private fallback instead.
	s.Close()
	if e := ro.Configure(context.Background(), map[string]Source{"one": src}); e != nil {
		t.Fatal(e)
	}
	probe, unlock, e := readIndex(context.Background(), filepath.Join(home, "state", "index", "one.db"), src.origin())
	if e != nil {
		t.Fatal("expected corruption beyond opening probes", e)
	}
	var n int
	e = probe.QueryRow("SELECT count(*) FROM threads WHERE threads MATCH 'searchable'").Scan(&n)
	probe.Close()
	if unlock != nil {
		unlock()
	}
	if e == nil {

		t.Fatal("fixture did not corrupt postings")
	}
	p, e := ro.Search(context.Background(), Query{Q: "searchable"})
	if e != nil || len(p.Results) != 3 {
		t.Fatal("late corruption did not use fallback", p, e)
	}
}
