package web

import (
	"database/sql"
	"embed"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sort"
)

//go:embed migrations/*.sql
var migrations embed.FS

func openState(home string) (*sql.DB, error) {
	dir := filepath.Join(home, "state")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	if e := config.PrivatePath(dir); e != nil {
		return nil, e
	}
	p := filepath.Join(dir, "app.db")
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	_ = f.Close()
	if e = config.PrivatePath(p); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", p)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*sql.DB, error) { _ = db.Close(); return nil, e }
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, e = db.Exec(stmt); e != nil {
			return fail(e)
		}
	}
	var version int
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		return fail(e)
	}
	names, e := migrations.ReadDir("migrations")
	if e != nil {
		return fail(e)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Name() < names[j].Name() })
	if version > len(names) {
		return fail(fmt.Errorf("unsupported application database version"))
	}
	for i, n := range names {
		if i < version {
			continue
		}
		b, e := migrations.ReadFile("migrations/" + n.Name())
		if e != nil {
			return fail(e)
		}
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		if _, e = tx.Exec(string(b)); e == nil {
			_, e = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1))
		}
		if e != nil {
			_ = tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	}
	_, e = db.Exec("DELETE FROM sessions WHERE expires < unixepoch(); DELETE FROM setup_sessions WHERE expires < unixepoch()")
	if e != nil {
		return fail(e)
	}
	return db, nil
}
