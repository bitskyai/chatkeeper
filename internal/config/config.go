// Package config loads strict YAML with neutral keys and registered plugin schemas.
// Credentials are resolved per workspace and are never part of serializable settings.
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"go.yaml.in/yaml/v3"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrConfig = errors.New("invalid configuration")

type Entry struct {
	Workspace   pluginapi.Workspace
	Plugin      pluginapi.Plugin
	Credentials pluginapi.Credentials
}
type Config struct {
	Workspaces []Entry
	Host       string
	Port       int
	Token      pluginapi.Credentials
	TokenEnv   string
}

func (c *Config) Address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

var wsID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var envID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func mapping(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
func keys(m map[string]any, allowed ...string) bool {
	set := map[string]bool{}
	for _, k := range allowed {
		set[k] = true
	}
	for k := range m {
		if !set[k] {
			return false
		}
	}
	return true
}
func text(v any) string { s, _ := v.(string); return s }
func stringList(v any) ([]string, bool) {
	a, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range a {
		s, ok := v.(string)
		if !ok || s == "" || seen[s] {
			return nil, false
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, true
}
func seconds(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, !math.IsInf(x, 0) && !math.IsNaN(x)
	}
	return 0, false
}
func canonicalPath(p string) (string, error) {
	absolute, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	resolved, e := filepath.EvalSymlinks(absolute)
	if e == nil {
		return resolved, nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", e
	}
	resolved, e = canonicalPath(parent)
	if e != nil {
		return "", e
	}
	return filepath.Join(resolved, filepath.Base(absolute)), nil
}
func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}
func Load(name string, requireSecrets bool) (*Config, error) {
	return loadConfig(name, requireSecrets, os.Getenv, true)
}
func LoadWithResolver(name string, requireSecrets bool, resolve func(string) string) (*Config, error) {
	return loadConfig(name, requireSecrets, resolve, false)
}
func loadConfig(name string, requireSecrets bool, resolve func(string) string, requirePublicToken bool) (*Config, error) {
	f, e := os.Open(name)
	if e != nil {
		return nil, ErrConfig
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, ErrConfig
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	var doc map[string]any
	if d.Decode(&doc) != nil || doc == nil || !keys(doc, "server", "workspaces") {
		return nil, ErrConfig
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, ErrConfig
	}
	c := &Config{Host: "127.0.0.1", Port: 8787, Token: pluginapi.Credentials{}}
	server := map[string]any{}
	if v, has := doc["server"]; has {
		var ok bool
		server, ok = mapping(v)
		if !ok || !keys(server, "host", "port", "token_env") {
			return nil, ErrConfig
		}
	}
	if v, has := server["host"]; has {
		c.Host = text(v)
		if c.Host == "" {
			return nil, ErrConfig
		}
	}
	if v, has := server["port"]; has {
		n, ok := v.(int)
		if !ok || n < 1 || n > 65535 {
			return nil, ErrConfig
		}
		c.Port = n
	}
	tokenEnv := "CHATKEEPER_API_TOKEN"
	if v, has := server["token_env"]; has {
		tokenEnv = text(v)
		if !envID.MatchString(tokenEnv) {
			return nil, ErrConfig
		}
	}
	c.TokenEnv = tokenEnv
	if requireSecrets {
		token := os.Getenv(tokenEnv)
		if token == "" && tokenEnv == "CHATKEEPER_API_TOKEN" {
			token = os.Getenv("THREADKEEP_API_TOKEN")
		}
		if token != "" && !regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`).MatchString(token) {
			return nil, ErrConfig
		}
		c.Token["bearer"] = token
		if requirePublicToken && token == "" && !Loopback(c.Host) {
			return nil, ErrConfig
		}
	}
	items, ok := doc["workspaces"].([]any)
	if !ok || len(items) > 100 {
		return nil, ErrConfig
	}
	ids := map[string]bool{}
	byPlugin := map[string][]pluginapi.Settings{}
	for _, item := range items {
		raw, ok := mapping(item)
		if !ok {
			return nil, ErrConfig
		}
		wid := text(raw["id"])
		if !wsID.MatchString(wid) || ids[wid] || wid == "api" || wid == "healthz" || wid == "mcp" || wid == "view" {
			return nil, ErrConfig
		}
		ids[wid] = true
		platform := text(raw["platform"])
		var p pluginapi.Plugin
		if platform == "" {
			p, ok = pluginapi.Legacy()
		} else {
			p, ok = pluginapi.Lookup(platform)
		}
		if !ok {
			return nil, ErrConfig
		}
		manifest, e := core.Call(context.Background(), 10*time.Second, func(context.Context) (pluginapi.Manifest, error) { return p.Manifest(), nil })
		if e != nil || manifest.APIVersion != pluginapi.APIVersion {
			return nil, ErrConfig
		}
		platform = manifest.ID
		schema, e := core.Call(context.Background(), 10*time.Second, func(context.Context) (json.RawMessage, error) { return p.ConfigSchema(), nil })
		if e != nil {
			return nil, ErrConfig
		}
		credSchema, e := core.Call(context.Background(), 10*time.Second, func(context.Context) (json.RawMessage, error) { return p.CredentialSchema(), nil })
		if e != nil {
			return nil, ErrConfig
		}
		var sch, cs struct {
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		}
		if json.Unmarshal(schema, &sch) != nil || json.Unmarshal(credSchema, &cs) != nil {
			return nil, ErrConfig
		}
		allowed := []string{"id", "platform", "channels", "storage", "trigger", "poll_interval", "cleanup"}
		for key := range sch.Properties {
			allowed = append(allowed, key)
		}
		for key := range cs.Properties {
			allowed = append(allowed, key+"_env")
		}
		if !keys(raw, allowed...) {
			return nil, ErrConfig
		}
		ws := pluginapi.Workspace{ID: wid, Platform: platform, PollInterval: 120 * time.Second, Cleanup: pluginapi.CleanupPolicy{MinAge: 24 * time.Hour, TokenEnv: "CHATKEEPER_CLEANUP_TOKEN"}, AutoChannels: true, Trigger: pluginapi.Trigger{Reactions: append([]string{}, manifest.DefaultReactions...), Who: "any"}}
		if v, has := raw["channels"]; has && v != "auto" {
			chs, ok := stringList(v)
			if !ok || len(chs) == 0 {
				return nil, ErrConfig
			}
			ws.AutoChannels = false
			ws.Channels = chs
		}
		if v, has := raw["poll_interval"]; has {
			n, ok := seconds(v)
			if !ok || n <= 0 || n > float64(math.MaxInt64)/float64(time.Second) {
				return nil, ErrConfig
			}
			ws.PollInterval = time.Duration(n * float64(time.Second))
		}
		if v, has := raw["trigger"]; has {
			t, ok := mapping(v)
			if !ok || !keys(t, "reactions", "who", "owners", "match") {
				return nil, ErrConfig
			}
			if v, has := t["reactions"]; has {
				ws.Trigger.Reactions, ok = stringList(v)
				if !ok || len(ws.Trigger.Reactions) == 0 {
					return nil, ErrConfig
				}
			}
			if v, has := t["who"]; has {
				ws.Trigger.Who = text(v)
			}
			if ws.Trigger.Who != "any" && ws.Trigger.Who != "owner" {
				return nil, ErrConfig
			}
			if v, has := t["match"]; has {
				ws.Trigger.Match = text(v)
				if ws.Trigger.Match != "any" && ws.Trigger.Match != "all" {
					return nil, ErrConfig
				}
			}
			if v, has := t["owners"]; has {
				ws.Trigger.Owners, ok = stringList(v)
				if !ok {
					return nil, ErrConfig
				}
			}
			if ws.Trigger.Who == "owner" && len(ws.Trigger.Owners) == 0 {
				return nil, ErrConfig
			}
		}
		store, ok := mapping(raw["storage"])
		if !ok || !keys(store, "type", "path") {
			return nil, ErrConfig
		}
		if kind, has := store["type"]; has && kind != "folder" {
			return nil, ErrConfig
		}
		location := text(store["path"])
		if location == "" {
			return nil, ErrConfig
		}
		if !filepath.IsAbs(location) {
			location = filepath.Join(filepath.Dir(name), location)
		}
		location, e = canonicalPath(location)
		if e != nil {
			return nil, ErrConfig
		}
		for _, prior := range c.Workspaces {
			if overlap(prior.Workspace.Storage, location) {
				return nil, ErrConfig
			}
		}
		ws.Storage = location
		if v, has := raw["cleanup"]; has {
			cleanup, ok := mapping(v)
			if !ok || !keys(cleanup, "enabled", "token_env", "allow_others", "min_age_hours", "buffer_max_age_seconds", "own_only", "allow_zero_age", "approval", "scope", "every_hours") {
				return nil, ErrConfig
			}
			if v, has := cleanup["approval"]; has {
				b, _ := json.Marshal(v)
				var t pluginapi.Trigger
				var fields map[string]any
				if json.Unmarshal(b, &t) != nil || json.Unmarshal(b, &fields) != nil || !keys(fields, "who", "owners", "reactions", "match") || t.Who != "owner" || t.Match != "" && t.Match != "any" && t.Match != "all" {
					return nil, ErrConfig
				}
				ws.Cleanup.Approval = &t
			}
			if v, has := cleanup["scope"]; has {
				if v != "thread" && v != "messages" {
					return nil, ErrConfig
				}
				ws.Cleanup.Scope = text(v)
			}
			if v, has := cleanup["every_hours"]; has {
				n, ok := seconds(v)
				if !ok || n < 0 || n > 1000000 || n > 0 && n < 1 {
					return nil, ErrConfig
				}
				ws.Cleanup.Every = time.Duration(n * float64(time.Hour))
			}
			for _, k := range []string{"enabled", "allow_others", "own_only", "allow_zero_age"} {
				if v, has := cleanup[k]; has {
					b, ok := v.(bool)
					if !ok {
						return nil, ErrConfig
					}
					switch k {
					case "enabled":
						ws.Cleanup.Enabled = b
					case "allow_others":
						ws.Cleanup.AllowOthers = b
					case "own_only":
						ws.Cleanup.OwnOnly = b
					}
				}
			}
			for _, k := range []string{"min_age_hours", "buffer_max_age_seconds"} {
				if v, has := cleanup[k]; has {
					n, ok := seconds(v)
					if !ok || n < 0 || n > 1000000 || n == 0 && (k == "buffer_max_age_seconds" || cleanup["allow_zero_age"] != true) {
						return nil, ErrConfig
					}
					if k == "min_age_hours" {
						ws.Cleanup.MinAge = time.Duration(n * float64(time.Hour))
						if ws.Cleanup.MinAge == 0 && cleanup["allow_zero_age"] != true {
							return nil, ErrConfig
						}
					}
				}
			}
			if v, has := cleanup["token_env"]; has {
				if !envID.MatchString(text(v)) {
					return nil, ErrConfig
				}
				ws.Cleanup.TokenEnv = text(v)
			}
		}
		creds := pluginapi.Credentials{}

		for k, property := range cs.Properties {
			env := text(property["x-env-default"])
			if v, has := raw[k+"_env"]; has {
				env = text(v)
			}
			if !envID.MatchString(env) {
				return nil, ErrConfig
			}
			if requireSecrets {
				creds[k] = resolve(env)
			}
		}
		if requireSecrets {
			for _, required := range cs.Required {
				if creds[required] == "" {
					return nil, ErrConfig
				}
			}
		}
		// Credential references remain settings; secret values do not.
		encoded, e := json.Marshal(raw)
		if e != nil {
			return nil, ErrConfig
		}
		settings, e := core.Call(context.Background(), 10*time.Second, func(ctx context.Context) (pluginapi.Settings, error) { return p.ParseWorkspace(ctx, encoded, creds) })
		if e != nil {
			return nil, ErrConfig
		}
		ws.Settings = settings
		byPlugin[platform] = append(byPlugin[platform], settings)
		c.Workspaces = append(c.Workspaces, Entry{Workspace: ws, Plugin: p, Credentials: creds})
	}
	for id, settings := range byPlugin {
		p, _ := pluginapi.Lookup(id)
		if core.Guard(context.Background(), 10*time.Second, func(ctx context.Context) error { return p.ValidateWorkspaces(ctx, settings) }) != nil {
			return nil, ErrConfig
		}
	}
	return c, nil
}

// Loopback accepts only literal loopback addresses; DNS names can resolve elsewhere.
func Loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
