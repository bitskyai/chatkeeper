package search

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"

	"github.com/bitskyai/chatkeeper/internal/storage"
)

// NewReadOnly reads existing indexes without taking archive locks.
// The optional home supports callers that only need a bounded private fallback.
func NewReadOnly(home ...string) *Service {
	h := ""
	if len(home) > 0 {
		h = home[0]
	}
	s := New(h)
	s.readOnly = true
	return s
}

// Configure prepares every changed source privately before replacing the running
// configuration. Failed validation cannot leak data from a rejected archive.
func (s *Service) Configure(ctx context.Context, sources map[string]Source) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.home, "state", "index")
	if !s.readOnly && s.unlock == nil {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return ErrUnavailable
		}
		unlock, e := writerLock(filepath.Join(dir, ".writer.lock"))
		if e != nil {
			return ErrLocked
		}
		s.unlock = unlock
	}
	if !s.readOnly {
		unlock, e := publicationLock(ctx, s.home)
		if e != nil {
			return ErrUnavailable
		}
		defer unlock()
	}
	next := map[string]*workspace{}
	defer func() {
		if s.readOnly {
			for _, w := range next {
				w.releaseReader()
			}
		}
	}()
	staged := map[string]string{}
	ok := false
	defer func() {
		if !ok {
			for id, w := range next {
				if s.workspaces[id] != w {
					if w.db != nil {
						w.db.Close()
					}
					w.store.Close()
				}
			}
		}
		for _, path := range staged {
			removeIndex(path)
		}
	}()
	names := []string{}
	for id := range sources {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		src := sources[id]
		if !identifier.MatchString(id) || src.Content == nil {
			return ErrUnavailable
		}
		if old := s.workspaces[id]; old != nil && old.db != nil && old.source.Path == src.Path && old.source.identity() == src.identity() && !s.readOnly {
			next[id] = old
			continue
		}
		st, e := storage.OpenReader(src.Path)
		if e != nil {
			return ErrUnavailable
		}
		var db *sql.DB
		if s.readOnly {
			db, e = memoryIndex()
		} else {
			path := filepath.Join(dir, id+".db")
			if len(s.workspaces) > 0 {
				f, err := os.CreateTemp(dir, "."+id+"-candidate-*.db")
				if err != nil {
					st.Close()
					return ErrUnavailable
				}
				path = f.Name()
				f.Close()
				staged[id] = path
			}
			db, e = openIndex(path)
		}
		if e != nil {
			st.Close()
			return ErrUnavailable
		}
		w := &workspace{source: src, store: st, db: db}
		next[id] = w
		if s.readOnly {
			if e := s.refreshReader(ctx, id, w); e != nil {
				return ErrUnavailable
			}
		} else if e := w.rebuild(ctx, false); e != nil {
			return ErrUnavailable
		}
	}
	if len(staged) > 0 {
		if e := s.publish(dir, next, staged); e != nil {
			return ErrUnavailable
		}
	}
	for id, w := range s.workspaces {
		if next[id] != w {
			if w.db != nil {
				w.db.Close()
			}
			w.store.Close()
		}
	}
	for id, w := range next {
		w.source = sources[id]
	}
	s.workspaces = next
	ok = true
	return nil
}
func memoryIndex() (*sql.DB, error) {
	db, e := sql.Open("sqlite", ":memory:")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if _, e := db.Exec(schema); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func removeIndex(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

// publish is protected by the service and process writer locks. Old files are
// retained until every replacement opens successfully, allowing rollback on I/O
// errors. Readers of this service cannot observe the intermediate filenames.
func (s *Service) publish(dir string, next map[string]*workspace, staged map[string]string) (err error) {
	type change struct {
		id, path, backup             string
		hadOld, published, oldClosed bool
	}
	changes := []*change{}
	names := []string{}
	for id := range staged {
		names = append(names, id)
	}
	sort.Strings(names)
	defer func() {
		if err != nil {
			for i := len(changes) - 1; i >= 0; i-- {
				c := changes[i]
				if next[c.id].db != nil {
					next[c.id].db.Close()
				}
				if c.published {
					removeIndex(c.path)
				}
				if c.hadOld {
					_ = os.Rename(c.backup, c.path)
				}
				if old := s.workspaces[c.id]; old != nil && c.oldClosed {
					old.db, _ = openIndex(c.path)
				}
			}
		} else {
			for _, c := range changes {
				removeIndex(c.backup)
			}
		}
	}()
	for _, id := range names {
		w := next[id]
		if _, err = w.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return err
		}
		if err = w.db.Close(); err != nil {
			return err
		}
		path := filepath.Join(dir, id+".db")
		backup := staged[id] + "-previous"
		c := &change{id: id, path: path, backup: backup}
		changes = append(changes, c)
		if old := s.workspaces[id]; old != nil && old.db != nil {
			if _, err = old.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				return err
			}
			c.oldClosed = true
			if err = old.db.Close(); err != nil {
				return err
			}
		}
		if _, e := os.Stat(path); e == nil {
			if err = os.Rename(path, backup); err != nil {
				return err
			}
			c.hadOld = true
		} else if !os.IsNotExist(e) {
			return e
		}
		if err = os.Rename(staged[id], path); err != nil {
			return err
		}
		c.published = true
		w.db, err = openIndex(path)
		if err != nil {
			return err
		}
	}
	return nil
}
