package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/app"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"html/template"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var assets embed.FS
var adminPages = template.Must(template.ParseFS(templates, "templates/admin.html"))
var workspacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Options struct {
	Home, Config, Mode, Host, SetupCode, LaunchToken, AdminPassword string
	AllowedHosts                                                    []string
	HTTP                                                            *http.Client
	SecureCookies                                                   bool
	Dev                                                             bool
}
type App struct {
	viewerCache      map[string]*Viewer
	viewerGeneration int
	opts             Options
	db               *sql.DB
	Manager          *app.Manager
	Secrets          *secrets.Store
	ctx              context.Context
	cancel           context.CancelFunc
	maintenanceDone  chan struct{}
	mu, authMu       sync.Mutex
	viewerMu         sync.Mutex
	attempts         map[string]attempts
	launchToken      string
	existingConfig   bool
	protocol         http.Handler
	wizards          map[string]*wizard
}
type wizard struct {
	mu          sync.Mutex
	Platform    string
	Step        int
	Data        pluginapi.SetupState
	Credentials pluginapi.Credentials
	References  map[string]string
	Workspace   string
	Messages    []string
	Started     time.Time
}
type field struct {
	Name, Title, Type, Value, Description, Widget string
	Provided                                      bool
	Required, Multiple, Checked                   bool
	Pattern, Minimum, Maximum, Step               string
	Options                                       []pluginapi.Choice
	Examples                                      []string
}
type card struct {
	ID, Name string
	Ready    bool
}
type workspaceRow struct {
	ID, State, Error, Transport, LastCapture string
	Paused                                   bool
	Fields                                   []field
}
type page struct {
	Title, Kind, Body, CSRF, Error, Storage, Workspace, Reaction string
	Step                                                         int
	Messages                                                     []string
	Links                                                        []pluginapi.Link
	Fields                                                       []field
	Cards                                                        []card
	Workspaces                                                   []workspaceRow
	Rows                                                         []row
	Managed                                                      bool
	YAML                                                         string
	ConfigPath                                                   string
}

func NewApp(o Options) (*App, error) {
	if o.Mode == "" {
		if config.Loopback(o.Host) {
			o.Mode = "desktop"
		} else {
			o.Mode = "server"
		}
	}
	if o.Mode != "desktop" && o.Mode != "server" {
		return nil, errors.New("invalid mode")
	}
	if o.Mode == "desktop" && !config.Loopback(o.Host) {
		return nil, errors.New("desktop requires a literal loopback bind")
	}
	if o.Config == "" {
		o.Config = filepath.Join(o.Home, "config.yaml")
	}
	if e := os.MkdirAll(o.Home, 0700); e != nil {
		return nil, e
	}
	if e := config.PrivatePath(o.Home); e != nil {
		return nil, e
	}
	if len(o.AllowedHosts) == 0 {
		o.AllowedHosts = []string{o.Host}
		if config.Loopback(o.Host) {
			o.AllowedHosts = append(o.AllowedHosts, "localhost")
		}
	}
	db, e := openState(o.Home)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{opts: o, db: db, ctx: ctx, cancel: cancel, launchToken: o.LaunchToken, attempts: map[string]attempts{}, wizards: map[string]*wizard{}}
	if o.Mode == "desktop" {
		_, _ = db.Exec("DELETE FROM sessions; DELETE FROM setup_sessions")
	}
	a.Secrets = &secrets.Store{Home: o.Home, Desktop: o.Mode == "desktop"}
	a.Manager = app.New(ctx, o.Config, a.Secrets, o.HTTP)
	a.Manager.Search = search.New(o.Home)
	a.protocol = a.newProtocol()
	a.Manager.OnCapture = func(wid string, k pluginapi.Key) {
		_, _ = a.db.Exec("INSERT OR IGNORE INTO events(workspace,channel,root,at) VALUES(?,?,?,?)", wid, k.Channel, k.RootTS, time.Now().Unix())
	}
	if cfg, e := config.Load(o.Config, false); e == nil {
		a.existingConfig = len(cfg.Workspaces) > 0
	} else if _, err := os.Stat(o.Config); !os.IsNotExist(err) {
		a.existingConfig = true
	}
	if o.AdminPassword != "" {
		if e = a.savePassword(o.AdminPassword); e != nil {
			a.Close()
			return nil, e
		}
		_, _ = db.Exec("DELETE FROM sessions")
	}
	if o.AdminPassword == "" {
		if hash := os.Getenv("CHATKEEPER_ADMIN_PASSWORD_HASH"); hash != "" {
			if !validPasswordHash(hash) {
				a.Close()
				return nil, errors.New("unsupported password hash")
			}
			oldHash, _ := os.ReadFile(filepath.Join(o.Home, "state", "admin.hash"))
			if !equal(hash, string(oldHash)) {
				_, _ = db.Exec("DELETE FROM sessions")
			}
			if e = config.AtomicWrite(filepath.Join(o.Home, "state", "admin.hash"), []byte(hash), 0600); e != nil {
				a.Close()
				return nil, e
			}
		}
	}
	rows, err := db.Query("SELECT workspace,paused FROM workspace_flags")
	if err != nil {
		a.Close()
		return nil, err
	}
	for rows.Next() {
		var id string
		var paused bool
		if rows.Scan(&id, &paused) == nil {
			a.Manager.SetPaused(id, paused)
		}
	}
	_ = rows.Close()
	if _, e = os.Stat(o.Config); e == nil {
		_ = a.Manager.Reload()
	}
	a.maintenanceDone = make(chan struct{})
	go a.maintain()
	return a, nil
}
func (a *App) Close() {
	a.cancel()
	if a.maintenanceDone != nil {
		<-a.maintenanceDone
	}
	a.Manager.Close()
	_ = a.db.Close()
}
func (a *App) LaunchURL(base string) string { return base + "/login?token=" + a.launchToken }
func (a *App) Watch(ctx context.Context)    { a.Manager.Watch(ctx) }
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.URL.Path == "/healthz" && (r.Method == "GET" || r.Method == "HEAD") {
		_, _ = fmt.Fprintln(w, "ok")
		return
	}
	if !a.allowedHost(r) {
		http.Error(w, "Host not allowed", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/static/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if a.opts.Dev {
			http.StripPrefix("/static/", http.FileServer(http.Dir("internal/web/static"))).ServeHTTP(w, r)
		} else {
			http.FileServer(http.FS(assets)).ServeHTTP(w, r)
		}
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if strings.HasPrefix(r.URL.Path, "/api/desktop/") {
		a.desktopAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" || r.URL.Path == "/search" {
		a.readAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/view/") {
		a.Manager.Read(func() {
			id, _ := a.session(r)
			if id != "" {
				a.refreshCookie(w, r)
				a.serveViewer(w, r, "")
				return
			}
			token := a.Manager.BearerToken()
			if token == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			a.serveViewer(w, r, token)
		})
		return
	}

	if r.Method != "GET" && r.Method != "HEAD" {
		protect := http.NewCrossOriginProtection()
		denied := false
		protect.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			denied = true
			http.Error(w, "Cross-origin request denied", http.StatusForbidden)
		}))
		protect.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)
		if denied {
			return
		}
		if e := r.ParseForm(); e != nil {
			http.Error(w, "Invalid form", http.StatusBadRequest)
			return
		}
	}
	switch r.URL.Path {
	case "/bootstrap":
		a.bootstrap(w, r)
		return
	case "/login":
		a.login(w, r)
		return
	}
	id, csrf := a.session(r)
	if id == "" {
		if strings.HasPrefix(r.URL.Path, "/view/") && a.Manager.BearerToken() != "" {
			a.serveViewer(w, r, a.Manager.BearerToken())
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		kind := "login"
		if a.opts.Mode == "server" && !a.adminExists() {
			kind = "welcome"
		}
		a.render(w, page{Title: "Welcome to ChatKeeper", Kind: kind})
		return
	}
	a.refreshCookie(w, r)
	if r.Method != "GET" && r.Method != "HEAD" {
		if !equal(r.FormValue("csrf"), csrf) {
			http.Error(w, "CSRF token required", http.StatusForbidden)
			return
		}
	}
	if r.URL.Path == "/logout" && r.Method == "POST" {
		_, _ = a.db.Exec("DELETE FROM sessions WHERE id=?", id)
		a.mu.Lock()
		delete(a.wizards, id)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: a.cookieName(r), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil || a.opts.SecureCookies, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/view/") {
		a.serveViewer(w, r, "")
		return
	}
	switch {
	case r.URL.Path == "/" && r.Method == "GET":
		if len(a.Manager.List()) == 0 {
			a.startPage(w, csrf)
		} else {
			a.dashboard(w, r, csrf, false)
		}
	case strings.HasPrefix(r.URL.Path, "/cleanup/"):
		a.cleanupPage(w, r, id, csrf)
	case strings.HasPrefix(r.URL.Path, "/setup/"):
		a.setup(w, r, id, csrf)
	case strings.HasPrefix(r.URL.Path, "/settings/"):
		a.settings(w, r, csrf)
	default:
		http.NotFound(w, r)
	}
}
func (a *App) viewer(token string) *Viewer {
	gen := a.Manager.Generation()
	a.viewerMu.Lock()
	if gen != a.viewerGeneration || a.viewerCache == nil {
		a.viewerCache = map[string]*Viewer{}
		a.viewerGeneration = gen
	}
	if v := a.viewerCache[token]; v != nil {
		a.viewerMu.Unlock()
		return v
	}
	a.viewerMu.Unlock()
	sources := map[string]Source{}
	for _, r := range a.Manager.List() {
		sources[r.Entry.Workspace.ID] = Source{Store: r.Store, Content: r.Entry.Plugin.Content(), path: r.Entry.Workspace.Storage}
	}
	v := New(sources, token)
	a.viewerMu.Lock()
	defer a.viewerMu.Unlock()
	if a.viewerGeneration != gen {
		return v
	}
	if prior := a.viewerCache[token]; prior != nil {
		return prior
	}
	a.viewerCache[token] = v
	return v
}

// Each response owns its archive handles. Reload can close runtime stores without
// invalidating an in-flight reader or keeping retired runtime handles alive.
func (a *App) serveViewer(w http.ResponseWriter, r *http.Request, token string) {
	v := a.viewer(token)
	sources := make(map[string]Source, len(v.sources))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	workspace := ""
	if len(parts) >= 2 && parts[0] == "view" {
		workspace = parts[1]
	}
	for id, source := range v.sources {
		if id == workspace && source.path != "" {
			store, err := storage.OpenReader(source.path)
			if err != nil {
				http.Error(w, "archive unavailable", http.StatusServiceUnavailable)
				return
			}
			defer store.Close()
			source.Store = store
		}
		sources[id] = source
	}
	v.serveHTTP(w, r, sources)
}
func (a *App) render(w http.ResponseWriter, p page) {
	if a.opts.Dev {
		t, e := template.ParseFiles("internal/web/templates/admin.html")
		if e == nil {
			_ = t.ExecuteTemplate(w, "admin", p)
			return
		}
	}
	_ = adminPages.ExecuteTemplate(w, "admin", p)
}
func (a *App) startPage(w http.ResponseWriter, csrf string) {
	p := page{Title: "What would you like to connect?", Kind: "platform", CSRF: csrf, Body: "Keep the threads that matter. Choose a platform to get started.", Managed: !config.Writable(a.opts.Config)}
	for _, plugin := range pluginapi.Plugins() {
		m := plugin.Manifest()
		p.Cards = append(p.Cards, card{ID: m.ID, Name: m.Name, Ready: m.APIVersion == pluginapi.APIVersion && len(plugin.SetupFlow()) > 0})
	}
	a.render(w, p)
}
func schemaFields(raw json.RawMessage) []field {
	var s struct {
		Properties map[string]struct {
			Type, Title, Description, Pattern string
			Minimum, Maximum                  *float64
			Items                             struct{ Enum []string }
			Default                           any
			Enum                              []string
			Examples                          []string
			Secret                            bool   `json:"x-secret"`
			Widget                            string `json:"x-widget"`
		}
		Required []string
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	keys := []string{}
	for k := range s.Properties {
		keys = append(keys, k)
	}
	required := map[string]bool{}
	for _, k := range s.Required {
		required[k] = true
	}
	sort.Slice(keys, func(i, j int) bool {
		if required[keys[i]] != required[keys[j]] {
			return required[keys[i]]
		}
		return keys[i] < keys[j]
	})
	out := []field{}
	for _, k := range keys {
		v := s.Properties[k]
		f := field{Name: k, Title: v.Title, Type: "text", Description: v.Description, Widget: v.Widget, Examples: v.Examples, Pattern: v.Pattern, Step: "any"}
		if v.Minimum != nil {
			f.Minimum = strconv.FormatFloat(*v.Minimum, 'f', -1, 64)
		}
		if v.Maximum != nil {
			f.Maximum = strconv.FormatFloat(*v.Maximum, 'f', -1, 64)
		}
		if f.Title == "" {
			f.Title = strings.ReplaceAll(k, "_", " ")
		}
		if v.Default != nil {
			f.Value = fmt.Sprint(v.Default)
		}
		switch v.Type {
		case "integer", "number":
			f.Type = "number"
			if v.Type == "integer" {
				f.Step = "1"
			}
		case "boolean":
			f.Type = "checkbox"
			f.Checked = v.Default == true
		case "array":
			f.Multiple = true
			f.Type = "textarea"
			v.Enum = v.Items.Enum
			if def, ok := v.Default.([]any); ok {
				values := []string{}
				for _, x := range def {
					values = append(values, fmt.Sprint(x))
				}
				f.Value = strings.Join(values, "\n")
			}
		}
		if v.Secret {
			f.Type = "password"
			f.Value = ""
		}
		for _, req := range s.Required {
			if req == k {
				f.Required = true
			}
		}
		for _, choice := range v.Enum {
			f.Options = append(f.Options, pluginapi.Choice{ID: choice, Label: choice})
		}
		out = append(out, f)
	}
	return out
}
func formInput(r *http.Request, raw json.RawMessage) (json.RawMessage, pluginapi.Credentials) {
	fields := schemaFields(raw)
	m := map[string]any{}
	creds := pluginapi.Credentials{}
	for _, f := range fields {
		v := r.FormValue(f.Name)
		if f.Type == "password" {
			creds[f.Name] = v
			continue
		}
		if f.Multiple {
			values := []string{}
			for _, raw := range r.Form[f.Name] {
				for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
					if value := strings.TrimSpace(line); value != "" {
						values = append(values, value)
					}
				}
			}
			m[f.Name] = values
		} else if f.Type == "checkbox" {
			m[f.Name] = v != ""
		} else if f.Type == "number" {
			n, _ := strconv.ParseFloat(v, 64)
			m[f.Name] = n
		} else {
			m[f.Name] = v
		}
	}
	b, _ := json.Marshal(m)
	return b, creds
}
func (wiz *wizard) merge(data json.RawMessage) error {
	if len(data) == 0 {
		return nil
	}
	for _, secret := range wiz.Credentials {
		if secret != "" && strings.Contains(string(data), secret) {
			return errors.New("unsafe setup result")
		}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return errors.New("invalid setup result")
	}
	for k, v := range fields {
		key := strings.ToLower(k)
		if strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "secret") {
			continue
		}
		wiz.Data[k] = v
	}
	return nil
}

// persist stores diagnostic progress only. Wizards and credentials remain in memory;
// setup cannot resume after restart. Rows expire with the corresponding session.
func (a *App) persist(id string, z *wizard) {
	data, _ := json.Marshal(z.Data)
	_, _ = a.db.Exec("INSERT OR REPLACE INTO setup_sessions(id,platform,step,data,expires) VALUES(?,?,?,?,?)", id, z.Platform, z.Step, string(data), time.Now().Add(time.Hour).Unix())
}
func (a *App) setup(w http.ResponseWriter, r *http.Request, id, csrf string) {
	if r.URL.Path == "/setup/" && r.Method == "GET" {
		a.startPage(w, csrf)
		return
	}
	if r.URL.Path == "/setup/start" && r.Method == "POST" {
		p, ok := pluginapi.Lookup(r.FormValue("platform"))
		if !ok || len(p.SetupFlow()) == 0 {
			http.Error(w, "Platform unavailable", http.StatusBadRequest)
			return
		}
		z := &wizard{Platform: p.Manifest().ID, Data: pluginapi.SetupState{}, Credentials: pluginapi.Credentials{}, Started: time.Now()}
		z.References = map[string]string{}
		var cs struct {
			Properties map[string]struct {
				Env string `json:"x-env-default"`
			}
		}
		_ = json.Unmarshal(p.CredentialSchema(), &cs)
		for k, prop := range cs.Properties {
			if v := os.Getenv(prop.Env); v != "" {
				z.Credentials[k] = v
				z.References[k] = prop.Env
			}
		}
		a.persist(id, z)
		a.mu.Lock()
		a.wizards[id] = z
		a.mu.Unlock()
		http.Redirect(w, r, "/setup/step", http.StatusSeeOther)
		return
	}
	a.mu.Lock()
	z := a.wizards[id]
	a.mu.Unlock()
	if z == nil {
		http.Redirect(w, r, "/setup/", http.StatusSeeOther)
		return
	}
	if !z.mu.TryLock() {
		http.Error(w, "Setup request already in progress", http.StatusConflict)
		return
	}
	defer z.mu.Unlock()
	if time.Since(z.Started) > time.Hour {
		a.mu.Lock()
		if a.wizards[id] == z {
			delete(a.wizards, id)
		}
		a.mu.Unlock()
		_, _ = a.db.Exec("DELETE FROM setup_sessions WHERE id=?", id)
		http.Error(w, "Setup expired. Start again.", http.StatusGone)
		return
	}
	p, ok := pluginapi.Lookup(z.Platform)
	if !ok {
		http.Error(w, "Platform unavailable", http.StatusBadRequest)
		return
	}
	steps := p.SetupFlow()
	runStep := func(step pluginapi.SetupStep, input json.RawMessage) (pluginapi.StepResult, error) {
		// A timed-out, noncooperative plugin must not retain mutable wizard maps.
		state := pluginapi.SetupState{}
		for k, v := range z.Data {
			state[k] = append(json.RawMessage(nil), v...)
		}
		env := pluginapi.Env{Credentials: maps.Clone(z.Credentials), HTTP: a.opts.HTTP}
		return core.Call(r.Context(), 30*time.Second, func(ctx context.Context) (pluginapi.StepResult, error) { return step.Run(ctx, state, input, env) })
	}
	if z.Step < len(steps) {
		step := steps[z.Step]
		result := pluginapi.StepResult{OK: true}
		var e error
		if r.Method == "POST" {
			input, creds := formInput(r, step.Schema)
			for k, v := range creds {
				if z.References[k] == "" {
					z.Credentials[k] = v
				}
			}
			if step.Run != nil {
				result, e = runStep(step, input)
			}
			if e == nil && result.OK {
				e = z.merge(result.Data)
				if e == nil {
					z.Step++
					z.Messages = result.Messages
					a.persist(id, z)
					http.Redirect(w, r, "/setup/step", http.StatusSeeOther)
					return
				}
			}
		}
		if r.Method == "GET" && step.Kind == "choose" && step.Run != nil {
			result, e = runStep(step, nil)
		}
		pg := page{Title: step.Title, Kind: "step", Body: step.Body, CSRF: csrf, Step: z.Step + 1, Links: step.Links, Fields: schemaFields(step.Schema), Messages: append(append([]string{}, z.Messages...), result.Messages...)}
		if e != nil {
			pg.Error = "Connection check failed. Please retry."
		}
		for k, msg := range result.FieldErrors {
			pg.Messages = append(pg.Messages, k+": "+msg)
		}
		for i := range pg.Fields {
			if z.References[pg.Fields[i].Name] != "" {
				pg.Fields[i].Provided = true
				pg.Fields[i].Description = "Provided by environment"
			}
			if pg.Fields[i].Widget == "channel-picker" {
				pg.Fields[i].Options = result.Choices
			}
		}
		a.redactPage(&pg, z.Credentials)
		a.render(w, pg)
		return
	}
	if z.Step == len(steps) {
		a.storageStep(w, r, id, csrf, z, p)
		return
	}
	a.live(w, r, csrf, z)
}
func (a *App) redactPage(p *page, creds pluginapi.Credentials) {
	for _, secret := range creds {
		if secret == "" {
			continue
		}
		p.Body = strings.ReplaceAll(p.Body, secret, "[redacted]")
		p.Error = strings.ReplaceAll(p.Error, secret, "[redacted]")
		for i := range p.Messages {
			p.Messages[i] = strings.ReplaceAll(p.Messages[i], secret, "[redacted]")
		}
	}
}
func confined(root, path string) bool {
	base, e := filepath.Abs(root)
	if e != nil {
		return false
	}
	target, e := filepath.Abs(path)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(base, target)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	current := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		st, e := os.Lstat(current)
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if e != nil && !os.IsNotExist(e) {
			return false
		}
	}
	return true
}
func (a *App) storageStep(w http.ResponseWriter, r *http.Request, id, csrf string, z *wizard, p pluginapi.Plugin) {
	wid := r.FormValue("workspace")
	if wid == "" {
		wid = "workspace"
	}
	root := filepath.Join(a.opts.Home, "data")
	if a.opts.Mode == "desktop" {
		if home, e := os.UserHomeDir(); e == nil {
			root = filepath.Join(home, "ChatKeeper")
		}
	}
	storagePath := r.FormValue("storage")
	if storagePath == "" {
		storagePath = filepath.Join(root, wid)
	}
	pg := page{Title: "Where should we save your threads?", Kind: "storage", CSRF: csrf, Storage: storagePath, Workspace: wid, Managed: !config.Writable(a.opts.Config)}
	if r.Method == "POST" {
		exists := false
		for _, running := range a.Manager.List() {
			if running.Entry.Workspace.ID == wid {
				exists = true
			}
		}
		if exists {
			pg.Error = "This workspace name is already in use. Choose a different name."
		} else if !workspacePattern.MatchString(wid) {
			pg.Error = "Use lowercase letters, numbers, underscores or hyphens for the workspace name."
		} else if a.opts.Mode == "server" && !confined(root, storagePath) {
			pg.Error = "Choose a folder inside the server data root."
		} else {
			entry := map[string]any{"id": wid, "platform": z.Platform, "storage": map[string]any{"type": "folder", "path": storagePath}}
			for k, v := range z.Data {
				var value any
				_ = json.Unmarshal(v, &value)
				entry[k] = value
			}
			for _, f := range schemaFields(p.CredentialSchema()) {
				name := "CHATKEEPER_" + strings.ToUpper(hex.EncodeToString([]byte(wid))) + "_" + strings.ToUpper(f.Name)
				if z.References[f.Name] != "" {
					name = z.References[f.Name]
				}
				entry[f.Name+"_env"] = name
			}
			b, _ := json.Marshal(entry)
			if _, e := p.ParseWorkspace(r.Context(), b, z.Credentials); e != nil {
				pg.Error = "Workspace settings are invalid. Restart setup and validate the connection."
			} else {
				yamlBytes, _ := json.MarshalIndent(entry, "", "  ")
				pg.YAML = string(yamlBytes)
				if pg.Managed {
					pg.Error = "Configuration is managed. Apply this workspace in your YAML configuration; credentials must be supplied separately."
				} else {
					var e error
					for k, v := range z.Credentials {
						if v != "" && z.References[k] == "" {
							name, ok := entry[k+"_env"].(string)
							if !ok || name == "" {
								e = errors.New("missing credential reference")
								break
							}
							e = a.Secrets.Set(name, v)
							if e != nil {
								break
							}
						}
					}
					if e == nil {
						e = a.validateAndWrite(wid, entry)
					}
					if e == nil {
						e = a.Manager.Reload()
					}
					if e != nil {
						pg.Error = "Could not save workspace. Check configuration, folder permissions and credential sources."
					} else {
						z.Credentials = nil
						z.Workspace = wid
						z.Step++
						z.Started = time.Now()
						a.persist(id, z)
						http.Redirect(w, r, "/setup/step", http.StatusSeeOther)
						return
					}
				}
			}
		}
	}
	a.render(w, pg)
}
func (a *App) validateAndWrite(id string, entry map[string]any) error {
	return config.UpdateValidated(a.opts.Config, id, entry, func(path string) error {
		var resolveErr error
		_, err := config.LoadWithResolver(path, true, func(k string) string {
			v, _, e := a.Secrets.Get(k)
			if e != nil {
				resolveErr = e
			}
			return v
		})
		if resolveErr != nil {
			return resolveErr
		}
		return err
	})
}
func (a *App) archiveRows(workspace string) []row {
	rows := []row{}
	for _, r := range a.Manager.List() {
		wid := r.Entry.Workspace.ID
		if workspace != "" && wid != workspace {
			continue
		}
		store, e := storage.Open(r.Entry.Workspace.Storage)
		if e != nil {
			continue
		}
		keys, e := store.Keys(a.ctx)
		_ = store.Close()
		if e != nil {
			continue
		}
		for _, k := range keys {
			rows = append(rows, row{URL: "/view/" + wid + "/" + k.Channel + "/" + k.RootTS, Label: wid + " · " + k.Channel + " / " + k.RootTS})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].URL > rows[j].URL })
	if len(rows) > 30 {
		rows = rows[:30]
	}
	return rows
}
func (a *App) live(w http.ResponseWriter, r *http.Request, csrf string, z *wizard) {
	reaction := "your chosen reaction"
	var trigger struct{ Reactions []string }
	if json.Unmarshal(z.Data["trigger"], &trigger) == nil && len(trigger.Reactions) > 0 {
		reaction = strings.Join(trigger.Reactions, ": and :")
	}
	var eventCount int
	_ = a.db.QueryRow("SELECT count(*) FROM events WHERE workspace=? AND at>=?", z.Workspace, z.Started.Unix()).Scan(&eventCount)
	pg := page{Title: "Try your first archive", Kind: "live", CSRF: csrf, Workspace: z.Workspace, Reaction: reaction, Rows: a.archiveRows(z.Workspace)}
	if eventCount == 0 {
		pg.Rows = nil
	}
	if len(pg.Rows) > 0 {
		pg.Title = "Your first thread is saved!"
	}
	if time.Since(z.Started) > 2*time.Minute {
		pg.Messages = []string{"Still waiting? Invite the bot to the channel, check the reaction name and owner filter. Polling-only connections can take two minutes. You can finish now and test later."}
	}
	a.render(w, pg)
}

func (a *App) maintain() {
	defer close(a.maintenanceDone)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.purgeExpired()
		}
	}
}
func (a *App) purgeExpired() {
	_, _ = a.db.Exec("DELETE FROM sessions WHERE expires<=unixepoch(); DELETE FROM setup_sessions WHERE expires<=unixepoch() OR id NOT IN (SELECT id FROM sessions); DELETE FROM events WHERE at<unixepoch()-604800")
	a.mu.Lock()
	snapshot := make(map[string]*wizard, len(a.wizards))
	for id, z := range a.wizards {
		snapshot[id] = z
	}
	a.mu.Unlock()
	for id, z := range snapshot {
		if !z.mu.TryLock() {
			continue
		}
		var exists int
		expired := time.Since(z.Started) > time.Hour || a.db.QueryRow("SELECT 1 FROM sessions WHERE id=? AND expires>unixepoch()", id).Scan(&exists) != nil
		z.mu.Unlock()
		if expired {
			a.mu.Lock()
			if a.wizards[id] == z {
				delete(a.wizards, id)
			}
			a.mu.Unlock()
		}
	}
}
