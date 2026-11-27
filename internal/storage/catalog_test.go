package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCatalogIsolationAndFailures(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	v := fixture(t)
	s.Save(ctx, key, v)
	other := pluginapi.Key{Channel: "CSECOND", RootTS: "200.000001"}
	v.Documents = nil
	v.Manifest.Channel = other.Channel
	v.Manifest.RootTS = other.RootTS
	v.Messages[0]["ts"] = other.RootTS
	v.Messages[1]["thread_ts"] = other.RootTS
	if _, e := s.Save(ctx, other, v); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(s.path, "archive", key.Channel, key.RootTS, "users.json"), []byte("bad"), 0600)
	os.WriteFile(filepath.Join(s.path, "archive", key.Channel, "stray"), []byte("private"), 0600)
	if _, e := s.Read(ctx, key); e == nil {
		t.Fatal("corrupt read")
	}
	keys, e := s.Keys(ctx)
	if e != nil || len(keys) != 2 {
		t.Fatal(keys, e)
	}
	if len(s.Failures()) == 0 {
		t.Fatal("discovery cleared integrity failure")
	}
	if _, e := s.Read(ctx, other); e != nil {
		t.Fatal("healthy snapshot unavailable", e)
	}
	b, _ := os.ReadFile(filepath.Join(s.path, "archive", key.Channel, key.RootTS, "users.json"))
	if string(b) != "bad" {
		t.Fatal("corruption silently repaired")
	}
}
func TestLazyBlobRechecksIntegrity(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.Save(ctx, key, fixture(t))
	v, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(s.path, "archive", key.Channel, key.RootTS, "files/FFILE.bin"), []byte("wrong"), 0600)
	if _, e := v.Blobs["files/FFILE.bin"].Open(); e == nil {
		t.Fatal("changed file accepted")
	}
}
func TestCleanupPin(t *testing.T) {
	for _, tc := range []struct {
		name        string
		journal     any
		wantErr     bool
		wantChanged bool
	}{
		{"progress", map[string]any{"version": 1, "targets": []string{"a", "b"}, "done": []string{"a"}, "pending": nil}, true, false},
		{"pending", map[string]any{"version": 1, "targets": []string{"a"}, "done": []string{}, "pending": "a"}, true, false},
		{"completed", map[string]any{"version": 1, "targets": []string{"a"}, "done": []string{"a"}, "pending": nil}, false, false},
		{"empty", map[string]any{"version": 1, "targets": []string{"a"}, "done": []string{}, "pending": nil}, false, true},
		{"invalid", map[string]any{"version": 2}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			defer s.Close()
			v := fixture(t)
			s.Save(ctx, key, v)
			os.Mkdir(filepath.Join(s.path, "cleanup"), 0700)
			b, _ := json.Marshal(tc.journal)
			os.WriteFile(filepath.Join(s.path, "cleanup", key.Channel+"_"+key.RootTS+".json"), b, 0600)
			v.Documents = nil
			v.Messages[0]["text"] = "edited"
			changed, e := s.Save(ctx, key, v)
			if (e != nil) != tc.wantErr || changed != tc.wantChanged {
				t.Fatal(changed, e)
			}
		})
	}
}
func TestLockCancellation(t *testing.T) {
	dir := t.TempDir()
	a, _ := Open(dir)
	defer a.Close()
	b, _ := Open(dir)
	defer b.Close()
	unlock, e := a.locked(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	c, cancel := context.WithCancel(ctx)
	cancel()
	if unlock, e := b.locked(c); e == nil {
		unlock()
		t.Fatal("lock ignored cancellation")
	}
}
func TestCatalogStreamsLargeAttachment(t *testing.T) {
	const size = 24 << 20
	source := t.TempDir()
	f, e := os.Create(filepath.Join(source, "large.bin"))
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.New()
	if _, e = io.Copy(io.MultiWriter(f, h), io.LimitReader(zeroReader{}, size)); e != nil {
		t.Fatal(e)
	}
	f.Close()
	sum := hex.EncodeToString(h.Sum(nil))
	v := fixture(t)
	v.Documents = nil
	v.Blobs["files/FFILE.bin"] = pluginapi.Blob{Size: size, SHA256: sum, Source: &pluginapi.FileSource{Root: source, Path: "large.bin"}}
	v.Manifest.Files[0].Size = size
	v.Manifest.Files[0].SHA256 = sum
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	read, e := s.Read(ctx, key)
	runtime.ReadMemStats(&after)
	if e != nil {
		t.Fatal(e)
	}
	if after.TotalAlloc-before.TotalAlloc > 4<<20 {
		t.Fatalf("read buffered attachment: %d bytes allocated", after.TotalAlloc-before.TotalAlloc)
	}
	if len(read.Blobs["files/FFILE.bin"].Data) != 0 {
		t.Fatal("attachment materialized")
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
