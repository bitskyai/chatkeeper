// Package storage implements the versioned source-of-truth folder archive.
package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

const MaxJSON int64 = 16 << 20
const MaxFile int64 = 100 << 20
const MaxSnapshot int64 = 512 << 20

var ErrIntegrity = errors.New("snapshot integrity verification failed")
var ErrPinned = errors.New("snapshot pinned by cleanup")
var segment = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)
var objectPath = regexp.MustCompile(`^(messages\.json|users\.json|files/[A-Za-z0-9_-]+\.bin)$`)

type Failure struct {
	Key   pluginapi.Key
	Class string
}
type Store interface {
	Keys(context.Context) ([]pluginapi.Key, error)
	Read(context.Context, pluginapi.Key) (*pluginapi.Snapshot, error)
	Save(context.Context, pluginapi.Key, pluginapi.Snapshot) (bool, error)
	Failures() []Failure
}
type Folder struct {
	readOnly bool
	path     string
	root     *os.Root
	mu       sync.Mutex
	failures map[pluginapi.Key]Failure
}

func Open(location string) (*Folder, error) {
	info, e := os.Lstat(location)
	if e == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrIntegrity
	}
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if e = os.MkdirAll(location, 0700); e != nil {
		return nil, e
	}
	r, e := os.OpenRoot(location)
	if e != nil {
		return nil, e
	}
	s := &Folder{path: location, root: r, failures: map[pluginapi.Key]Failure{}}
	if e = s.mkdir("archive"); e != nil {
		r.Close()
		return nil, e
	}
	return s, nil
}
func (s *Folder) Close() error { return s.root.Close() }
func (s *Folder) Failures() []Failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Failure, 0, len(s.failures))
	for _, f := range s.failures {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key.Channel+out[i].Key.RootTS < out[j].Key.Channel+out[j].Key.RootTS
	})
	return out
}
func (s *Folder) fail(k pluginapi.Key) { s.failures[k] = Failure{k, "integrity"} }
func validKey(k pluginapi.Key) bool {
	return segment.MatchString(k.Channel) && segment.MatchString(k.RootTS) && k.Channel != "." && k.RootTS != "." && !strings.Contains(k.RootTS, "..")
}
func (s *Folder) safe(name string) error {
	if !fs.ValidPath(name) {
		return ErrIntegrity
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		info, e := s.root.Lstat(strings.Join(parts[:i+1], "/"))
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrIntegrity
		}
	}
	return nil
}
func (s *Folder) mkdir(name string) error {
	for i := range strings.Split(name, "/") {
		part := strings.Join(strings.Split(name, "/")[:i+1], "/")
		created := false
		if e := s.root.Mkdir(part, 0700); e == nil {
			created = true
		} else if !os.IsExist(e) {
			return e
		}
		if created {
			if e := s.syncDir(path.Dir(part)); e != nil {
				return e
			}
		}
		if e := s.safe(part); e != nil {
			return e
		}
		info, e := s.root.Stat(part)
		if e != nil || !info.IsDir() {
			return ErrIntegrity
		}
	}
	return nil
}
func (s *Folder) syncDir(name string) error {
	f, e := s.root.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	return syncDirectory(f)
}
func (s *Folder) locked(ctx context.Context) (func(), error) {
	s.mu.Lock()
	unlock, e := lockFiles(ctx, s)
	if e != nil {
		s.mu.Unlock()
		return nil, e
	}
	return func() { unlock(); s.mu.Unlock() }, nil
}
func (s *Folder) target(k pluginapi.Key) (string, error) {
	if s.readOnly {
		return s.readerTarget(k)
	}
	if !validKey(k) {
		return "", ErrIntegrity
	}
	parent := "archive/" + k.Channel
	if e := s.mkdir(parent); e != nil {
		return "", e
	}
	target := parent + "/" + k.RootTS
	previous := parent + "/." + k.RootTS + "-previous"
	if _, e := s.root.Lstat(previous); e == nil {
		if e = s.safe(previous); e != nil {
			return "", e
		}
		if _, e = s.root.Lstat(target); e == nil {
			if e = s.safe(target); e != nil {
				return "", e
			}
			if e = s.root.RemoveAll(previous); e != nil {
				return "", e
			}
		} else if os.IsNotExist(e) {
			if e = s.root.Rename(previous, target); e != nil {
				return "", e
			}
		} else {
			return "", e
		}
		if e = s.syncDir(parent); e != nil {
			return "", e
		}
	}
	if _, e := s.root.Lstat(target); e == nil {
		if e = s.safe(target); e != nil {
			return "", e
		}
	} else if !os.IsNotExist(e) {
		return "", e
	}
	return target, nil
}
func (s *Folder) Keys(ctx context.Context) ([]pluginapi.Key, error) {
	unlock, e := s.locked(ctx)
	if e != nil {
		return nil, e
	}
	defer unlock()
	return s.keys()
}
func (s *Folder) keys() ([]pluginapi.Key, error) {
	out := []pluginapi.Key{}
	if e := s.safe("archive"); e != nil {
		return nil, e
	}
	dirs, e := fs.ReadDir(s.root.FS(), "archive")
	if e != nil {
		return nil, e
	}
	for _, ch := range dirs {
		if !ch.IsDir() || ch.Type()&os.ModeSymlink != 0 || !segment.MatchString(ch.Name()) {
			s.fail(pluginapi.Key{Channel: ch.Name()})
			continue
		}
		entries, e := fs.ReadDir(s.root.FS(), "archive/"+ch.Name())
		if e != nil {
			continue
		}
		roots := map[string]bool{}
		for _, entry := range entries {
			n := entry.Name()
			if strings.HasPrefix(n, ".") {
				if strings.HasSuffix(n, "-previous") {
					roots[strings.TrimSuffix(strings.TrimPrefix(n, "."), "-previous")] = true
				} else if !strings.Contains(n, "-next-") {
					s.fail(pluginapi.Key{Channel: ch.Name()})
				}
				continue
			}
			roots[n] = true
		}
		for ts := range roots {
			k := pluginapi.Key{Channel: ch.Name(), RootTS: ts}
			if !validKey(k) {
				s.fail(k)
				continue
			}
			target, e := s.target(k)
			if e != nil {
				s.fail(k)
				continue
			}
			info, e := s.root.Stat(target)
			if e != nil || !info.IsDir() {
				s.fail(k)
				continue
			}
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel+"/"+out[i].RootTS < out[j].Channel+"/"+out[j].RootTS })
	return out, nil
}
func (s *Folder) Read(ctx context.Context, k pluginapi.Key) (*pluginapi.Snapshot, error) {
	unlock, e := s.locked(ctx)
	if e != nil {
		return nil, e
	}
	defer unlock()
	target, e := s.target(k)
	if e != nil {
		return nil, e
	}
	v, e := s.read(ctx, k, target)
	if e != nil {
		s.fail(k)
	} else {
		delete(s.failures, k)
		if v != nil {
			e = s.deletionOverlay(k, v)
		}
	}
	return v, e
}
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := d.Decode(v); e != nil {
		return ErrIntegrity
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrIntegrity
	}
	return nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *Folder) read(ctx context.Context, k pluginapi.Key, base string) (*pluginapi.Snapshot, error) {
	if _, e := s.root.Lstat(base); os.IsNotExist(e) {
		return nil, nil
	}
	if e := s.safe(base); e != nil {
		return nil, e
	}
	docs := map[string][]byte{}
	blobs := map[string]pluginapi.Blob{}
	var total int64
	err := fs.WalkDir(s.root.FS(), base, func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return ErrIntegrity
		}
		if name == base {
			return nil
		}
		rel := strings.TrimPrefix(name, base+"/")
		if d.IsDir() {
			if rel != "files" {
				return ErrIntegrity
			}
			return nil
		}
		if rel != "manifest.json" && !objectPath.MatchString(rel) {
			return ErrIntegrity
		}
		info, e := d.Info()
		if e != nil || !info.Mode().IsRegular() {
			return ErrIntegrity
		}
		limit := MaxJSON
		if strings.HasPrefix(rel, "files/") {
			limit = MaxFile
		}
		if info.Size() > limit || info.Size() < 0 {
			return ErrIntegrity
		}
		total += info.Size()
		if total > MaxSnapshot {
			return ErrIntegrity
		}
		if strings.HasPrefix(rel, "files/") {
			blobs[rel] = pluginapi.Blob{Size: info.Size()}
			return nil
		}
		f, e := s.root.Open(name)
		if e != nil {
			return e
		}
		b, e := io.ReadAll(io.LimitReader(f, limit+1))
		f.Close()
		if e != nil || int64(len(b)) != info.Size() {
			return ErrIntegrity
		}
		docs[rel] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	v := &pluginapi.Snapshot{Documents: docs, Blobs: blobs}
	if decode(docs["manifest.json"], &v.Manifest) != nil || decode(docs["messages.json"], &v.Messages) != nil || decode(docs["users.json"], &v.RawUsers) != nil {
		return nil, ErrIntegrity
	}
	v.Users = map[string]pluginapi.Message{}
	for uid, raw := range v.RawUsers {
		if user, ok := raw.(map[string]any); ok {
			v.Users[uid] = user
		}
	}
	for rel, blob := range blobs {
		obj, ok := v.Manifest.Objects[rel]
		if !ok || obj.Size != blob.Size {
			return nil, ErrIntegrity
		}
		blob.SHA256 = obj.SHA256
		blob.Source = &pluginapi.FileSource{Root: s.path, Path: base + "/" + rel}
		v.Blobs[rel] = blob
		f, e := blob.Open()
		if e != nil {
			return nil, e
		}
		f.Close()
	}
	if e := Validate(k, *v); e != nil {
		return nil, e
	}
	return v, nil
}

// Prepare rebuilds JSON documents and manifest hashes from the in-memory data.
func Prepare(v pluginapi.Snapshot) (pluginapi.Snapshot, error) {
	docs := map[string][]byte{}
	var e error
	docs["messages.json"], e = Canonical(v.Messages)
	if e != nil {
		return v, e
	}
	docs["users.json"], e = Canonical(rawUsers(v))
	if e != nil {
		return v, e
	}
	v.Manifest.Objects = map[string]pluginapi.Object{}
	for p, b := range docs {
		v.Manifest.Objects[p] = pluginapi.Object{Size: int64(len(b)), SHA256: digest(b)}
	}
	for p, b := range v.Blobs {
		v.Manifest.Objects[p] = pluginapi.Object{Size: b.Size, SHA256: b.SHA256}
	}
	if v.Manifest.Files == nil {
		v.Manifest.Files = []pluginapi.Attachment{}
	}
	docs["manifest.json"], e = Canonical(v.Manifest)
	v.Documents = docs
	return v, e
}
func Validate(k pluginapi.Key, v pluginapi.Snapshot) error {
	m := v.Manifest
	if !validKey(k) || (m.Schema != 1 && m.Schema != 2) || m.Channel != k.Channel || m.RootTS != k.RootTS || !segment.MatchString(m.TeamID) || m.MessageCount != len(v.Messages) || len(v.Messages) == 0 || v.Users == nil || m.Files == nil {
		return ErrIntegrity
	}
	if len(m.Objects) != 2+len(v.Blobs) || len(v.Documents) != 3 {
		return ErrIntegrity
	}
	var total int64
	for p, o := range m.Objects {
		if !objectPath.MatchString(p) || o.Size < 0 || len(o.SHA256) != 64 {
			return ErrIntegrity
		}
		total += o.Size
		if strings.HasPrefix(p, "files/") {
			b, ok := v.Blobs[p]
			if !ok || o.Size > MaxFile || b.Size != o.Size || b.SHA256 != o.SHA256 {
				return ErrIntegrity
			}
		} else {
			b, ok := v.Documents[p]
			if !ok || o.Size > MaxJSON || int64(len(b)) != o.Size || digest(b) != o.SHA256 {
				return ErrIntegrity
			}
		}
	}
	total += int64(len(v.Documents["manifest.json"]))
	if total > MaxSnapshot || int64(len(v.Documents["manifest.json"])) > MaxJSON {
		return ErrIntegrity
	}
	var dm pluginapi.ArchiveManifest
	var msgs []pluginapi.Message
	var users map[string]any
	if decode(v.Documents["manifest.json"], &dm) != nil || decode(v.Documents["messages.json"], &msgs) != nil || decode(v.Documents["users.json"], &users) != nil {
		return ErrIntegrity
	}
	a, _ := Canonical(m)
	b, _ := Canonical(dm)
	if !bytes.Equal(a, b) {
		return ErrIntegrity
	}
	a, _ = Canonical(msgs)
	b, _ = Canonical(v.Messages)
	if !bytes.Equal(a, b) {
		return ErrIntegrity
	}
	a, _ = Canonical(users)
	b, _ = Canonical(rawUsers(v))
	if !bytes.Equal(a, b) {
		return ErrIntegrity
	}
	seen := map[string]bool{}
	for i, msg := range v.Messages {
		if e := validateHistory(m.Schema, msg); e != nil {
			return e
		}
		ts, ok := msg["ts"].(string)
		user, uok := msg["user"].(string)
		if !ok || !uok || !segment.MatchString(ts) || !segment.MatchString(user) || seen[ts] || i == 0 && ts != k.RootTS {
			return ErrIntegrity
		}
		seen[ts] = true
		thread, has := msg["thread_ts"]
		if has && thread != k.RootTS || i > 0 && !has {
			return ErrIntegrity
		}
		if text, has := msg["text"]; has {
			if _, ok := text.(string); !ok {
				return ErrIntegrity
			}
		}
		author := v.Users[user]
		if author == nil || author["id"] != user {
			return ErrIntegrity
		}
		if profile, has := author["profile"]; has {
			if _, ok := profile.(map[string]any); !ok {
				return ErrIntegrity
			}
		}
	}
	count := 0
	if v, ok := v.Messages[0]["reply_count"]; ok {
		n, e := strconvInt(v)
		if e != nil {
			return ErrIntegrity
		}
		count = n
	}
	if count < 0 || m.Schema == 1 && count != len(v.Messages)-1 || m.Schema == 2 && count > len(v.Messages)-1 {
		return ErrIntegrity
	}
	found := map[string]bool{}
	for _, f := range m.Files {
		o, ok := m.Objects[f.Path]
		if !ok || f.Path != "files/"+f.ID+".bin" || found[f.ID] || o.Size != f.Size || o.SHA256 != f.SHA256 {
			return ErrIntegrity
		}
		found[f.ID] = true
	}
	if len(found) != len(v.Blobs) {
		return ErrIntegrity
	}
	return nil
}
func strconvInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		i, e := n.Int64()
		return int(i), e
	case int:
		return n, nil
	case float64:
		if n == float64(int(n)) {
			return int(n), nil
		}
	}
	return 0, ErrIntegrity
}
func comparison(v pluginapi.Snapshot) ([]byte, error) {
	b, e := Canonical(v.Messages)
	if e != nil {
		return nil, e
	}
	var msgs []pluginapi.Message
	if e = decode(b, &msgs); e != nil {
		return nil, e
	}
	for _, m := range msgs {
		containers := []map[string]any{m}
		if a, ok := m["attachments"].([]any); ok {
			for _, v := range a {
				if c, ok := v.(map[string]any); ok {
					containers = append(containers, c)
				}
			}
		}
		for _, c := range containers {
			if blocks, ok := c["blocks"].([]any); ok {
				for _, v := range blocks {
					if block, ok := v.(map[string]any); ok && block["type"] == "rich_text" {
						delete(block, "block_id")
					}
				}
			}
		}
		if rs, ok := m["reactions"].([]any); ok {
			for _, v := range rs {
				r, ok := v.(map[string]any)
				if !ok {
					continue
				}
				if us, ok := r["users"].([]any); ok {
					sort.Slice(us, func(i, j int) bool { return fmt.Sprint(us[i]) < fmt.Sprint(us[j]) })
				}
			}
		}
	}
	v.Messages = msgs
	v, e = Prepare(v)
	if e != nil {
		return nil, e
	}
	return Canonical(v.Documents)
}
func (s *Folder) Save(ctx context.Context, k pluginapi.Key, v pluginapi.Snapshot) (bool, error) {
	if s.readOnly {
		return false, ErrIntegrity
	}
	unlock, e := s.locked(ctx)
	if e != nil {
		return false, e
	}
	defer unlock()
	if v.Documents != nil {
		if e = Validate(k, v); e != nil {
			return false, e
		}
	}
	v, e = Prepare(v)
	if e != nil {
		return false, e
	}
	if len(v.Messages) > 0 {
		if e = Validate(k, v); e != nil {
			return false, e
		}
	} else if v.Manifest.Schema != 2 || v.Manifest.MessageCount != 0 {
		return false, ErrIntegrity
	}
	target, e := s.target(k)
	if e != nil {
		return false, e
	}
	old, e := s.read(ctx, k, target)
	if e != nil {
		return false, e
	}
	if old == nil && len(v.Messages) == 0 {
		return false, ErrIntegrity
	}
	if old != nil {
		if e = s.deletionOverlay(k, old); e != nil {
			return false, e
		}
		if len(old.SourceDeletedAt) > 0 {
			old.Manifest.Schema = 2
		}
		v.Manifest.CleanupProof = old.Manifest.CleanupProof
		if old.CleanupStatus != "" {
			var j struct {
				Proof   string            `json:"proof"`
				Done    []string          `json:"done"`
				Deleted map[string]string `json:"source_deleted_at"`
			}
			raw, err := (&CleanupView{s}).journal(k)
			if err != nil {
				return false, err
			}
			if json.Unmarshal(raw, &j) != nil {
				return false, ErrIntegrity
			}
			v.Manifest.CleanupProof = j.Proof
			v.Messages, e = cloneMessages(v.Messages)
			if e != nil {
				return false, e
			}
			// A capture may have been fetched before DELETE. A confirmed source
			// receipt remains authoritative even if that stale capture still has TS.
			for _, msg := range v.Messages {
				ts, _ := msg["ts"].(string)
				if slices.Contains(j.Done, ts) && j.Deleted[ts] != "" {
					msg["source_deleted_at"] = j.Deleted[ts]
				}
			}
		}
		v, e = merge(*old, v, time.Now().UTC().Format(time.RFC3339Nano))
		if e != nil {
			return false, e
		}
		v, e = Prepare(v)
		if e != nil {
			return false, e
		}
		if e = Validate(k, v); e != nil {
			return false, e
		}
		a, e := comparison(*old)
		if e != nil {
			return false, e
		}
		b, e := comparison(v)
		if e != nil {
			return false, e
		}
		if bytes.Equal(a, b) {
			return false, nil
		}

	}
	parent := path.Dir(target)
	nonce := make([]byte, 12)
	if _, e = rand.Read(nonce); e != nil {
		return false, e
	}
	stage := parent + "/." + k.RootTS + "-next-" + hex.EncodeToString(nonce)
	if e = s.mkdir(stage); e != nil {
		return false, e
	}
	defer s.root.RemoveAll(stage)
	for rel, b := range v.Documents {
		if e = s.write(stage+"/"+rel, bytes.NewReader(b), int64(len(b)), digest(b)); e != nil {
			return false, e
		}
	}
	if len(v.Blobs) > 0 {
		if e = s.mkdir(stage + "/files"); e != nil {
			return false, e
		}
	}
	for rel, b := range v.Blobs {
		r, e := b.Open()
		if e != nil {
			return false, e
		}
		e = s.write(stage+"/"+rel, r, b.Size, b.SHA256)
		r.Close()
		if e != nil {
			return false, e
		}
	}
	if len(v.Blobs) > 0 {
		if e = s.syncDir(stage + "/files"); e != nil {
			return false, e
		}
	}
	if e = s.syncDir(stage); e != nil {
		return false, e
	}
	previous := parent + "/." + k.RootTS + "-previous"
	if old != nil {
		if e = s.root.Rename(target, previous); e != nil {
			return false, e
		}
		if e = s.syncDir(parent); e != nil {
			return false, e
		}
	}
	if e = s.root.Rename(stage, target); e != nil {
		return false, e
	}
	if e = s.syncDir(parent); e != nil {
		return false, e
	}
	if old != nil {
		if e = s.root.RemoveAll(previous); e != nil {
			return false, e
		}
		if e = s.syncDir(parent); e != nil {
			return false, e
		}
	}
	return true, nil
}
func (s *Folder) write(name string, r io.Reader, size int64, sum string) error {
	f, e := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	if e != nil || n != size || hex.EncodeToString(h.Sum(nil)) != sum {
		return ErrIntegrity
	}
	return f.Sync()
}

// BytesBlob creates an immutable in-memory attachment source.
func BytesBlob(b []byte) pluginapi.Blob {
	b = bytes.Clone(b)
	return pluginapi.Blob{Size: int64(len(b)), SHA256: digest(b), Data: b}
}

func rawUsers(v pluginapi.Snapshot) map[string]any {
	users := map[string]any{}
	for id, user := range v.RawUsers {
		users[id] = user
	}
	for id, user := range v.Users {
		users[id] = user
	}
	return users
}
