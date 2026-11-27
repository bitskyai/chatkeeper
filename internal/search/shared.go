package search

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// publicationLock serializes canonical file replacement with short-lived peers.
// Peers never retain a database handle after releasing this lock.
func publicationLock(ctx context.Context, home string) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		unlock, e := writerLock(filepath.Join(home, "state", "index", ".publication.lock"))
		if e == nil {
			return unlock, nil
		}
		if os.IsNotExist(e) || os.IsPermission(e) {
			return nil, ErrUnavailable
		}
		select {
		case <-ctx.Done():
			return nil, ErrUnavailable
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// SyncShared lets a standalone backfill update an index owned by a running
// server. It cannot migrate/repair or replace files, and checks source identity
// under the publication lock before updating the latest verified snapshot.
func SyncShared(ctx context.Context, home, id string, source Source, key pluginapi.Key) error {
	if !identifier.MatchString(id) {
		return ErrQuery
	}
	unlock, e := publicationLock(ctx, home)
	if e != nil {
		return e
	}
	defer unlock()
	path, e := filepath.Abs(filepath.Join(home, "state", "index", id+".db"))
	if e != nil {
		return ErrUnavailable
	}

	db, e := sql.Open("sqlite", sharedDSN(path))
	if e != nil {
		return ErrUnavailable
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	st, e := storage.OpenReader(source.Path)
	if e != nil {
		return ErrUnavailable
	}
	defer st.Close()
	return (&workspace{source: source, store: st, db: db}).sync(ctx, key)
}

func sharedDSN(path string) string {
	path = filepath.ToSlash(path)
	if len(path) > 1 && path[1] == ':' {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(30000)"}}
	u.RawQuery = q.Encode()
	return u.String()
}
