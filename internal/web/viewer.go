// Package web renders read-only pages from verified archives. Archived text never
// becomes trusted HTML and attachments are always downloads of octet streams.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templates embed.FS
var pages = template.Must(template.ParseFS(templates, "templates/*.html"))

type Source struct {
	Store   storage.Store
	Content pluginapi.ContentHooks
	path    string
}
type Viewer struct {
	sources  map[string]Source
	token    [32]byte
	auth     bool
	mu       sync.Mutex
	mentions map[string]mentionCache
}

func New(sources map[string]Source, token string) *Viewer {
	copy := map[string]Source{}
	for k, v := range sources {
		copy[k] = v
	}
	return &Viewer{sources: copy, token: sha256.Sum256([]byte(token)), auth: token != "", mentions: map[string]mentionCache{}}
}

type row struct{ URL, Label string }
type message struct{ TS, Author, Text, SourceDeletedAt string }
type threadPage struct {
	Title, Back, Permalink string
	Messages               []message
	Files                  []row
}

func (v *Viewer) ServeHTTP(w http.ResponseWriter, r *http.Request) { v.serveHTTP(w, r, v.sources) }
func (v *Viewer) serveHTTP(w http.ResponseWriter, r *http.Request, sources map[string]Source) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/healthz" && (r.Method == "GET" || r.Method == "HEAD") {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; sandbox allow-downloads allow-forms allow-same-origin")
	if v.auth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(sum[:], v.token[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="viewer"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/view/" {
		rows := []row{}
		for id := range sources {
			rows = append(rows, row{URL: "/view/" + url.PathEscape(id) + "/", Label: id})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Label < rows[j].Label })
		v.render(w, "index", rows)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "view" {
		http.NotFound(w, r)
		return
	}
	source, ok := sources[parts[1]]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if len(parts) == 2 {
		keys, e := source.Store.Keys(ctx)
		if e != nil {
			http.Error(w, "archive unavailable", http.StatusServiceUnavailable)
			return
		}
		rows := []row{}
		for _, k := range keys {
			snapshot, e := source.Store.Read(ctx, k)
			if e != nil || snapshot == nil {
				continue
			}
			if e = core.Guard(ctx, 10*time.Second, func(c context.Context) error { return source.Content.Validate(c, *snapshot) }); e != nil {
				continue
			}
			rows = append(rows, row{URL: "/view/" + parts[1] + "/" + url.PathEscape(k.Channel) + "/" + url.PathEscape(k.RootTS), Label: k.Channel + " / " + k.RootTS})
		}
		v.render(w, "index", rows)
		return
	}
	if len(parts) != 4 && len(parts) != 6 {
		http.NotFound(w, r)
		return
	}
	k := pluginapi.Key{Channel: parts[2], RootTS: parts[3]}
	snapshot, e := source.Store.Read(ctx, k)
	if e != nil {
		http.Error(w, "archive integrity failure", http.StatusServiceUnavailable)
		return
	}
	if snapshot == nil {
		http.NotFound(w, r)
		return
	}
	if e = core.Guard(ctx, 10*time.Second, func(c context.Context) error { return source.Content.Validate(c, *snapshot) }); e != nil {
		http.Error(w, "archive content failure", http.StatusServiceUnavailable)
		return
	}
	if len(parts) == 6 {
		if parts[4] != "files" {
			http.NotFound(w, r)
			return
		}
		b, ok := snapshot.Blobs["files/"+parts[5]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		file, e := b.Open()
		if e != nil {
			http.Error(w, "archive integrity failure", http.StatusServiceUnavailable)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
		if r.Method != "HEAD" {
			_, _ = io.Copy(w, file)
		}
		return
	}
	// Newest archived metadata supplies missing mentions; this thread's own users win.
	users := v.mentionUsers(ctx, parts[1], source)
	for id, u := range snapshot.Users {
		users[id] = u
	}
	page := threadPage{Back: "/view/" + parts[1] + "/", Permalink: snapshot.Manifest.Permalink}
	for _, m := range snapshot.Messages {
		text, e := core.Call(ctx, 10*time.Second, func(c context.Context) (string, error) { return source.Content.Text(c, m, users) })
		if e != nil {
			http.Error(w, "archive content failure", http.StatusServiceUnavailable)
			return
		}
		uid, _ := m["user"].(string)
		author, e := core.Call(ctx, 10*time.Second, func(c context.Context) (string, error) { return source.Content.Author(c, users[uid]) })
		if e != nil {
			http.Error(w, "archive content failure", http.StatusServiceUnavailable)
			return
		}
		ts, _ := m["ts"].(string)
		deletedAt, _ := m["source_deleted_at"].(string)
		page.Messages = append(page.Messages, message{TS: ts, Author: author, Text: text, SourceDeletedAt: deletedAt})
		if page.Title == "" {
			page.Title = text
		}
	}
	for _, f := range snapshot.Manifest.Files {
		page.Files = append(page.Files, row{URL: r.URL.Path + "/" + f.Path, Label: f.Name})
	}
	v.render(w, "thread", page)
}
func (v *Viewer) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pages.ExecuteTemplate(w, name, data)
}
func timestampLess(a, b string) bool {
	aa := strings.SplitN(a, ".", 2)
	bb := strings.SplitN(b, ".", 2)
	if len(aa[0]) != len(bb[0]) {
		return len(aa[0]) < len(bb[0])
	}
	return a < b
}

// NewForBind is the startup boundary for network listeners. New remains useful
// for embedded handlers whose access control is supplied by their caller.
func NewForBind(sources map[string]Source, token, host string) (*Viewer, error) {
	ip := net.ParseIP(host)
	if token == "" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("public viewer bind requires bearer token")
	}
	return New(sources, token), nil
}

type mentionCache struct {
	until time.Time
	users map[string]pluginapi.Message
}

func (v *Viewer) mentionUsers(ctx context.Context, id string, source Source) map[string]pluginapi.Message {
	v.mu.Lock()
	cached := v.mentions[id]
	v.mu.Unlock()
	if time.Now().After(cached.until) {
		cached.users = map[string]pluginapi.Message{}
		keys, e := source.Store.Keys(ctx)
		if e == nil {
			sort.Slice(keys, func(i, j int) bool { return timestampLess(keys[i].RootTS, keys[j].RootTS) })
			for _, key := range keys {
				other, e := source.Store.Read(ctx, key)
				if e != nil || other == nil {
					continue
				}
				for uid, user := range other.Users {
					cached.users[uid] = user
				}
			}
			cached.until = time.Now().Add(30 * time.Second)
			v.mu.Lock()
			if prior := v.mentions[id]; prior.until.After(cached.until) {
				cached = prior
			} else {
				v.mentions[id] = cached
			}
			v.mu.Unlock()
		}
	}
	users := map[string]pluginapi.Message{}
	for uid, user := range cached.users {
		users[uid] = user
	}
	return users
}
