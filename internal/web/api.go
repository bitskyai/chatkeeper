package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	archiveMCP "github.com/bitskyai/chatkeeper/internal/mcp"
	"github.com/bitskyai/chatkeeper/internal/search"
)

func (a *App) newProtocol() http.Handler { return archiveMCP.HTTP(a.Manager.Search) }
func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, e error) {
	status := 503
	if errors.Is(e, search.ErrQuery) {
		status = 400
	}
	if errors.Is(e, search.ErrNotFound) {
		status = 404
	}
	jsonReply(w, status, map[string]string{"error": e.Error()})
}
func parseQuery(r *http.Request) (search.Query, error) {
	values, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return search.Query{}, search.ErrQuery
	}
	allowed := map[string]bool{"q": true, "workspace": true, "channel": true, "author": true, "from": true, "to": true, "limit": true, "after": true, "cursor": true}
	for k, v := range values {
		if !allowed[k] || len(v) != 1 || (k != "q" && v[0] == "") {
			return search.Query{}, search.ErrQuery
		}
	}
	q := search.Query{Q: values.Get("q"), Workspace: values.Get("workspace"), Channel: values.Get("channel"), Author: values.Get("author"), From: values.Get("from"), To: values.Get("to"), After: values.Get("after")}
	if v, ok := values["limit"]; ok {
		q.Limit, e = strconv.Atoi(v[0])
		if e != nil || q.Limit < 1 || q.Limit > 100 {
			return q, search.ErrQuery
		}
	}
	if v, ok := values["cursor"]; ok {
		if _, exists := values["after"]; exists {
			return q, search.ErrQuery
		}
		q.After = v[0]
	}
	return q, nil
}
func (a *App) readAPI(w http.ResponseWriter, r *http.Request) {
	a.Manager.Read(func() { a.readAPILocked(w, r) })
}
func (a *App) readAPILocked(w http.ResponseWriter, r *http.Request) {
	token := a.Manager.BearerToken()
	parts := strings.Fields(r.Header.Get("Authorization"))
	bearer := token != "" && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && equal(parts[1], token)
	id, _ := a.session(r)
	if r.URL.Path == "/mcp" {
		// HTTP MCP never inherits a browser session or an unconfigured token.
		if !bearer {
			w.Header().Set("WWW-Authenticate", "Bearer")
			jsonReply(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			scheme := "http"
			if r.TLS != nil || a.opts.SecureCookies {
				scheme = "https"
			}
			if e != nil || u.Scheme != scheme || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				jsonReply(w, 403, map[string]string{"error": "cross-origin request denied"})
				return
			}
		}
		a.protocol.ServeHTTP(w, r)
		return
	}
	if !bearer && id == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		jsonReply(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		jsonReply(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if r.URL.Path == "/api/openapi.json" {
		jsonReply(w, 200, openAPI())
		return
	}
	if r.URL.Path == "/api/search" || r.URL.Path == "/search" {
		q, e := parseQuery(r)
		if e != nil {
			apiError(w, e)
			return
		}
		p, e := a.Manager.Search.Search(r.Context(), q)
		if e != nil {
			apiError(w, e)
			return
		}
		if r.URL.Path == "/search" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = searchPage.Execute(w, struct {
				Q    string
				Page search.Page
				Next string
			}{q.Q, p, nextSearch(r, p)})
			return
		}
		jsonReply(w, 200, p)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/threads/") {
		p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/threads/"), "/")
		if len(p) != 3 {
			apiError(w, search.ErrQuery)
			return
		}
		v, e := a.Manager.Search.GetThread(r.Context(), p[0], p[1], p[2])
		if e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, 200, v)
		return
	}
	jsonReply(w, 404, map[string]string{"error": "not found"})
}
func nextSearch(r *http.Request, p search.Page) string {
	if p.NextCursor == nil {
		return ""
	}
	q := r.URL.Query()
	q.Del("cursor")
	q.Set("after", *p.NextCursor)
	return "/search?" + q.Encode()
}

var searchPage = template.Must(template.New("search").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Search · ChatKeeper</title><body><nav><a href="/">Dashboard</a> <a href="/view/">Archives</a></nav><h1>Search archives</h1><form action="/search" method="get"><label>Search <input name="q" value="{{.Q}}" required maxlength="4096"></label><button>Search</button></form>{{range .Page.Results}}<article><h2><a href="{{.Permalink}}">{{.Workspace}} / {{.Channel}} / {{.RootTS}}</a></h2>{{if .SourceDeleted}}<p>Deleted at source · archived content retained</p>{{end}}{{if .PriorVersions}}<p>Includes prior edit versions</p>{{end}}<pre>{{.Snippet}}</pre></article>{{else}}<p>No matching threads.</p>{{end}}{{if .Next}}<a href="{{.Next}}">Next page</a>{{end}}</body></html>`))
