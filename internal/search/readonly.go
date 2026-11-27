package search

import (
	"context"
	"database/sql"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Reopen for every request: a server may atomically replace the index. Never
// use immutable with a live WAL: that would silently omit committed captures.
func (s *Service) refreshReader(ctx context.Context, id string, w *workspace) error {
	path, e := filepath.Abs(filepath.Join(s.home, "state", "index", id+".db"))
	if e != nil {
		return e
	}
	if s.home != "" {
		w.releaseReader()
		db, unlock, e := readIndex(ctx, path, w.source.origin())
		if e == nil {
			if w.db != nil {
				w.db.Close()
			}
			w.db = db
			w.readerUnlock = unlock
			return nil
		}
	}
	return s.fallback(ctx, id, w)
}
func (s *Service) fallback(ctx context.Context, id string, w *workspace) error {
	if !w.fallbackLogged {
		log.Printf("search: index unavailable for workspace %s; using bounded private fallback", id)
		w.fallbackLogged = true
	}
	db, e := memoryIndex()
	if e != nil {
		return e
	}
	if _, e := db.Exec("PRAGMA max_page_count=16384"); e != nil {
		db.Close()
		return e
	}
	budget := 32 << 20
	w.textBudget = &budget
	defer func() { w.textBudget = nil }()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		db.Close()
		return e
	}
	defer tx.Rollback()
	previous := w.db
	w.db = db
	e = w.store.VisitUnlinked(ctx, func(k pluginapi.Key, v *pluginapi.Snapshot) error {
		return w.insert(ctx, tx, k, v)
	})
	if e == nil {
		_, e = tx.ExecContext(ctx, "INSERT INTO origin VALUES(1,?)", w.source.origin())
	}
	if e == nil {
		e = revision(ctx, tx, w.source.origin())
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		w.db = previous
		db.Close()
		return e
	}
	if previous != nil {
		previous.Close()
	}
	return nil
}

func readIndex(ctx context.Context, path, origin string) (db *sql.DB, unlock func(), err error) {
	unlock = nil
	defer func() {
		if err != nil && unlock != nil {
			unlock()
		}
	}()
	if _, e := os.Stat(path); e != nil {
		return nil, unlock, e
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if len(u.Path) > 1 && u.Path[1] == ':' {
		u.Path = "/" + u.Path
	}
	q := url.Values{"mode": {"ro"}, "readonly_shm": {"1"}, "_pragma": {"query_only(1)", "busy_timeout(50)"}}
	// Immutable is safe only with both locks held: the writer lease excludes
	// a server (including Sync/Rebuild), publication excludes standalone backfill.
	// Neither lock touches an archive, creates a file, or waits for a writer.
	coldUnlock, coldErr := coldReaderLock(filepath.Dir(path))
	if coldErr == nil {
		unlock = coldUnlock
		if info, e := os.Stat(path + "-wal"); e == nil && info.Size() > 0 {
			// A crashed process left WAL; retain SQLite WAL semantics.
			if runtime.GOOS == "windows" {
				return nil, unlock, ErrUnavailable
			}
			if _, e := os.Stat(path + "-shm"); e != nil {
				return nil, unlock, e
			}
		} else if e != nil && !os.IsNotExist(e) {
			return nil, unlock, e
		} else {
			q.Set("immutable", "1")
		}
	} else {
		// readonly_shm is a Unix VFS option. Windows live-WAL reads use
		// the bounded private fallback rather than risk sidecar writes.
		if runtime.GOOS == "windows" {
			return nil, unlock, ErrUnavailable
		}
		// Live WAL readers use SQLite's regular read-only WAL protocol.
		if _, e := os.Stat(path + "-wal"); e != nil {
			return nil, unlock, e
		}
		if _, e := os.Stat(path + "-shm"); e != nil {
			return nil, unlock, e
		}
	}
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, unlock, e
	}
	db.SetMaxOpenConns(1)
	var got string
	e = db.QueryRowContext(ctx, "SELECT source FROM origin WHERE id=1").Scan(&got)
	if e == nil && got != origin {
		e = ErrUnavailable
	}
	if e == nil {
		var revision string
		e = db.QueryRowContext(ctx, "SELECT revision FROM state WHERE id=1").Scan(&revision)
	}
	if e == nil {
		rows, err := db.QueryContext(ctx, "SELECT channel,root_ts,rank FROM threads WHERE threads MATCH 'integrityprobe' LIMIT 0")
		e = err
		if rows != nil {
			rows.Close()
		}
	}
	if e != nil {
		db.Close()
		return nil, unlock, e
	}
	return db, unlock, nil
}

func coldReaderLock(dir string) (func(), error) {
	writer, e := readerLock(filepath.Join(dir, ".writer.lock"))
	if e != nil {
		return nil, e
	}
	publication, e := readerLock(filepath.Join(dir, ".publication.lock"))
	if e != nil {
		writer()
		return nil, e
	}
	// A crashed writer can leave uncheckpointed WAL. Immutable must never ignore it.
	return func() { publication(); writer() }, nil
}

func (w *workspace) releaseReader() {
	if w.readerUnlock != nil {
		if w.db != nil {
			w.db.Close()
			w.db = nil
		}
		w.readerUnlock()
		w.readerUnlock = nil
	}
}
