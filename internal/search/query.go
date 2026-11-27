package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type cursor struct {
	Signature string  `json:"query"`
	Score     float64 `json:"score"`
	Workspace string  `json:"workspace"`
	Channel   string  `json:"channel"`
	TS        string  `json:"ts"`
}

func less(a, b Hit) bool {
	if a.Workspace != b.Workspace {
		return a.Workspace < b.Workspace
	}
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	if a.Channel != b.Channel {
		return a.Channel < b.Channel
	}
	return a.RootTS < b.RootTS
}

var errRetryIndex = errors.New("retry corrupt readonly index")

func (s *Service) Search(ctx context.Context, q Query) (Page, error) {
	failed := map[string]bool{}
	for attempts := len(s.ListWorkspaces()) + 1; attempts > 0; attempts-- {
		p, e := s.searchOnce(ctx, q, failed)
		if !errors.Is(e, errRetryIndex) {
			return p, e
		}
	}
	return Page{Results: []Hit{}}, ErrUnavailable
}
func (s *Service) searchOnce(ctx context.Context, q Query, failed map[string]bool) (Page, error) {
	p := Page{Results: []Hit{}}
	if strings.TrimSpace(q.Q) == "" || !utf8.ValidString(q.Q) || utf8.RuneCountInString(q.Q) > 4096 || q.Limit < 0 || q.Limit > 100 {
		return p, ErrQuery
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	for _, id := range []string{q.Workspace, q.Channel, q.Author} {
		if id != "" && !identifier.MatchString(id) {
			return p, ErrQuery
		}
	}
	var lower, upper int64
	var e error
	if q.From != "" {
		lower, e = parseTime(q.From)
		if e != nil {
			return p, e
		}
	}
	if q.To != "" {
		upper, e = parseTime(q.To)
		if e != nil {
			return p, e
		}
	}
	if q.From != "" && q.To != "" && lower >= upper {
		return p, ErrQuery
	}
	var after cursor
	if q.After != "" {
		if len(q.After) > 2048 {
			return p, ErrQuery
		}
		b, e := base64.RawURLEncoding.DecodeString(q.After)
		if e != nil || json.Unmarshal(b, &after) != nil || after.Signature == "" || math.IsNaN(after.Score) || math.IsInf(after.Score, 0) || !identifier.MatchString(after.Workspace) || !threadID.MatchString(after.Channel) || !threadID.MatchString(after.TS) {
			return p, ErrQuery
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readOnly {
		defer func() {
			for _, w := range s.workspaces {
				w.releaseReader()
			}
		}()
	}
	revisions := map[string]string{}
	hits := []Hit{}
	for id, w := range s.workspaces {
		if q.Workspace != "" && q.Workspace != id {
			continue
		}
		if w.db == nil && !s.readOnly {
			return p, ErrUnavailable
		}
		if s.readOnly {
			var e error
			if failed[id] {
				w.releaseReader()
				e = s.fallback(ctx, id, w)
			} else {
				e = s.refreshReader(ctx, id, w)
			}
			if e != nil {
				return p, ErrUnavailable
			}
		}
		failIndex := func() error {
			if s.readOnly && !failed[id] {
				failed[id] = true
				return errRetryIndex
			}
			return ErrUnavailable
		}

		tx, e := w.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if e != nil {
			return p, failIndex()
		}
		var rev string
		e = tx.QueryRowContext(ctx, "SELECT revision FROM state WHERE id=1").Scan(&rev)
		if e != nil {
			tx.Rollback()
			return p, failIndex()
		}
		var origin string
		if tx.QueryRowContext(ctx, "SELECT source FROM origin WHERE id=1").Scan(&origin) != nil || origin != w.source.origin() {
			tx.Rollback()
			return p, failIndex()
		}

		revisions[id] = rev
		where := []string{"threads MATCH ?"}
		args := []any{q.Q}
		score, snippet := "rank", "snippet(threads,5,'[',']',' … ',40)"
		if cjkLiteral(q.Q) {
			// unicode61 treats an unspaced CJK sentence as one token. Literal
			// substring search also supports one/two-character words (trigram cannot).
			where = []string{"instr(body,?)>0"}
			score, snippet = "0.0", "substr(body,1,240)"
		}
		if q.Channel != "" {
			where = append(where, "channel=?")
			args = append(args, q.Channel)
		}
		if q.Author != "" {
			where = append(where, "EXISTS (SELECT 1 FROM json_each(authors) WHERE value=?)")
			args = append(args, q.Author)
		}
		if q.From != "" {
			where = append(where, "root_time IS NOT NULL AND CAST(root_time AS INTEGER)>=?")
			args = append(args, lower)
		}
		if q.To != "" {
			where = append(where, "root_time IS NOT NULL AND CAST(root_time AS INTEGER)<?")
			args = append(args, upper)
		}
		if q.After != "" {
			where = append(where, "(? > ? OR (? = ? AND ("+score+">? OR ("+score+"=? AND (channel,root_ts)>(?,?)))))")
			args = append(args, id, after.Workspace, id, after.Workspace, after.Score, after.Score, after.Channel, after.TS)
		}
		args = append(args, q.Limit+1)
		rows, e := tx.QueryContext(ctx, "SELECT channel,root_ts,"+score+","+snippet+",source_deleted,prior_versions FROM threads WHERE "+strings.Join(where, " AND ")+" ORDER BY "+score+",channel,root_ts LIMIT ?", args...)
		if e != nil {
			tx.Rollback()
			if isSyntax(e) {
				return p, ErrQuery
			}
			return p, failIndex()
		}
		for rows.Next() {
			h := Hit{Workspace: id}
			if e = rows.Scan(&h.Channel, &h.RootTS, &h.Score, &h.Snippet, &h.SourceDeleted, &h.PriorVersions); e != nil {
				break
			}
			h.Permalink = "/view/" + url.PathEscape(id) + "/" + url.PathEscape(h.Channel) + "/" + url.PathEscape(h.RootTS) + "/"
			hits = append(hits, h)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		tx.Rollback()
		if e != nil {
			if isSyntax(e) {
				return p, ErrQuery
			}
			return p, failIndex()
		}
	}
	if len(revisions) == 0 {
		db, e := sql.Open("sqlite", ":memory:")
		if e != nil {
			return p, ErrUnavailable
		}
		defer db.Close()
		if _, e = db.Exec(schema); e != nil {
			return p, ErrUnavailable
		}
		rows, e := db.QueryContext(ctx, "SELECT rowid FROM threads WHERE threads MATCH ?", q.Q)
		if e != nil {
			if isSyntax(e) {
				return p, ErrQuery
			}
			return p, ErrUnavailable
		}
		for rows.Next() {
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			if isSyntax(e) {
				return p, ErrQuery
			}
			return p, ErrUnavailable
		}
	}

	sigQuery := q
	sigQuery.After = ""
	sigQuery.Limit = 0
	b, _ := json.Marshal([]any{sigQuery, revisions})
	sum := sha256.Sum256(b)
	signature := hex.EncodeToString(sum[:])
	if q.After != "" && after.Signature != signature {
		return p, ErrQuery
	}
	sort.Slice(hits, func(i, j int) bool { return less(hits[i], hits[j]) })
	if len(hits) > q.Limit {
		p.Results = hits[:q.Limit]
		last := p.Results[len(p.Results)-1]
		b, _ := json.Marshal(cursor{signature, last.Score, last.Workspace, last.Channel, last.RootTS})
		v := base64.RawURLEncoding.EncodeToString(b)
		p.NextCursor = &v
	} else {
		p.Results = hits
	}
	return p, nil
}
func isSyntax(e error) bool {
	msg := strings.ToLower(e.Error())
	for _, s := range []string{"fts5: syntax error", "unterminated string", "no such column:", "unknown special query", "expected integer, got", "malformed match expression"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Operators and quoted phrases retain the normal FTS5 query grammar.
func cjkLiteral(q string) bool {
	found := false
	for _, r := range q {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			found = true
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return found
}
