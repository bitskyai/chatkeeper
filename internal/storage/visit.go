package storage

import (
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Catalog keeps the archive lock through the consumer's final commit, even for
// an empty archive. The provided visitor must only be used during consume.
type CatalogVisitor func(func(pluginapi.Key, *pluginapi.Snapshot) error) error

func (s *Folder) Catalog(ctx context.Context, consume func(CatalogVisitor) error) error {
	unlock, e := s.locked(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	return consume(func(yield func(pluginapi.Key, *pluginapi.Snapshot) error) error {
		keys, e := s.keys()
		if e != nil {
			return e
		}
		for _, k := range keys {
			if e := ctx.Err(); e != nil {
				return e
			}
			target, e := s.target(k)
			if e != nil {
				s.fail(k)
				continue
			}
			v, e := s.read(ctx, k, target)
			if e != nil {
				s.fail(k)
				continue
			}
			if v != nil {
				if e := yield(k, v); e != nil {
					return e
				}
			}
		}
		return nil
	})
}

// Inspect looks up an existing key without creating missing channel directories,
// holding the archive lock through consume for race-free index publication.
func (s *Folder) Inspect(ctx context.Context, k pluginapi.Key, consume func(*pluginapi.Snapshot) error) error {
	unlock, e := s.locked(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	if !validKey(k) {
		return ErrIntegrity
	}

	if e := s.safe("archive/" + k.Channel); e != nil {
		if os.IsNotExist(e) {
			return consume(nil)
		}
		return e
	}
	target, e := s.target(k)
	if e != nil {
		return e
	}
	v, e := s.read(ctx, k, target)
	if e != nil {
		return e
	}
	return consume(v)
}

// VisitUnlinked is an optimistic, read-only traversal for a disposable search
// fallback. It never takes an archive lock. Concurrent publication may cause a
// verified thread to be omitted, but cannot delay a capture or expose unverified
// data. Unlike Catalog this is not a consistent archive snapshot.
func (s *Folder) VisitUnlinked(ctx context.Context, yield func(pluginapi.Key, *pluginapi.Snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readOnly {
		return ErrIntegrity
	}
	keys, e := s.fallbackKeys(ctx)
	if e != nil {
		return e
	}
	if len(keys) > 10000 {
		return ErrIntegrity
	}
	for _, k := range keys {
		if e := ctx.Err(); e != nil {
			return e
		}
		target, e := s.readerTarget(k)
		if e != nil {
			continue
		}
		v, e := s.read(ctx, k, target)
		if e != nil || v == nil {
			continue
		}
		if e := yield(k, v); e != nil {
			return e
		}
	}
	return nil
}

// Enumerate incrementally: even a directory full of staging/invalid names must
// not allocate an unbounded slice or evade the traversal budget.
func (s *Folder) fallbackKeys(ctx context.Context) ([]pluginapi.Key, error) {
	seen := map[pluginapi.Key]bool{}
	keys := []pluginapi.Key{}
	remaining := 30000
	entries := func(path string, visit func(fs.DirEntry) error) error {
		f, e := s.root.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		for {
			if e := ctx.Err(); e != nil {
				return e
			}
			batch, e := f.ReadDir(128)
			for _, entry := range batch {
				remaining--
				if remaining < 0 {
					return ErrIntegrity
				}
				if e := visit(entry); e != nil {
					return e
				}
			}
			if e == io.EOF {
				return nil
			}
			if e != nil {
				return e
			}
		}
	}
	e := entries("archive", func(ch fs.DirEntry) error {
		if !ch.IsDir() || ch.Type()&os.ModeSymlink != 0 || !segment.MatchString(ch.Name()) {
			return nil
		}
		return entries("archive/"+ch.Name(), func(entry fs.DirEntry) error {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			ts := entry.Name()
			if strings.HasPrefix(ts, ".") {
				if !strings.HasSuffix(ts, "-previous") {
					return nil
				}
				ts = strings.TrimSuffix(strings.TrimPrefix(ts, "."), "-previous")
			}
			k := pluginapi.Key{Channel: ch.Name(), RootTS: ts}
			if !validKey(k) || seen[k] {
				return nil
			}
			seen[k] = true
			keys = append(keys, k)
			if len(keys) > 10000 {
				return ErrIntegrity
			}
			return nil
		})
	})
	return keys, e
}
