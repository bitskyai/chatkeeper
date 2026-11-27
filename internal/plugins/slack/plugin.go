// Package slack owns all source-specific validation and transport behavior.
package slack

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"regexp"
	"strings"
)

func init() { pluginapi.Register(Plugin{}) }

type Plugin struct{}

func (Plugin) Manifest() pluginapi.Manifest {
	return pluginapi.Manifest{ID: "slack", Name: "Slack", Version: "0.1.0", APIVersion: 1, Capabilities: pluginapi.Capabilities{ReadHistory: true, Realtime: true, Delete: true}, ClaimsLegacy: true, DefaultReactions: []string{"white_check_mark"}}
}
func (Plugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"team_id":{"type":"string","pattern":"^T[A-Z0-9]+$"},"app_id":{"type":"string","pattern":"^A[A-Z0-9]+$"}},"required":["team_id"]}`)
}
func (Plugin) CredentialSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"bot_token":{"type":"string","x-secret":true,"x-env-default":"SLACK_BOT_TOKEN"},"app_token":{"type":"string","x-secret":true,"x-env-default":"SLACK_APP_TOKEN"}},"required":["bot_token"]}`)
}
func (Plugin) Content() pluginapi.ContentHooks { return Hooks{} }
func validToken(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) && len(s) > len(prefix) && !strings.ContainsAny(s, " \t\r\n")
}
func (Plugin) ParseWorkspace(_ context.Context, raw json.RawMessage, creds pluginapi.Credentials) (pluginapi.Settings, error) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || !id(m["team_id"], "team") {
		return nil, pluginapi.ErrInvalid
	}
	if v, has := m["app_id"]; has && !appPattern.MatchString(str(v)) {
		return nil, pluginapi.ErrInvalid
	}
	if len(creds) > 0 && (!validToken(creds["bot_token"], "xoxb-") || creds["app_token"] != "" && !validToken(creds["app_token"], "xapp-")) {
		return nil, pluginapi.ErrInvalid
	}
	if channels, has := m["channels"]; has && channels != "auto" {
		a, ok := channels.([]any)
		if !ok || len(a) == 0 {
			return nil, pluginapi.ErrInvalid
		}
		for _, ch := range a {
			if !id(ch, "channel") {
				return nil, pluginapi.ErrInvalid
			}
		}
	}
	if raw, has := m["trigger"]; has {
		t, ok := raw.(map[string]any)
		if !ok {
			return nil, pluginapi.ErrInvalid
		}
		if raw, has := t["reactions"]; has {
			a, ok := raw.([]any)
			if !ok || len(a) == 0 {
				return nil, pluginapi.ErrInvalid
			}
			for _, r := range a {
				if !reactionPattern.MatchString(str(r)) {
					return nil, pluginapi.ErrInvalid
				}
			}
		}
		if raw, has := t["match"]; has && raw != "any" && raw != "all" {
			return nil, pluginapi.ErrInvalid
		}
		if raw, has := t["owners"]; has {
			a, ok := raw.([]any)
			if !ok {
				return nil, pluginapi.ErrInvalid
			}
			for _, u := range a {
				if !id(u, "user") {
					return nil, pluginapi.ErrInvalid
				}
			}
		}
	}
	return pluginapi.Settings(append([]byte{}, raw...)), nil
}

// reactionPattern accepts emoji short names, optionally with a skin tone
// ("+1::skin-tone-3" matches only that tone; "+1" matches every tone).
var reactionPattern = regexp.MustCompile(`^[a-zA-Z0-9_+\-]{1,100}(::skin-tone-[2-6])?$`)

func patternsCode() *regexp.Regexp                                            { return regexp.MustCompile(`^[a-z_]{1,64}$`) }
func (Plugin) ValidateWorkspaces(context.Context, []pluginapi.Settings) error { return nil }
func (Plugin) NewSession(_ context.Context, ws pluginapi.Workspace, env pluginapi.Env) (pluginapi.Session, error) {
	var m map[string]any
	if json.Unmarshal(ws.Settings, &m) != nil || !id(m["team_id"], "team") || !validToken(env.Credentials["bot_token"], "xoxb-") {
		return nil, pluginapi.ErrInvalid
	}
	triggers := map[string]bool{}
	for _, name := range ws.Trigger.Reactions {
		triggers[baseReaction(name)] = true
	}
	client := NewClient(env.Credentials["bot_token"], env.HTTP)
	client.log = pluginapi.RedactingLogger(env.Log, env.Credentials)
	return &Session{client: client, team: str(m["team_id"]), appToken: env.Credentials["app_token"], triggers: triggers}, nil
}
