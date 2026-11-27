package search

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func TestPythonGoldenParity(t *testing.T) {
	var corpus struct {
		Threads []struct {
			Workspace string              `json:"workspace"`
			Channel   string              `json:"channel"`
			RootTS    string              `json:"root_ts"`
			Messages  []pluginapi.Message `json:"messages"`
		} `json:"threads"`
		Queries []Query `json:"queries"`
	}
	b, e := os.ReadFile("testdata/corpus.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &corpus); e != nil {
		t.Fatal(e)
	}
	var expected [][]Hit
	b, e = os.ReadFile("testdata/python-results.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &expected); e != nil {
		t.Fatal(e)
	}
	home := t.TempDir()
	sources := map[string]Source{}
	ctx := context.Background()
	for _, thread := range corpus.Threads {
		path := filepath.Join(home, thread.Workspace)
		st, e := storage.Open(path)
		if e != nil {
			t.Fatal(e)
		}
		v := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, TeamID: "TFIXTURE", Channel: thread.Channel, RootTS: thread.RootTS, MessageCount: len(thread.Messages)}, Messages: thread.Messages, Users: map[string]pluginapi.Message{"UOWNER": {"id": "UOWNER"}, "UAUTHOR": {"id": "UAUTHOR"}}}
		_, e = st.Save(ctx, pluginapi.Key{Channel: thread.Channel, RootTS: thread.RootTS}, v)
		st.Close()
		if e != nil {
			t.Fatal(e)
		}
		sources[thread.Workspace] = Source{Path: path, Content: textHooks{}}
	}
	s := New(home)
	defer s.Close()
	if e := s.Configure(ctx, sources); e != nil {
		t.Fatal(e)
	}
	for i, q := range corpus.Queries {
		p, e := s.Search(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		want := expected[i]
		// The prototype interleaves incomparable BM25 scores. Preserve its
		// independently generated scores/snippets but group by workspace.
		sort.SliceStable(want, func(i, j int) bool { return want[i].Workspace < want[j].Workspace })
		if len(p.Results) != len(want) {
			t.Fatalf("%+v: %v want %v", q, p.Results, want)
		}
		for j, got := range p.Results {
			w := want[j]
			if got.Workspace != w.Workspace || got.Channel != w.Channel || got.RootTS != w.RootTS || (!cjkLiteral(q.Q) && (got.Snippet != w.Snippet || math.Abs(got.Score-w.Score) > 1e-12)) {
				t.Fatalf("%+v: %+v want %+v", q, got, w)
			}
		}
	}
}
