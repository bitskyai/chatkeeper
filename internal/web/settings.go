package web

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (a *App) dashboard(w http.ResponseWriter, r *http.Request, csrf string, settings bool) {
	kind := "dashboard"
	title := "Your threads, kept safe"
	if settings {
		kind = "settings"
		title = "Settings"
	}
	pg := page{Title: title, Kind: kind, CSRF: csrf, Error: a.Manager.Failure(), Managed: !config.Writable(a.opts.Config), Rows: a.archiveRows(""), ConfigPath: a.opts.Config}
	for _, running := range a.Manager.List() {
		health := running.Runtime.Health()
		transport := transportLabel(running)
		state := health.State
		if running.Paused {
			state = "paused"
		}
		last := "Waiting for first archive"
		if !health.LastCapture.IsZero() {
			last = health.LastCapture.UTC().Format(time.RFC3339)
		}
		errText := health.ErrorClass
		if health.ErrorClass != "" && health.LastError != "" {
			errText += ": " + health.LastError
		}
		wr := workspaceRow{ID: running.Entry.Workspace.ID, State: state, Error: errText, Transport: transport, LastCapture: last, Paused: running.Paused, Activity: running.Runtime.Activity()}
		if len(wr.Activity) > 10 {
			wr.Activity = wr.Activity[:10]
		}
		a.reportMu.Lock()
		wr.Report = a.reports[wr.ID]
		a.reportMu.Unlock()
		wr.Fields = schemaFields(running.Entry.Plugin.CredentialSchema())
		var raw map[string]any
		_ = json.Unmarshal(running.Entry.Workspace.Settings, &raw)
		for i, f := range wr.Fields {
			name := credentialReference(running.Entry.Plugin, raw, f.Name)
			value, source, _ := a.Secrets.Get(name)
			if value != "" {
				wr.Fields[i].Description = "Set · " + source
			} else {
				wr.Fields[i].Description = "Not set"
			}
			wr.Fields[i].Required = false
		}
		pg.Workspaces = append(pg.Workspaces, wr)
		if pg.Managed {
			b, _ := json.MarshalIndent(raw, "", "  ")
			pg.YAML += string(b) + "\n"
		}
	}
	for _, p := range pluginapi.Plugins() {
		pg.Cards = append(pg.Cards, card{ID: p.Manifest().ID, Name: p.Manifest().Name, Ready: len(p.SetupFlow()) > 0})
	}
	sort.Slice(pg.Workspaces, func(i, j int) bool { return pg.Workspaces[i].ID < pg.Workspaces[j].ID })
	a.render(w, pg)
}
func (a *App) settings(w http.ResponseWriter, r *http.Request, csrf string) {
	if r.Method == "GET" {
		a.dashboard(w, r, csrf, true)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/settings/password":
		a.authMu.Lock()
		defer a.authMu.Unlock()
		if e := a.savePassword(r.FormValue("password")); e != nil {
			http.Error(w, e.Error(), http.StatusBadRequest)
			return
		}
		_, _ = a.db.Exec("DELETE FROM sessions")
		a.mu.Lock()
		a.wizards = map[string]*wizard{}
		a.mu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case "/settings/check":
		id := r.FormValue("workspace")
		for _, running := range a.Manager.List() {
			if running.Entry.Workspace.ID == id {
				ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
				report := running.Runtime.CheckNow(ctx)
				cancel()
				a.reportMu.Lock()
				a.reports[id] = &report
				a.reportMu.Unlock()
				http.Redirect(w, r, "/#workspace-"+id, http.StatusSeeOther)
				return
			}
		}
		http.NotFound(w, r)
		return
	case "/settings/remove", "/settings/pause", "/settings/credentials":
		id := r.FormValue("workspace")
		var found bool
		for _, running := range a.Manager.List() {
			if running.Entry.Workspace.ID != id {
				continue
			}
			found = true
			if !config.Writable(a.opts.Config) {
				http.Error(w, "Configuration is managed; apply changes in YAML", http.StatusConflict)
				return
			}
			switch r.URL.Path {
			case "/settings/remove":
				if config.Update(a.opts.Config, id, nil) != nil {
					http.Error(w, "Cannot update configuration", http.StatusBadRequest)
					return
				}
				_, _ = a.db.Exec("DELETE FROM workspace_flags WHERE workspace=?", id)
				a.Manager.SetPaused(id, false)
			case "/settings/pause":
				if a.Manager.Pause(id) != nil {
					http.Error(w, "Cannot pause workspace", http.StatusBadRequest)
					return
				}
				for _, current := range a.Manager.List() {
					if current.Entry.Workspace.ID == id {
						_, _ = a.db.Exec("INSERT OR REPLACE INTO workspace_flags(workspace,paused) VALUES(?,?)", id, current.Paused)
					}
				}
			case "/settings/credentials":
				var raw map[string]any
				_ = json.Unmarshal(running.Entry.Workspace.Settings, &raw)
				for _, f := range schemaFields(running.Entry.Plugin.CredentialSchema()) {
					if v := r.FormValue(f.Name); v != "" {
						name := credentialReference(running.Entry.Plugin, raw, f.Name)
						if name == "" || a.Secrets.Set(name, v) != nil {
							http.Error(w, "Credential rotation failed. Environment credentials must be changed on the host.", http.StatusBadRequest)
							return
						}
					}
				}
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		if a.Manager.Reload() != nil {
			http.Error(w, "Configuration reload failed; previous configuration retained", http.StatusBadRequest)
			return
		}
	case "/settings/reload":
		if a.Manager.Reload() != nil {
			http.Error(w, "Invalid configuration; running configuration retained", http.StatusBadRequest)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/settings/", http.StatusSeeOther)
}

// DataRoot is the server-side boundary for browser-selected archive locations.
func (a *App) DataRoot() string { return filepath.Join(a.opts.Home, "data") }

// ConfiguredMode resolves environment defaults without putting deployment logic in templates.
func ConfiguredMode(explicit, host string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("CHATKEEPER_MODE"); v != "" {
		return v
	}
	if config.Loopback(strings.TrimSpace(host)) {
		return "desktop"
	}
	return "server"
}

func credentialReference(p pluginapi.Plugin, raw map[string]any, key string) string {
	if v, ok := raw[key+"_env"].(string); ok {
		return v
	}
	var cs struct {
		Properties map[string]struct {
			Env string `json:"x-env-default"`
		}
	}
	_ = json.Unmarshal(p.CredentialSchema(), &cs)
	return cs.Properties[key].Env
}
