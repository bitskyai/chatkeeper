package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/internal/cleanup"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type cleanupPage struct {
	CSRF, Workspace, Digest string
	Enabled                 bool
	Running                 bool
	Report                  *cleanup.Report
	Workspaces              []string
	Problems                []string
	Logs                    []string
}

var cleanupTemplate = template.Must(template.New("cleanup").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Cleanup · ChatKeeper</title>{{if .Running}}<meta http-equiv="refresh" content="2">{{end}}<link rel="stylesheet" href="/static/app.css"></head><body><header><a href="/">ChatKeeper</a><nav><a href="/settings/">Settings</a></nav></header><main><h1>Cleanup</h1><p>Cleanup is off by default. Preview checks your archive offline. Apply verifies live identity and every message before deleting from the source. Your archive stays preserved.</p>{{range .Workspaces}}<p><a href="/cleanup/?workspace={{.}}">{{.}}</a></p>{{end}}{{if .Workspace}}<h2>{{.Workspace}}</h2><p><a href="/settings/cleanup?workspace={{.Workspace}}">Configure source deletion in Settings</a></p><p>Enabled: {{.Enabled}}</p><form method="post" action="/cleanup/preview"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="workspace" value="{{.Workspace}}"><button>Preview dry-run</button></form>{{end}}{{range .Problems}}<p class="notice error">{{.}} · <a href="/settings/cleanup?workspace={{$.Workspace}}">Fix in Settings</a></p>{{end}}{{with .Report}}<h2>Report</h2><p><a href="/settings/cleanup?workspace={{$.Workspace}}">Fix in Settings</a></p>{{if .Reason}}<p>{{.Reason}}</p>{{end}}<table><thead><tr><th>Channel / thread</th><th>Message</th><th>Status</th><th>Reason</th></tr></thead><tbody>{{range .Threads}}<tr><td>{{.Channel}} / {{.RootTS}}</td><td>Thread</td><td>{{.Status}}</td><td>{{.Reason}}</td></tr>{{range .Results}}<tr><td></td><td>{{.TS}}</td><td>{{.Status}}</td><td>{{.Reason}}</td></tr>{{end}}{{end}}</tbody></table>{{end}}{{if .Digest}}<form method="post" action="/cleanup/apply"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="workspace" value="{{.Workspace}}"><input type="hidden" name="digest" value="{{.Digest}}"><label>Type workspace id to confirm deletion<input name="confirm" required autocomplete="off"></label><label>Administrator password (server mode)<input name="password" type="password" autocomplete="current-password"></label><button class="danger">Apply source deletions</button></form>{{end}}{{if .Running}}<p role="status">Cleanup running. Progress refreshes every 2 seconds; you can leave this page.</p>{{end}}<h2>Results log</h2>{{range .Logs}}<pre>{{.}}</pre>{{else}}<p>No cleanup runs yet.</p>{{end}}</main></body></html>`))

func policyDigest(w pluginapi.Workspace) string {
	b, _ := json.Marshal(w)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (a *App) cleanupPage(w http.ResponseWriter, r *http.Request, session, csrf string) {
	pg := cleanupPage{CSRF: csrf}
	cfg, e := config.Load(a.opts.Config, false)
	if _, statErr := os.Stat(a.opts.Config); os.IsNotExist(statErr) && r.Method == "GET" {
		cfg = &config.Config{}
		e = nil
	}
	if e != nil {
		http.Error(w, "Cannot load configuration", http.StatusBadRequest)
		return
	}
	id := r.FormValue("workspace")
	var entry *config.Entry
	for i := range cfg.Workspaces {
		item := &cfg.Workspaces[i]
		pg.Workspaces = append(pg.Workspaces, item.Workspace.ID)
		if item.Workspace.ID == id {
			entry = item
		}
	}
	if id != "" && entry == nil {
		http.NotFound(w, r)
		return
	}
	if entry != nil {
		pg.Workspace = id
		pg.Enabled = entry.Workspace.Cleanup.Enabled
		pg.Problems = a.cleanupWarnings(*entry)
	}
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == "POST" {
		if entry == nil {
			http.Error(w, "Workspace required", http.StatusBadRequest)
			return
		}
		ws := entry.Workspace
		switch r.URL.Path {
		case "/cleanup/enable":
			http.Redirect(w, r, "/settings/cleanup?workspace="+id, http.StatusSeeOther)
			return
		case "/cleanup/preview", "/cleanup/apply":
			s, e := storage.Open(ws.Storage)
			if e != nil {
				http.Error(w, "Archive unavailable", http.StatusConflict)
				return
			}
			defer s.Close()
			preview := cleanup.Run(r.Context(), ws, s, nil, false)
			if r.URL.Path == "/cleanup/preview" {
				pg.Report = &preview
				if ws.Cleanup.Enabled {
					b, _ := json.Marshal(preview)
					if _, e = a.db.Exec("DELETE FROM cleanup_previews WHERE expires<unixepoch() OR (session=? AND workspace=?)", session, id); e != nil {
						http.Error(w, "Cannot update preview", http.StatusInternalServerError)
						return
					}
					pg.Digest = randomToken()
					_, e = a.db.Exec("INSERT INTO cleanup_previews VALUES(?,?,?,?,?,?)", pg.Digest, session, id, policyDigest(ws), string(b), time.Now().Add(10*time.Minute).Unix())
					if e != nil {
						http.Error(w, "Cannot save preview", http.StatusInternalServerError)
						return
					}
				}
			} else {
				if r.FormValue("confirm") != id {
					http.Error(w, "Type the workspace id to confirm", http.StatusBadRequest)
					return
				}
				if a.opts.Mode == "server" {
					hash, e := os.ReadFile(filepath.Join(a.opts.Home, "state/admin.hash"))
					if e != nil || !passwordMatches(string(hash), r.FormValue("password")) {
						http.Error(w, "Administrator password required", http.StatusUnauthorized)
						return
					}
				}
				var policy, prior string
				e = a.db.QueryRow("SELECT policy,report FROM cleanup_previews WHERE digest=? AND session=? AND workspace=? AND expires>?", r.FormValue("digest"), session, id, time.Now().Unix()).Scan(&policy, &prior)
				var approved cleanup.Report
				if e != nil || policy != policyDigest(ws) || json.Unmarshal([]byte(prior), &approved) != nil || cleanup.Digest(approved) != cleanup.Digest(preview) {
					http.Error(w, "Preview changed or expired; preview again", http.StatusConflict)
					return
				}
				res, e := a.db.Exec("DELETE FROM cleanup_previews WHERE digest=?", r.FormValue("digest"))
				if e != nil {
					http.Error(w, "Cannot consume preview", http.StatusInternalServerError)
					return
				}
				n, _ := res.RowsAffected()
				if n != 1 {
					http.Error(w, "Preview already applied", http.StatusConflict)
					return
				}
				token, _, e := a.Secrets.Get(ws.Cleanup.TokenEnv)
				if e != nil {
					http.Error(w, "Cannot resolve cleanup credential", http.StatusConflict)
					return
				}
				d, e := entry.Plugin.NewCleanupDriver(a.ctx, ws, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}, HTTP: a.opts.HTTP})
				if e != nil {
					http.Error(w, "Cleanup credential unavailable or invalid", http.StatusConflict)
					return
				}
				runID, e := a.claimCleanupRun(id, "Starting cleanup")
				if e != nil {
					http.Error(w, "Cannot start cleanup", 500)
					return
				}
				if runID == 0 {
					http.Error(w, "Cleanup already running", http.StatusConflict)
					return
				}
				a.cleanupJobs.Add(1)
				go func() {
					defer a.cleanupJobs.Done()
					report := cleanup.Report{Workspace: id, Reason: "Cannot suspend capture for cleanup"}
					resume, err := a.Manager.SuspendCapture(id)
					if err != nil {
						b, _ := json.Marshal(report)
						_, _ = a.db.Exec("UPDATE cleanup_runs SET report=?,status='complete' WHERE id=?", string(b), runID)
						return
					}
					defer resume()
					store, err := storage.Open(ws.Storage)
					report.Reason = "Archive unavailable"
					if err == nil {
						defer store.Close()
						report = cleanup.ApplyBoundProgress(a.ctx, ws, store, d, approved, func(progress cleanup.Report) {
							if a.Manager.Search != nil && len(progress.Threads) > 0 {
								thread := progress.Threads[len(progress.Threads)-1]
								_ = a.Manager.Search.Sync(a.ctx, id, pluginapi.Key{Channel: thread.Channel, RootTS: thread.RootTS})
							}
							b, _ := json.Marshal(progress)
							_, _ = a.db.Exec("UPDATE cleanup_runs SET report=? WHERE id=?", string(b), runID)
						})
					}
					b, _ := json.Marshal(report)
					_, _ = a.db.Exec("UPDATE cleanup_runs SET report=?,status='complete' WHERE id=?", string(b), runID)
				}()
				http.Redirect(w, r, "/cleanup/?workspace="+id, http.StatusSeeOther)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
	}
	rows, e := a.db.Query("SELECT report,status FROM cleanup_runs ORDER BY id DESC LIMIT 20")
	if e == nil {
		for rows.Next() {
			var b, status string
			if rows.Scan(&b, &status) == nil {
				if status == "running" {
					pg.Running = true
				}
				var report cleanup.Report
				if pg.Report == nil && json.Unmarshal([]byte(b), &report) == nil && (id == "" || report.Workspace == id) {
					pg.Report = &report
				}
				pg.Logs = append(pg.Logs, b)
			}
		}
		rows.Close()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = cleanupTemplate.Execute(w, pg)
}

// claimCleanupRun atomically excludes manual and scheduled runs for a workspace.
// A skipped run creates no row, preserving the scheduler's last-run time.
func (a *App) claimCleanupRun(id, reason string) (int64, error) {
	starting, _ := json.Marshal(cleanup.Report{Workspace: id, Reason: reason})
	result, err := a.db.Exec("INSERT INTO cleanup_runs(workspace,at,report,status) SELECT ?,unixepoch(),?,'running' WHERE NOT EXISTS (SELECT 1 FROM cleanup_runs WHERE workspace=? AND status='running')", id, string(starting), id)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return 0, err
	}
	return result.LastInsertId()
}
