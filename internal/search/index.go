package search

import (
	"database/sql"
	_ "embed"
	"errors"
	sqlite "modernc.org/sqlite"
	"os"
	"path/filepath"
)

//go:embed migrations/001-index.sql
var schema string
var errIndexSchema = errors.New("index schema mismatch")
var errIndexCorrupt = errors.New("corrupt index")

func openIndex(path string) (*sql.DB, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	open := func() (*sql.DB, error) {
		db, e := sql.Open("sqlite", path)
		if e != nil {
			return nil, e
		}
		db.SetMaxOpenConns(1)
		fail := func(e error) (*sql.DB, error) { db.Close(); return nil, e }
		if _, e = db.Exec("PRAGMA busy_timeout=30000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON"); e != nil {
			return fail(e)
		}
		var check string
		if e = db.QueryRow("PRAGMA quick_check").Scan(&check); e != nil {
			return fail(e)
		}
		if check != "ok" {
			return fail(errIndexCorrupt)
		}
		var version int
		if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
			return fail(e)
		}
		if version != 0 && version != 1 {
			return fail(errIndexSchema)
		}
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		if _, e = tx.Exec(schema); e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
		rows, e := db.Query("SELECT channel,root_ts,root_time,authors,fingerprint,body,source_deleted,prior_versions,permalink FROM threads LIMIT 0")
		if e != nil {
			return fail(errIndexSchema)
		}
		rows.Close()
		var revision string
		if e = db.QueryRow("SELECT COALESCE(MAX(revision),'') FROM state").Scan(&revision); e != nil {
			return fail(errIndexSchema)
		}
		if _, e = db.Exec("INSERT INTO threads(threads) VALUES('integrity-check')"); e != nil {
			return fail(e)
		}
		if e = os.Chmod(path, 0600); e != nil {
			return fail(e)
		}
		return db, nil
	}
	db, e := open()
	if e == nil {
		return db, nil
	}
	var sqliteError *sqlite.Error
	corrupt := errors.As(e, &sqliteError) && (sqliteError.Code()&255 == 11 || sqliteError.Code()&255 == 26)
	if !corrupt && !errors.Is(e, errIndexCorrupt) && !errors.Is(e, errIndexSchema) {
		return nil, e
	}

	// Only derived files are discarded. Failure to remove is surfaced, never an
	// excuse to touch archives or application state.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return open()
}
