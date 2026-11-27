package web

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"sort"
	"time"

	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// WorkspaceStatus reports which credentials a workspace has, never their values.
type WorkspaceStatus struct {
	ID          string          `json:"id"`
	Platform    string          `json:"platform"`
	Credentials map[string]bool `json:"credentials"`
	// Available lists optional setup actions whose credentials are missing,
	// for example enabling live events.
	Available []string `json:"available,omitempty"`
}

// SetupStatusResult is the setup_status MCP tool result.
type SetupStatusResult struct {
	Configured bool              `json:"configured"`
	Platforms  []string          `json:"platforms"`
	Workspaces []WorkspaceStatus `json:"workspaces"`
	Next       string            `json:"next"`
}

// storedCredentials resolves a workspace's credentials from the store.
func storedCredentials(p pluginapi.Plugin, settings json.RawMessage, store *secrets.Store) (pluginapi.Credentials, error) {
	var raw map[string]any
	_ = json.Unmarshal(settings, &raw)
	creds := pluginapi.Credentials{}
	for _, f := range schemaFields(p.CredentialSchema()) {
		name := credentialReference(p, raw, f.Name)
		if name == "" {
			continue
		}
		v, _, e := store.Get(name)
		if e != nil {
			return nil, errors.New("secret store unavailable")
		}
		if v != "" {
			creds[f.Name] = v
		}
	}
	return creds, nil
}

// SetupStatus summarizes setup for an agent: platforms, workspaces and which
// credentials are present. It reads secrets only to test presence.
func SetupStatus(configPath string, store *secrets.Store) (SetupStatusResult, error) {
	res := SetupStatusResult{Platforms: []string{}, Workspaces: []WorkspaceStatus{}}
	for _, p := range pluginapi.Plugins() {
		if len(p.SetupFlow()) > 0 {
			res.Platforms = append(res.Platforms, p.Manifest().ID)
		}
	}
	sort.Strings(res.Platforms)
	cfg, e := config.Load(configPath, false)
	if e != nil {
		res.Next = "No valid configuration yet. Run: chatkeeper setup PLATFORM --help"
		return res, nil
	}
	for _, entry := range cfg.Workspaces {
		creds, e := storedCredentials(entry.Plugin, json.RawMessage(entry.Workspace.Settings), store)
		if e != nil {
			return res, e
		}
		ws := WorkspaceStatus{ID: entry.Workspace.ID, Platform: entry.Plugin.Manifest().ID, Credentials: map[string]bool{}}
		for _, f := range schemaFields(entry.Plugin.CredentialSchema()) {
			ws.Credentials[f.Name] = creds[f.Name] != ""
		}
		for _, step := range entry.Plugin.SetupFlow() {
			for _, f := range secretFields(step.Schema) {
				if step.Upgrade != "" && creds[f.Name] == "" {
					ws.Available = append(ws.Available, step.Upgrade)
					break
				}
			}
		}
		res.Workspaces = append(res.Workspaces, ws)
	}
	res.Configured = len(res.Workspaces) > 0
	res.Next = "Run validate_setup for a workspace to test its stored credentials."
	if !res.Configured {
		res.Next = "No workspaces. Run: chatkeeper setup PLATFORM --help"
	}
	return res, nil
}

// ValidateWorkspace re-runs the connection tests of a saved workspace with its
// stored credentials. Results are redacted; nothing is written.
func ValidateWorkspace(ctx context.Context, configPath string, store *secrets.Store, workspace string, client *http.Client) (SetupResult, error) {
	cfg, e := config.Load(configPath, false)
	if e != nil {
		return SetupResult{}, errors.New("configuration is invalid or missing")
	}
	for _, entry := range cfg.Workspaces {
		if entry.Workspace.ID != workspace {
			continue
		}
		p := entry.Plugin
		creds, e := storedCredentials(p, json.RawMessage(entry.Workspace.Settings), store)
		if e != nil {
			return SetupResult{}, e
		}
		res := SetupResult{OK: true, Platform: p.Manifest().ID, Workspace: workspace, Steps: []SetupStepResult{}}
		state := pluginapi.SetupState{}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(entry.Workspace.Settings, &raw)
		maps.Copy(state, raw)
		for _, step := range p.SetupFlow() {
			secretsIn := secretFields(step.Schema)
			if step.Test == nil || len(secretsIn) == 0 {
				continue
			}
			missing := false
			for _, f := range secretsIn {
				missing = missing || creds[f.Name] == ""
			}
			if missing {
				r := SetupStepResult{ID: step.ID, Title: step.Title, OK: step.Skip != "", Skipped: true, Messages: []string{"credential not set"}}
				res.OK = res.OK && r.OK
				res.Steps = append(res.Steps, r)
				continue
			}
			st := maps.Clone(state)
			env := pluginapi.Env{Credentials: maps.Clone(creds), HTTP: client}
			out, e := core.Call(ctx, 60*time.Second, func(c context.Context) (pluginapi.StepResult, error) { return step.Test(c, st, nil, env) })
			r := SetupStepResult{ID: step.ID, Title: step.Title}
			if e == nil {
				r = redactStep(step, out, creds)
			} else {
				r.Messages = []string{"the check could not complete; retry"}
			}
			res.OK = res.OK && r.OK
			res.Steps = append(res.Steps, r)
		}
		return res, nil
	}
	return SetupResult{}, errors.New("unknown workspace")
}
