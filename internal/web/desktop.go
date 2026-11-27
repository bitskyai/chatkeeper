package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Desktop companion endpoints let a local launcher (tray or menu-bar app) that
// started this process show status and pause capture. They exist only in
// desktop mode and only accept the per-launch token as a bearer credential:
// that token already grants a full browser session, so no privilege is added.
// Responses carry identifiers, states and timestamps only — never credentials,
// settings, file paths or message content.

// capturingWindow marks a workspace as actively capturing after an archive.
const capturingWindow = 90 * time.Second

type desktopWorkspace struct {
	ID          string `json:"id"`
	Plugin      string `json:"plugin"`
	State       string `json:"state"`
	ErrorClass  string `json:"error_class,omitempty"`
	Realtime    bool   `json:"realtime"`
	Paused      bool   `json:"paused"`
	LastCapture string `json:"last_capture,omitempty"`
}
type desktopArchive struct {
	Workspace string `json:"workspace"`
	Channel   string `json:"channel"`
	Root      string `json:"root"`
	Path      string `json:"path"`
	At        string `json:"at"`
}
type desktopStatus struct {
	State       string             `json:"state"`
	ConfigError bool               `json:"config_error"`
	Paused      bool               `json:"paused"`
	LastCapture string             `json:"last_capture,omitempty"`
	Workspaces  []desktopWorkspace `json:"workspaces"`
	Recent      []desktopArchive   `json:"recent"`
}

func (a *App) desktopAuthorized(r *http.Request) bool {
	parts := strings.Fields(r.Header.Get("Authorization"))
	return a.opts.Mode == "desktop" && a.launchToken != "" && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && equal(parts[1], a.launchToken)
}

func (a *App) desktopAPI(w http.ResponseWriter, r *http.Request) {
	if a.opts.Mode != "desktop" {
		jsonReply(w, 404, map[string]string{"error": "not found"})
		return
	}
	if !a.desktopAuthorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		jsonReply(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	switch r.URL.Path {
	case "/api/desktop/status":
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			jsonReply(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		jsonReply(w, 200, a.desktopStatus(time.Now()))
	case "/api/desktop/pause":
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			jsonReply(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		var body struct {
			Paused *bool `json:"paused"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Paused == nil {
			jsonReply(w, 400, map[string]string{"error": "expected {\"paused\": true|false}"})
			return
		}
		if e := a.setAllPaused(*body.Paused); e != nil {
			jsonReply(w, 503, map[string]string{"error": "cannot change capture state"})
			return
		}
		jsonReply(w, 200, a.desktopStatus(time.Now()))
	default:
		jsonReply(w, 404, map[string]string{"error": "not found"})
	}
}

// setAllPaused pauses or resumes every workspace, persisting flags the same
// way the settings page does so the choice survives restarts.
func (a *App) setAllPaused(paused bool) error {
	for _, running := range a.Manager.List() {
		id := running.Entry.Workspace.ID
		if running.Paused == paused {
			continue
		}
		if e := a.Manager.Pause(id); e != nil {
			return e
		}
		_, _ = a.db.Exec("INSERT OR REPLACE INTO workspace_flags(workspace,paused) VALUES(?,?)", id, paused)
	}
	return nil
}

func (a *App) desktopStatus(now time.Time) desktopStatus {
	s := desktopStatus{Workspaces: []desktopWorkspace{}, Recent: []desktopArchive{}, ConfigError: a.Manager.Failure() != ""}
	var last time.Time
	failing, paused := false, 0
	list := a.Manager.List()
	for _, running := range list {
		h := running.Runtime.Health()
		state := h.State
		if running.Paused {
			state = "paused"
			paused++
		}
		if state == "failed" || state == "quarantined" {
			failing = true
		}
		dw := desktopWorkspace{ID: running.Entry.Workspace.ID, Plugin: running.Entry.Plugin.Manifest().ID, State: state, ErrorClass: h.ErrorClass, Realtime: h.Realtime, Paused: running.Paused}
		if !h.LastCapture.IsZero() {
			dw.LastCapture = h.LastCapture.UTC().Format(time.RFC3339)
			if h.LastCapture.After(last) {
				last = h.LastCapture
			}
		}
		s.Workspaces = append(s.Workspaces, dw)
	}
	sort.Slice(s.Workspaces, func(i, j int) bool { return s.Workspaces[i].ID < s.Workspaces[j].ID })
	rows, e := a.db.Query("SELECT workspace,channel,root,MAX(at) AS last FROM events GROUP BY workspace,channel,root ORDER BY last DESC, MAX(id) DESC LIMIT 5")
	if e == nil {
		for rows.Next() {
			var v desktopArchive
			var at int64
			if rows.Scan(&v.Workspace, &v.Channel, &v.Root, &at) != nil {
				continue
			}
			v.At = time.Unix(at, 0).UTC().Format(time.RFC3339)
			v.Path = "/view/" + v.Workspace + "/" + v.Channel + "/" + v.Root
			s.Recent = append(s.Recent, v)
			if t := time.Unix(at, 0); t.After(last) {
				last = t
			}
		}
		_ = rows.Close()
	}
	if !last.IsZero() {
		s.LastCapture = last.UTC().Format(time.RFC3339)
	}
	s.Paused = len(list) > 0 && paused == len(list)
	switch {
	case s.ConfigError || failing:
		s.State = "error"
	case len(list) == 0:
		s.State = "setup"
	case s.Paused:
		s.State = "paused"
	case !last.IsZero() && now.Sub(last) < capturingWindow:
		s.State = "capturing"
	default:
		s.State = "running"
	}
	return s
}

// safeNext accepts only local archive or dashboard paths for post-login redirects.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	if next == "/" || strings.HasPrefix(next, "/view/") || strings.HasPrefix(next, "/settings/") {
		return next
	}
	return "/"
}
