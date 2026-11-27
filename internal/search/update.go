package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"reflect"
	"sort"
	"strings"
	"time"
)

func (s *Service) Rebuild(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workspaces[id]
	if w == nil || w.db == nil {
		return ErrNotFound
	}
	if w.rebuild(ctx, true) != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) Sync(ctx context.Context, id string, k pluginapi.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workspaces[id]
	if w == nil || w.db == nil {
		return ErrUnavailable
	}
	return w.sync(ctx, k)
}

func (w *workspace) sync(ctx context.Context, k pluginapi.Key) error {
	e := w.store.Inspect(ctx, k, func(v *pluginapi.Snapshot) error {
		tx, e := w.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		var origin string
		if tx.QueryRowContext(ctx, "SELECT source FROM origin WHERE id=1").Scan(&origin) != nil || origin != w.source.origin() {
			return ErrUnavailable
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM threads WHERE channel=? AND root_ts=?", k.Channel, k.RootTS); e != nil {
			return e
		}
		if v != nil {
			if e = w.insert(ctx, tx, k, v); e != nil {
				return e
			}
		}
		// The revision is derived from indexed manifest fingerprints, never a clock.
		if e = revision(ctx, tx, w.source.origin()); e != nil {
			return e
		}
		return tx.Commit()
	})
	if e != nil {
		return ErrUnavailable
	}
	return nil
}
func (w *workspace) rebuild(ctx context.Context, force bool) error {
	// Begin the transaction only inside the archive traversal. The whole catalog
	// stays locked through commit (including an empty catalog).
	return w.store.Catalog(ctx, func(visit storage.CatalogVisitor) error {
		tx, e := w.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()

		h := sha256.New()
		fmt.Fprintln(h, w.source.origin())
		e = visit(func(k pluginapi.Key, v *pluginapi.Snapshot) error {
			fmt.Fprintf(h, "%s/%s:%s\n", k.Channel, k.RootTS, fingerprint(v))
			return nil
		})
		if e != nil {
			return e
		}
		want := hex.EncodeToString(h.Sum(nil))
		var old string
		_ = tx.QueryRowContext(ctx, "SELECT revision FROM state WHERE id=1").Scan(&old)
		indexed, e := indexedRevision(ctx, tx, w.source.origin())
		if e != nil {
			return e
		}
		var source string
		_ = tx.QueryRowContext(ctx, "SELECT source FROM origin WHERE id=1").Scan(&source)
		if !force && old == want && indexed == want && source == w.source.origin() {
			return nil
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM threads"); e != nil {
			return e
		}

		if e = visit(func(k pluginapi.Key, v *pluginapi.Snapshot) error { return w.insert(ctx, tx, k, v) }); e != nil {
			return e
		}

		if _, e = tx.ExecContext(ctx, "INSERT OR REPLACE INTO state VALUES(1,?)", want); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "INSERT OR REPLACE INTO origin VALUES(1,?)", w.source.origin()); e != nil {
			return e
		}
		return tx.Commit()
	})
}
func fingerprint(v *pluginapi.Snapshot) string {
	b, _ := json.Marshal(struct {
		Manifest pluginapi.ArchiveManifest
		Messages []pluginapi.Message
	}{v.Manifest, v.Messages})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func indexedRevision(ctx context.Context, tx *sql.Tx, origin string) (string, error) {
	rows, e := tx.QueryContext(ctx, "SELECT channel,root_ts,fingerprint FROM threads ORDER BY channel,root_ts")
	if e != nil {
		return "", e
	}
	h := sha256.New()
	fmt.Fprintln(h, origin)
	for rows.Next() {
		var ch, ts, fp string
		if e := rows.Scan(&ch, &ts, &fp); e != nil {
			rows.Close()
			return "", e
		}
		fmt.Fprintf(h, "%s/%s:%s\n", ch, ts, fp)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func revision(ctx context.Context, tx *sql.Tx, origin string) error {
	r, e := indexedRevision(ctx, tx, origin)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT OR REPLACE INTO state VALUES(1,?)", r)
	return e
}
func (w *workspace) insert(ctx context.Context, tx *sql.Tx, k pluginapi.Key, v *pluginapi.Snapshot) error {
	if e := core.Guard(ctx, 10*time.Second, func(c context.Context) error { return w.source.Content.Validate(c, *v) }); e != nil {
		return e
	}
	texts := []string{}
	authors := map[string]bool{}
	deleted, prior := false, false
	add := func(m pluginapi.Message) error {
		text, e := core.Call(ctx, 10*time.Second, func(c context.Context) (string, error) { return w.source.Content.Text(c, m, v.Users) })
		if e != nil {
			return e
		}
		if w.textBudget != nil {
			*w.textBudget -= len(text)
			if *w.textBudget < 0 {
				return ErrUnavailable
			}
		}
		texts = append(texts, text)
		a, _ := m["user"].(string)
		authors[a] = true
		return nil
	}
	for _, m := range v.Messages {
		if e := add(m); e != nil {
			return e
		}
		if m["source_deleted_at"] != nil {
			deleted = true
		}
		if versions, ok := m["versions"].([]any); ok {
			for _, raw := range versions {
				version, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				content, ok := version["content"].(map[string]any)
				if !ok {
					continue
				}
				prior = true
				if e := add(pluginapi.Message(content)); e != nil {
					return e
				}
			}
		}
	}
	names := []string{}
	for a := range authors {
		names = append(names, a)
	}
	sort.Strings(names)
	a, _ := json.Marshal(names)
	rootTime := k.RootTS
	if len(v.Messages) > 0 {
		if timestamp, ok := v.Messages[0]["timestamp"].(string); ok {
			rootTime = timestamp
		}
	}
	timestamp, e := parseTime(rootTime)
	var indexedTime any = timestamp
	if e != nil {
		indexedTime = nil
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO threads VALUES(?,?,?,?,?,?,?,?,?)", k.Channel, k.RootTS, indexedTime, string(a), fingerprint(v), strings.Join(texts, "\n"), deleted, prior, v.Manifest.Permalink)
	return e
}

func (src Source) identity() string {
	if src.Identity != "" {
		return src.Identity
	}
	return reflect.TypeOf(src.Content).String()
}
func (src Source) origin() string {
	sum := sha256.Sum256([]byte(src.Path + "\x00" + src.identity()))
	return hex.EncodeToString(sum[:])
}
