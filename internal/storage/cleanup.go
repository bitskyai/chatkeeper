package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"os"
	"path"
	"path/filepath"
)

func (s *Folder) Path() string { return s.path }

// CleanupView holds the same lock as archive refresh for the entire destructive transaction.
// Callers must not call Folder methods from inside the callback.
type CleanupView struct{ s *Folder }

func (s *Folder) WithCleanup(ctx context.Context, fn func(*CleanupView) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.mkdir("cleanup"); e != nil {
		return e
	}
	u, e := lockFiles(ctx, s, "cleanup/lock")
	if e != nil {
		return e
	}
	defer u()
	return fn(&CleanupView{s})
}
func (v *CleanupView) Read(ctx context.Context, k pluginapi.Key) (*pluginapi.Snapshot, error) {
	target, e := v.s.target(k)
	if e != nil {
		return nil, e
	}
	snap, e := v.s.read(ctx, k, target)
	if e != nil || snap == nil {
		return snap, e
	}
	// Imported snapshots must reach stable storage too. Sync every verified object,
	// then its directory chain, before returning a proof to the cleanup engine.
	names := []string{"manifest.json"}
	for name := range snap.Manifest.Objects {
		names = append(names, name)
	}
	for _, name := range names {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		f, e := v.s.root.Open(target + "/" + name)
		if e != nil {
			return nil, e
		}
		e = f.Sync()
		ce := f.Close()
		if e != nil {
			return nil, e
		}
		if ce != nil {
			return nil, ce
		}
	}
	if len(snap.Blobs) > 0 {
		if e = v.s.syncDir(target + "/files"); e != nil {
			return nil, e
		}
	}
	for dir := target; ; dir = path.Dir(dir) {
		if e = v.s.syncDir(dir); e != nil {
			return nil, e
		}
		if dir == "." {
			break
		}
	}
	parent, e := os.Open(filepath.Dir(v.s.path))
	if e != nil {
		return nil, e
	}
	e = syncDirectory(parent)
	ce := parent.Close()
	if e != nil {
		return nil, e
	}
	if ce != nil {
		return nil, ce
	}
	return snap, nil
}
func (v *CleanupView) Journal(k pluginapi.Key) ([]byte, error) {
	if !validKey(k) {
		return nil, ErrIntegrity
	}
	name := "cleanup/" + k.Channel + "_" + k.RootTS + ".json"
	if _, e := v.s.root.Lstat(name); os.IsNotExist(e) {
		return nil, nil
	}
	if e := v.s.safe(name); e != nil {
		return nil, e
	}
	f, e := v.s.root.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, ErrIntegrity
	}
	return b, nil
}
func (v *CleanupView) SaveJournal(k pluginapi.Key, b []byte) error {
	if !validKey(k) || len(b) > 1<<20 || v.s.readOnly {
		return ErrIntegrity
	}
	if e := v.s.mkdir("cleanup"); e != nil {
		return e
	}
	nonce := make([]byte, 12)
	if _, e := rand.Read(nonce); e != nil {
		return e
	}
	stage := "cleanup/.journal-" + hex.EncodeToString(nonce)
	defer v.s.root.Remove(stage)
	if e := v.s.write(stage, bytes.NewReader(b), int64(len(b)), digest(b)); e != nil {
		return e
	}
	name := "cleanup/" + k.Channel + "_" + k.RootTS + ".json"
	if _, e := v.s.root.Lstat(name); e == nil {
		if e = v.s.safe(name); e != nil {
			return e
		}
	}
	if e := v.s.root.Rename(stage, name); e != nil {
		return e
	}
	return v.s.syncDir("cleanup")
}

// Annotate reads only metadata proved against the immutable manifest. It never rewrites content.
func (s *Folder) deletionOverlay(k pluginapi.Key, snap *pluginapi.Snapshot) error {
	b, e := (&CleanupView{s}).Journal(k)
	if e != nil {
		return e
	}
	if b == nil {
		return nil
	}
	var j struct {
		Version int               `json:"version"`
		Proof   string            `json:"proof"`
		Done    []string          `json:"done"`
		Deleted map[string]string `json:"source_deleted_at"`
	}
	if json.Unmarshal(b, &j) != nil || j.Version != 1 || j.Proof != digest(snap.Documents["manifest.json"]) {
		return ErrIntegrity
	}
	snap.SourceDeletedAt = map[string]string{}
	for _, ts := range j.Done {
		if at := j.Deleted[ts]; at != "" {
			snap.SourceDeletedAt[ts] = at
		}
	}
	return nil
}
