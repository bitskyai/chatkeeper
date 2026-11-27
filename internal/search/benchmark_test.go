package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func benchmarkCorpus(b *testing.B) (*Service, string) {
	b.Helper()
	home := b.TempDir()
	archive := filepath.Join(home, "data")
	for i := 0; i < 10000; i++ {
		ts := fmt.Sprintf("%d.000001", 100+i)
		v := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, TeamID: "TFIXTURE", Channel: "CFIXTURE", RootTS: ts, MessageCount: 1}, Messages: []pluginapi.Message{{"ts": ts, "user": "UOWNER", "text": fmt.Sprintf("searchable synthetic archive number %d café 東京", i)}}, Users: map[string]pluginapi.Message{"UOWNER": {"id": "UOWNER"}}}
		v, e := storage.Prepare(v)
		if e != nil {
			b.Fatal(e)
		}
		dir := filepath.Join(archive, "archive", "CFIXTURE", ts)
		if e := os.MkdirAll(dir, 0700); e != nil {
			b.Fatal(e)
		}
		for name, data := range v.Documents {
			if e := os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
				b.Fatal(e)
			}
		}
	}
	s := New(home)
	b.Cleanup(s.Close)
	if e := s.Configure(context.Background(), map[string]Source{"fixture": {Path: archive, Content: textHooks{}}}); e != nil {
		b.Fatal(e)
	}
	return s, home
}
func BenchmarkRebuild10K(b *testing.B) {
	s, _ := benchmarkCorpus(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if e := s.Rebuild(context.Background(), "fixture"); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkQuery10K(b *testing.B) {
	s, _ := benchmarkCorpus(b)
	times := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		p, e := s.Search(context.Background(), Query{Q: "searchable", Limit: 20})
		times = append(times, time.Since(start))
		if e != nil || len(p.Results) != 20 {
			b.Fatal(p, e)
		}
	}
	b.StopTimer()
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	if len(times) > 0 {
		b.ReportMetric(float64(times[(len(times)*95+99)/100-1].Microseconds())/1000, "p95-ms")
	}
}

func BenchmarkReadOnlyQuery10K(b *testing.B) {
	_, home := benchmarkCorpus(b)
	s := NewReadOnly(home)
	defer s.Close()
	if e := s.Configure(context.Background(), map[string]Source{"fixture": {Path: filepath.Join(home, "data"), Content: textHooks{}}}); e != nil {
		b.Fatal(e)
	}
	times := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		p, e := s.Search(context.Background(), Query{Q: "searchable"})
		times = append(times, time.Since(start))
		if e != nil || len(p.Results) != 20 {
			b.Fatal(p, e)
		}
	}
	b.StopTimer()
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	if len(times) > 0 {
		b.ReportMetric(float64(times[(len(times)*95+99)/100-1].Microseconds())/1000, "p95-ms")
	}
}
