package web

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bitskyai/chatkeeper/internal/app"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// copyItem is a CopyText prepared for the template.
type copyItem struct {
	Label, Value string
	Block        bool
}

// bodyPart is a run of step text; Copy parts render as code with a copy button.
type bodyPart struct {
	Text, ID string
	Copy     bool
}

// stepBody renders backticked values in body that match a Copy entry inline
// with a copy button and returns the entries left for the copy list.
func stepBody(body string, copies []pluginapi.CopyText) ([]bodyPart, []copyItem) {
	values := map[string]bool{}
	for _, c := range copies {
		if !strings.Contains(c.Value, "\n") {
			values[c.Value] = true
		}
	}
	parts, used := []bodyPart{}, map[string]bool{}
	rest := body
	for {
		open := strings.Index(rest, "`")
		if open < 0 {
			break
		}
		end := strings.Index(rest[open+1:], "`")
		if end < 0 {
			break
		}
		value := rest[open+1 : open+1+end]
		if !values[value] {
			parts = append(parts, bodyPart{Text: rest[:open+2+end]})
			rest = rest[open+2+end:]
			continue
		}
		parts = append(parts, bodyPart{Text: rest[:open]}, bodyPart{Text: value, Copy: true, ID: fmt.Sprintf("inline-%d", len(used))})
		used[value] = true
		rest = rest[open+2+end:]
	}
	parts = append(parts, bodyPart{Text: rest})
	left := []pluginapi.CopyText{}
	for _, c := range copies {
		if !used[c.Value] {
			left = append(left, c)
		}
	}
	if len(used) == 0 {
		parts = nil
	}
	return parts, copyItems(left)
}

func copyItems(in []pluginapi.CopyText) []copyItem {
	out := make([]copyItem, 0, len(in))
	for _, c := range in {
		out = append(out, copyItem{Label: c.Label, Value: c.Value, Block: strings.Contains(c.Value, "\n")})
	}
	return out
}

// mergeLinks keeps the first link for each URL, in order.
func mergeLinks(lists ...[]pluginapi.Link) []pluginapi.Link {
	out, seen := []pluginapi.Link{}, map[string]bool{}
	for _, list := range lists {
		for _, l := range list {
			if !seen[l.URL] {
				seen[l.URL] = true
				out = append(out, l)
			}
		}
	}
	return out
}

func secretFields(schema json.RawMessage) []field {
	out := []field{}
	for _, f := range schemaFields(schema) {
		if f.Type == "password" {
			out = append(out, f)
		}
	}
	return out
}

// upgradeLinks lists Upgrade actions whose secret fields the workspace lacks.
func (a *App) upgradeLinks(running *app.Running) []pluginapi.Link {
	if !config.Writable(a.opts.Config) {
		return nil
	}
	var raw map[string]any
	_ = json.Unmarshal(running.Entry.Workspace.Settings, &raw)
	out := []pluginapi.Link{}
	for _, step := range running.Entry.Plugin.SetupFlow() {
		if step.Upgrade == "" {
			continue
		}
		missing := false
		for _, f := range secretFields(step.Schema) {
			name := credentialReference(running.Entry.Plugin, raw, f.Name)
			if v, _, _ := a.Secrets.Get(name); name != "" && v == "" {
				missing = true
			}
		}
		if missing {
			out = append(out, pluginapi.Link{Label: step.Upgrade, URL: "/settings/upgrade?" + url.Values{"workspace": {running.Entry.Workspace.ID}, "step": {step.ID}}.Encode()})
		}
	}
	return out
}

// upgrade reopens one setup step for a running workspace and saves only that
// step's secret fields once its Run hook accepts them.
func (a *App) upgrade(w http.ResponseWriter, r *http.Request, csrf string) {
	if !config.Writable(a.opts.Config) {
		http.Error(w, "Configuration is managed; apply changes in YAML", http.StatusConflict)
		return
	}
	wid, sid := r.FormValue("workspace"), r.FormValue("step")
	var running *app.Running
	for _, current := range a.Manager.List() {
		if current.Entry.Workspace.ID == wid {
			running = current
		}
	}
	if running == nil {
		http.NotFound(w, r)
		return
	}
	var step *pluginapi.SetupStep
	steps := running.Entry.Plugin.SetupFlow()
	for i := range steps {
		if steps[i].ID == sid && steps[i].Upgrade != "" {
			step = &steps[i]
		}
	}
	if step == nil {
		http.NotFound(w, r)
		return
	}
	p := running.Entry.Plugin
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(running.Entry.Workspace.Settings, &raw)
	var refs map[string]any
	_ = json.Unmarshal(running.Entry.Workspace.Settings, &refs)
	state := pluginapi.SetupState{}
	for k, v := range raw {
		if !strings.HasSuffix(k, "_env") {
			state[k] = v
		}
	}
	creds := pluginapi.Credentials{}
	for _, f := range schemaFields(p.CredentialSchema()) {
		if v, _, _ := a.Secrets.Get(credentialReference(p, refs, f.Name)); v != "" {
			creds[f.Name] = v
		}
	}
	call := func(hook func(context.Context, pluginapi.SetupState, json.RawMessage, pluginapi.Env) (pluginapi.StepResult, error), input json.RawMessage) (pluginapi.StepResult, error) {
		st := pluginapi.SetupState{}
		for k, v := range state {
			st[k] = append(json.RawMessage(nil), v...)
		}
		env := pluginapi.Env{Credentials: maps.Clone(creds), HTTP: a.opts.HTTP}
		return core.Call(r.Context(), 30*time.Second, func(ctx context.Context) (pluginapi.StepResult, error) { return hook(ctx, st, input, env) })
	}
	var result pluginapi.StepResult
	var e error
	tested := false
	if r.Method == "POST" {
		input, entered := formInput(r, step.Schema)
		for k, v := range entered {
			if v != "" {
				creds[k] = v
			}
		}
		hook := step.Run
		if r.FormValue("action") == "test" && step.Test != nil {
			hook, tested = step.Test, true
		}
		if hook != nil {
			result, e = call(hook, input)
		}
		if e == nil && result.OK && !tested {
			for k, v := range entered {
				if v == "" {
					continue
				}
				name := credentialReference(p, refs, k)
				if name == "" || a.Secrets.Set(name, v) != nil {
					http.Error(w, "Credential save failed. Environment credentials must be changed on the host.", http.StatusBadRequest)
					return
				}
			}
			if a.Manager.Reload() != nil {
				http.Error(w, "Configuration reload failed; previous configuration retained", http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, "/#workspace-"+wid, http.StatusSeeOther)
			return
		}
	}
	var prepared pluginapi.StepResult
	if step.Prepare != nil {
		st := pluginapi.SetupState{}
		maps.Copy(st, state)
		env := pluginapi.Env{Credentials: maps.Clone(creds), HTTP: a.opts.HTTP}
		prepared, _ = core.Call(r.Context(), 30*time.Second, func(ctx context.Context) (pluginapi.StepResult, error) { return step.Prepare(ctx, st, env) })
	}
	parts, copies := stepBody(step.Body, step.Copy)
	pg := page{BodyParts: parts, Copy: copies, Title: step.Title, Kind: "upgrade", Body: step.Body, CSRF: csrf, Workspace: wid, StepID: step.ID, Links: mergeLinks(step.Links, prepared.Links, result.Links), Fields: secretFields(step.Schema), CanTest: step.Test != nil, Checks: result.Checks, Tested: tested}
	pg.Messages = append(append(pg.Messages, prepared.Messages...), result.Messages...)
	for k, msg := range result.FieldErrors {
		pg.Messages = append(pg.Messages, k+": "+msg)
	}
	if e != nil {
		pg.Error = "Connection check failed. Please retry."
	}
	a.redactPage(&pg, creds)
	a.render(w, pg)
}
