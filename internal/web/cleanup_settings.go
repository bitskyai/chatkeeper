package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var cleanupSettingsTemplate = template.Must(template.Must(adminPages.Clone()).ParseFS(templates, "templates/cleanup-settings.html"))

type cleanupSettingsPage struct {
	CSRF, Workspace, Error, Notice, OAuthURL, Instructions, Title, WizardTitle, Age, Every, Action, ImageOrigins, CredentialDraft string
	Enabled, AllowOthers, Immediate, Automatic, Managed, Wizard, CredentialSet                                                    bool
	Scope                                                                                                                         string
	Reaction, Owners                                                                                                              field
}

func cleanupReference(id string) string {
	return "CHATKEEPER_" + strings.ToUpper(hex.EncodeToString([]byte(id))) + "_CLEANUP_TOKEN"
}
func (a *App) cleanupSettings(w http.ResponseWriter, r *http.Request, csrf string, wizard bool) {
	wizard = wizard || r.FormValue("wizard") == "true"
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, e := config.Load(a.opts.Config, false)
	if e != nil {
		http.Error(w, "Cannot load workspace configuration", http.StatusConflict)
		return
	}
	var entry *config.Entry
	for i := range cfg.Workspaces {
		if cfg.Workspaces[i].Workspace.ID == r.FormValue("workspace") {
			entry = &cfg.Workspaces[i]
			break
		}
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	provider, ok := entry.Plugin.(pluginapi.CleanupConfigurator)
	if !ok {
		http.Error(w, "Source cleanup configuration unavailable", http.StatusConflict)
		return
	}
	ws := entry.Workspace
	// Safety controls must remain available during a source outage.
	if r.Method == "POST" && (r.FormValue("action") == "disable" || r.FormValue("action") == "stop_schedule" || ws.Cleanup.Enabled && r.FormValue("enabled") != "true" && r.FormValue("action") != "test") {
		var raw map[string]any
		_ = json.Unmarshal(ws.Settings, &raw)
		policy, _ := raw["cleanup"].(map[string]any)
		if policy == nil {
			policy = map[string]any{}
		}
		if r.FormValue("action") != "stop_schedule" {
			policy["enabled"] = false
		}
		policy["every_hours"] = 0
		raw["cleanup"] = policy
		e = config.UpdateValidated(a.opts.Config, ws.ID, raw, func(path string) error { _, err := config.Load(path, false); return err })
		if e == nil {
			// Capture credential failure must not prevent a durable cleanup OFF.
			_ = a.Manager.Reload()
		}
		if e != nil {
			http.Error(w, "Cannot disable managed or invalid configuration", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/settings/cleanup?workspace="+url.QueryEscape(ws.ID)+"&saved=1", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	env := pluginapi.Env{Credentials: pluginapi.Credentials{}, HTTP: a.opts.HTTP}
	var raw map[string]any
	_ = json.Unmarshal(ws.Settings, &raw)
	for _, f := range schemaFields(entry.Plugin.CredentialSchema()) {
		v, _, _ := a.Secrets.Get(credentialReference(entry.Plugin, raw, f.Name))
		env.Credentials[f.Name] = v
	}
	setup, setupErr := provider.CleanupSetup(ctx, ws, env)
	pg := cleanupSettingsPage{CSRF: csrf, Workspace: ws.ID, Title: setup.Title, WizardTitle: setup.WizardTitle, Instructions: setup.Instructions, OAuthURL: setup.OAuthURL, Enabled: ws.Cleanup.Enabled, AllowOthers: ws.Cleanup.AllowOthers, Age: strconv.FormatFloat(ws.Cleanup.MinAge.Hours(), 'f', -1, 64), Scope: ws.Cleanup.Scope, Every: strconv.FormatFloat(ws.Cleanup.Every.Hours(), 'f', -1, 64), Automatic: ws.Cleanup.Every > 0, Immediate: ws.Cleanup.MinAge == 0, Managed: !config.Writable(a.opts.Config), Wizard: wizard, Action: "/settings/cleanup"}
	if pg.Scope == "" {
		pg.Scope = "thread"
	}
	if pg.Every == "0" {
		pg.Every = "24"
	}
	if setupErr != nil {
		pg.Error = setupErr.Error()
	}
	approval := ws.CleanupApproval()
	// New UI policies start with a separate wastebasket approval, never the archive trigger.
	if ws.Cleanup.Approval == nil && !pg.Managed {
		approval = pluginapi.Trigger{Who: "owner", Reactions: []string{"wastebasket"}}
	}
	pg.Reaction = field{Name: "reactions", Title: "Delete-approval reactions (separate from archive reactions)", Widget: "reaction-picker", Value: strings.Join(approval.Reactions, ","), Options: setup.Reactions}
	pg.Owners = field{Name: "owners", Title: "Who may approve? Only listed owners — pick yourself to start", Multiple: true, Options: setup.Members, Selected: map[string]bool{}}
	for _, id := range approval.Owners {
		pg.Owners.Selected[id] = true
	}
	ref := ws.Cleanup.TokenEnv
	if ws.Cleanup.Approval == nil || ref == "" {
		ref = cleanupReference(ws.ID)
	}
	token, _, _ := a.Secrets.Get(ref)
	draft := r.FormValue("credential_draft")
	draftPrefix := cleanupReference(ws.ID) + "_DRAFT_"
	if strings.HasPrefix(draft, draftPrefix) && len(draft) == len(draftPrefix)+86 {
		if v, _, err := a.Secrets.Get(draft); err == nil && v != "" {
			token = v
			ref = draft
			pg.CredentialDraft = draft
		}
	}
	pg.CredentialSet = token != ""
	if r.Method == "POST" {
		if pg.Managed {
			http.Error(w, "Configuration is managed; update your YAML", http.StatusConflict)
			return
		}
		pg.Enabled = r.FormValue("enabled") == "true"
		pg.AllowOthers = r.FormValue("allow_others") == "true"
		pg.Immediate = r.FormValue("allow_zero_age") == "true"
		pg.Automatic = r.FormValue("automatic") == "true"
		pg.Age = r.FormValue("min_age_hours")
		pg.Every = r.FormValue("every_hours")
		pg.Scope = r.FormValue("scope")
		pg.Reaction.Value = strings.Join(r.Form["reactions"], ",")
		pg.Owners.Selected = map[string]bool{}
		owners := []string{}
		validMembers := map[string]bool{}
		for _, m := range setup.Members {
			validMembers[m.ID] = true
		}
		invalidOwner := false
		for _, id := range r.Form["owners"] {
			if !validMembers[id] {
				invalidOwner = true
			}
			if !pg.Owners.Selected[id] {
				owners = append(owners, id)
			}
			pg.Owners.Selected[id] = true
		}
		reactions := []string{}
		seen := map[string]bool{}
		for _, name := range strings.FieldsFunc(pg.Reaction.Value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
			name = strings.Trim(name, ":")
			if name != "" && !seen[name] {
				reactions = append(reactions, name)
				seen[name] = true
			}
		}
		age, ageErr := strconv.ParseFloat(pg.Age, 64)
		every := float64(0)
		var everyErr error
		if pg.Automatic {
			every, everyErr = strconv.ParseFloat(pg.Every, 64)
		}
		if v := strings.TrimSpace(r.FormValue("cleanup_token")); v != "" {
			token = v
		}
		check := func() error {
			if ageErr != nil || age < 0 || age > 1000000 || age != age || age == 0 && !pg.Immediate {
				return errors.New("minimum age must be valid; 0 requires Allow immediate")
			}
			if everyErr != nil || every < 0 || every > 1000000 || every != every || pg.Automatic && every < 1 {
				return errors.New("automatic interval must be at least 1 hour")
			}
			if pg.Scope != "thread" && pg.Scope != "messages" {
				return errors.New("choose whole thread or only approved messages")
			}
			if len(reactions) == 0 {
				return errors.New("choose at least one delete-approval reaction")
			}
			if invalidOwner || len(owners) == 0 {
				return errors.New("pick yourself from workspace members; only listed owners may approve")
			}
			if setupErr != nil {
				return setupErr
			}
			if pg.Enabled || r.FormValue("action") == "test" || r.FormValue("cleanup_token") != "" {
				return provider.TestCleanupCredential(ctx, ws, env, token, pg.AllowOthers)
			}
			return nil
		}
		pg.Error = ""
		if e = check(); e != nil {
			pg.Error = e.Error()
		} else if r.FormValue("action") == "test" {
			draftRef := draftPrefix + hex.EncodeToString([]byte(randomToken()))
			if e = a.Secrets.Set(draftRef, token); e != nil {
				pg.Error = "Cannot save credential in credential store"
			} else {
				pg.CredentialSet = true
				pg.CredentialDraft = draftRef
				pg.Notice = "Credential verified: workspace, identity, permissions and scopes. Save settings to persist changes."
			}
		} else {
			policy, _ := raw["cleanup"].(map[string]any)
			if policy == nil {
				policy = map[string]any{}
			}
			policy["enabled"] = pg.Enabled
			policy["allow_others"] = pg.AllowOthers
			policy["own_only"] = !pg.AllowOthers
			policy["min_age_hours"] = age
			policy["allow_zero_age"] = pg.Immediate
			policy["scope"] = pg.Scope
			policy["every_hours"] = every
			policy["approval"] = pluginapi.Trigger{Who: "owner", Owners: owners, Reactions: reactions, Match: "any"}

			// New immutable reference: a failed credential write cannot change
			// a currently enabled policy's identity or publish a new policy.
			if r.FormValue("cleanup_token") != "" {
				ref = draftPrefix + hex.EncodeToString([]byte(randomToken()))
			}
			policy["token_env"] = ref
			raw["cleanup"] = policy
			// Validate the complete candidate before storing secrets or publishing configuration.
			e = config.UpdateValidated(a.opts.Config, ws.ID, raw, func(path string) error {
				if _, err := config.Load(path, false); err != nil {
					return err
				}
				if token != "" && (r.FormValue("cleanup_token") != "" || ref != ws.Cleanup.TokenEnv && pg.CredentialDraft == "") {
					return a.Secrets.Set(ref, token)
				}
				return nil
			})
			if e == nil {
				e = a.Manager.Reload()
			}
			if e != nil {
				pg.Error = "Could not save cleanup settings. Check credential-store access and configuration permissions."
			} else {
				suffix := ""
				if wizard {
					suffix = "&wizard=true"
				}
				http.Redirect(w, r, "/settings/cleanup?workspace="+url.QueryEscape(ws.ID)+"&saved=1"+suffix, http.StatusSeeOther)
				return
			}
		}
	}
	if r.URL.Query().Get("saved") == "1" {
		pg.Notice = "Cleanup settings saved"
	}
	pickerGroups(&pg.Reaction)
	pg.ImageOrigins = imageOrigins(pg.Reaction.Options)
	if pg.ImageOrigins != "" {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' "+pg.ImageOrigins+"; script-src 'self'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = cleanupSettingsTemplate.ExecuteTemplate(w, "cleanup-settings", pg)
}
