package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// SetupField is one setup input the non-interactive CLI accepts. Secret fields
// are read only from environment variables (--<flag>-env VAR).
type SetupField struct {
	Step, Name, Flag, Title, Type string
	Secret, Required, Optional    bool
	Default, EnvDefault           string
}

// SetupFields lists the flag-settable inputs of a plugin's setup flow.
func SetupFields(p pluginapi.Plugin) []SetupField {
	envDefaults := map[string]string{}
	var cs struct {
		Properties map[string]struct {
			Env string `json:"x-env-default"`
		}
	}
	_ = json.Unmarshal(p.CredentialSchema(), &cs)
	for k, v := range cs.Properties {
		envDefaults[k] = v.Env
	}
	out := []SetupField{}
	for _, step := range p.SetupFlow() {
		if step.Run == nil {
			continue
		}
		var raw struct {
			Properties map[string]struct {
				Type string
			}
		}
		_ = json.Unmarshal(step.Schema, &raw)
		for _, f := range schemaFields(step.Schema) {
			sf := SetupField{Step: step.ID, Name: f.Name, Flag: strings.ReplaceAll(f.Name, "_", "-"), Title: f.Title, Type: raw.Properties[f.Name].Type, Required: f.Required, Optional: step.Skip != "", Default: strings.ReplaceAll(f.Value, "\n", ",")}
			if f.Type == "password" {
				sf.Secret, sf.Flag, sf.Default, sf.EnvDefault = true, sf.Flag+"-env", "", envDefaults[f.Name]
			}
			out = append(out, sf)
		}
	}
	return out
}

// SetupOptions drive one non-interactive setup run.
type SetupOptions struct {
	Platform, Workspace, Storage, Config, Home, Mode string
	// Values holds flag values by field name; secret fields hold the
	// environment variable name to read.
	Values  map[string]string
	Getenv  func(string) string
	Secrets *secrets.Store
	HTTP    *http.Client
	// Skip lists optional steps (by id) to skip even when their inputs are set.
	Skip    map[string]bool
	DryRun  bool
	Replace bool
}

// SetupCheck is a redacted connection check in the JSON result.
type SetupCheck struct {
	Label    string `json:"label"`
	Detail   string `json:"detail"`
	OK       bool   `json:"ok"`
	Optional bool   `json:"optional,omitempty"`
}

// SetupLink is a page the human may need to open.
type SetupLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// SetupStepResult reports one setup step.
type SetupStepResult struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	OK       bool              `json:"ok"`
	Skipped  bool              `json:"skipped,omitempty"`
	Messages []string          `json:"messages,omitempty"`
	Errors   map[string]string `json:"field_errors,omitempty"`
	Checks   []SetupCheck      `json:"checks,omitempty"`
	Links    []SetupLink       `json:"links,omitempty"`
}

// SetupResult is printed as JSON. It never contains credential values.
type SetupResult struct {
	OK          bool              `json:"ok"`
	DryRun      bool              `json:"dry_run,omitempty"`
	Platform    string            `json:"platform"`
	Workspace   string            `json:"workspace,omitempty"`
	Config      string            `json:"config,omitempty"`
	Storage     string            `json:"storage,omitempty"`
	Error       string            `json:"error,omitempty"`
	FailedStep  string            `json:"failed_step,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
	Settings    map[string]any    `json:"settings,omitempty"`
	Steps       []SetupStepResult `json:"steps"`
}

// RunSetup runs every step of a plugin's setup flow with the same Run hooks
// (and so the same checks) as the browser wizard, then saves the workspace.
func RunSetup(ctx context.Context, o SetupOptions) SetupResult {
	res := SetupResult{Platform: o.Platform, DryRun: o.DryRun, Steps: []SetupStepResult{}}
	creds := pluginapi.Credentials{}
	redact := func(s string) string {
		for _, v := range creds {
			if v != "" {
				s = strings.ReplaceAll(s, v, "[redacted]")
			}
		}
		return s
	}
	fail := func(step, msg string) SetupResult {
		res.OK, res.FailedStep, res.Error = false, step, redact(msg)
		return res
	}
	p, ok := pluginapi.Lookup(o.Platform)
	if !ok || len(p.SetupFlow()) == 0 {
		return fail("", "unknown platform "+strconv.Quote(o.Platform))
	}
	fields := SetupFields(p)
	byName := map[string]SetupField{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for k := range o.Values {
		if _, known := byName[k]; !known {
			return fail("", "unknown setup option --"+strings.ReplaceAll(k, "_", "-"))
		}
	}
	for id := range o.Skip {
		skippable := false
		for _, step := range p.SetupFlow() {
			skippable = skippable || (step.ID == id && step.Skip != "")
		}
		if !skippable {
			return fail("", "--skip "+id+": not an optional step")
		}
	}
	credSource := map[string]string{}
	for _, f := range fields {
		if !f.Secret {
			continue
		}
		env := o.Values[f.Name]
		if env == "" {
			env = f.EnvDefault
		}
		if env == "" {
			continue
		}
		if v := strings.TrimSpace(o.Getenv(env)); v != "" {
			creds[f.Name], credSource[f.Name] = v, env
		} else if o.Values[f.Name] != "" {
			return fail(f.Step, "environment variable "+env+" (--"+f.Flag+") is empty")
		}
	}
	if !o.DryRun {
		if !workspacePattern.MatchString(o.Workspace) {
			return fail("", "--workspace-name is required: lowercase letters, numbers, underscores or hyphens")
		}
		if !config.Writable(o.Config) {
			return fail("", "configuration is managed (not writable): "+o.Config)
		}
		if c, e := config.Load(o.Config, false); e == nil && !o.Replace {
			for _, w := range c.Workspaces {
				if w.Workspace.ID == o.Workspace {
					return fail("", "workspace "+o.Workspace+" already exists; pass --replace to overwrite it")
				}
			}
		}
	}
	state := pluginapi.SetupState{}
	call := func(hook func(context.Context, pluginapi.SetupState, json.RawMessage, pluginapi.Env) (pluginapi.StepResult, error), input json.RawMessage) (pluginapi.StepResult, error) {
		st := pluginapi.SetupState{}
		for k, v := range state {
			st[k] = append(json.RawMessage(nil), v...)
		}
		env := pluginapi.Env{Credentials: maps.Clone(creds), HTTP: o.HTTP}
		return core.Call(ctx, 60*time.Second, func(c context.Context) (pluginapi.StepResult, error) { return hook(c, st, input, env) })
	}
	report := func(step pluginapi.SetupStep, r pluginapi.StepResult) SetupStepResult {
		return redactStep(step, r, creds)
	}
	for _, step := range p.SetupFlow() {
		if step.Run == nil {
			continue
		}
		input := map[string]any{}
		provided := false
		for _, f := range fields {
			if f.Step != step.ID {
				continue
			}
			if f.Secret {
				provided = provided || creds[f.Name] != ""
				continue
			}
			v, has := o.Values[f.Name]
			if has {
				provided = true
			} else if f.Default != "" {
				v = f.Default
			} else {
				continue
			}
			value, e := typed(f.Type, v)
			if e != nil {
				return fail(step.ID, "--"+f.Flag+": "+e.Error())
			}
			input[f.Name] = value
		}
		if step.Skip != "" && (!provided || o.Skip[step.ID]) {
			for _, f := range fields {
				if f.Step == step.ID && f.Secret {
					delete(creds, f.Name)
				}
			}
			r := SetupStepResult{ID: step.ID, Title: step.Title, OK: true, Skipped: true}
			if step.SkipNote != "" {
				r.Messages = []string{step.SkipNote}
			}
			if step.Prepare != nil {
				if prep, e := call(func(c context.Context, st pluginapi.SetupState, _ json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
					return step.Prepare(c, st, env)
				}, nil); e == nil {
					r.Links = report(step, prep).Links
				}
			}
			res.Steps = append(res.Steps, r)
			continue
		}
		if step.Kind == "choose" {
			if listed, e := call(step.Run, nil); e == nil {
				mapChoices(input, listed.Choices)
			}
		}
		b, _ := json.Marshal(input)
		r, e := call(step.Run, b)
		if e != nil {
			res.Steps = append(res.Steps, SetupStepResult{ID: step.ID, Title: step.Title})
			return fail(step.ID, step.Title+": the check could not complete; retry")
		}
		out := report(step, r)
		if !r.OK && len(out.Links) == 0 && step.Prepare != nil {
			if prep, e := call(func(c context.Context, st pluginapi.SetupState, _ json.RawMessage, env pluginapi.Env) (pluginapi.StepResult, error) {
				return step.Prepare(c, st, env)
			}, nil); e == nil {
				out.Links = report(step, prep).Links
			}
		}
		res.Steps = append(res.Steps, out)
		if !r.OK {
			msg := step.Title + " failed"
			if len(out.Messages) > 0 {
				msg += ": " + out.Messages[0]
			}
			for k, v := range out.Errors {
				msg += "; " + k + ": " + v
			}
			return fail(step.ID, msg)
		}
		z := wizard{Data: state, Credentials: creds}
		if e := z.merge(r.Data); e != nil {
			return fail(step.ID, "unsafe setup result")
		}
	}
	res.Credentials = map[string]string{}
	keys := []string{}
	for k := range creds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		res.Credentials[k] = "from $" + credSource[k]
	}
	res.Settings = map[string]any{}
	for k, v := range state {
		var value any
		_ = json.Unmarshal(v, &value)
		res.Settings[k] = value
	}
	if o.DryRun {
		res.OK = true
		return res
	}
	storage := o.Storage
	if storage == "" {
		storage = filepath.Join(StorageRoot(o.Home, o.Mode), o.Workspace)
	}
	if o.Mode == "server" && !confined(StorageRoot(o.Home, o.Mode), storage) {
		return fail("", "--storage must be inside the server data root "+StorageRoot(o.Home, o.Mode))
	}
	draft := Draft{ID: o.Workspace, Storage: storage, Data: state, Credentials: creds}
	entry, e := BuildEntry(ctx, p, draft)
	if e != nil {
		return fail("", "workspace settings are invalid")
	}
	if e := SaveEntry(o.Config, o.Secrets, p, draft, entry); e != nil {
		return fail("", "could not save workspace: check configuration, folder permissions and the secret store")
	}
	for k := range creds {
		res.Credentials[k] = "stored as " + entry[k+"_env"].(string)
	}
	res.OK, res.Workspace, res.Config, res.Storage = true, o.Workspace, o.Config, storage
	return res
}

// redactStep converts a hook result for JSON output without credential values.
func redactStep(step pluginapi.SetupStep, r pluginapi.StepResult, creds pluginapi.Credentials) SetupStepResult {
	redact := func(s string) string {
		for _, v := range creds {
			if v != "" {
				s = strings.ReplaceAll(s, v, "[redacted]")
			}
		}
		return s
	}
	out := SetupStepResult{ID: step.ID, Title: step.Title, OK: r.OK}
	for _, m := range r.Messages {
		out.Messages = append(out.Messages, redact(m))
	}
	for k, v := range r.FieldErrors {
		if out.Errors == nil {
			out.Errors = map[string]string{}
		}
		out.Errors[k] = redact(v)
	}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, SetupCheck{Label: redact(c.Label), Detail: redact(c.Detail), OK: c.OK, Optional: c.Optional})
	}
	for _, l := range mergeLinks(step.Links, r.Links) {
		out.Links = append(out.Links, SetupLink{Label: l.Label, URL: l.URL})
	}
	return out
}

// typed converts a flag value by JSON schema type; arrays are comma-separated.
func typed(kind, v string) (any, error) {
	switch kind {
	case "array":
		out := []string{}
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case "boolean":
		return strconv.ParseBool(v)
	case "integer", "number":
		n, e := strconv.ParseFloat(v, 64)
		if e != nil {
			return nil, errors.New("not a number")
		}
		return n, nil
	}
	return v, nil
}

// mapChoices lets list values name a choice by its label (with or without a
// leading # and ignoring any " — hint" suffix) as well as by its id.
func mapChoices(input map[string]any, choices []pluginapi.Choice) {
	norm := func(s string) string {
		s, _, _ = strings.Cut(s, " — ")
		return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	}
	for k, v := range input {
		list, ok := v.([]string)
		if !ok {
			continue
		}
		for i, item := range list {
			for _, c := range choices {
				if c.ID == item {
					break
				}
				if norm(c.Label) == norm(item) {
					list[i] = c.ID
					break
				}
			}
		}
		input[k] = list
	}
}

// SetupHelp describes the setup flags of a plugin.
func SetupHelp(p pluginapi.Plugin) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Setup options for %s:\n", p.Manifest().ID)
	for _, f := range SetupFields(p) {
		arg := "VALUE"
		switch {
		case f.Secret:
			arg = "ENV_VAR"
		case f.Type == "array":
			arg = "A,B,..."
		}
		note := f.Title
		if f.Secret && f.EnvDefault != "" {
			note += " (default: $" + f.EnvDefault + " when set)"
		}
		if f.Default != "" {
			note += " (default: " + f.Default + ")"
		}
		if f.Optional {
			note += " [optional step " + f.Step + "; --skip " + f.Step + " to skip]"
		}
		fmt.Fprintf(&b, "  --%-28s %s\n", f.Flag+" "+arg, note)
	}
	return b.String()
}
