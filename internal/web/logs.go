package web

import (
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/logging"
	"html/template"
	"net/http"
	"strings"
)

var logsTemplate = template.Must(template.New("logs").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="5"><title>Logs · ChatKeeper</title><link rel="stylesheet" href="/static/app.css"></head><body><header><a href="/">ChatKeeper</a></header><main><h1>Logs</h1><p>Refreshes every five seconds. Shows the latest 200 matching lines.</p><form method="get"><label>Filter <input name="filter" value="{{.Filter}}" placeholder="error, archive, method…"></label><button>Filter</button></form><p><a href="?download=1&amp;filter={{.Filter}}">Download logs</a></p><pre>{{.Text}}</pre></main></body></html>`))

// logsPage is reachable only through the administrator session gate.
func (a *App) logsPage(w http.ResponseWriter, r *http.Request, csrf string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	filter := r.URL.Query().Get("filter")
	if len(filter) > 200 {
		http.Error(w, "Filter is too long", 400)
		return
	}
	limit := 200
	if r.URL.Query().Get("download") == "1" {
		limit = 100000
	}
	lines, e := logging.Tail(a.opts.Home, filter, limit)
	if e != nil {
		http.Error(w, "Cannot read logs", 500)
		return
	}
	// The writer already redacts credentials. Reading never emits raw HTML.
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="chatkeeper.log"`)
		fmt.Fprintln(w, strings.Join(lines, "\n"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = logsTemplate.Execute(w, struct{ Filter, Text string }{filter, strings.Join(lines, "\n")})
}
