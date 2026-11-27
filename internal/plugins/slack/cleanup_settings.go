package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/url"
	"sort"
	"strings"
)

const cleanupInstructions = `1. Open OAuth & Permissions using the setup link below.
2. Add the permissions below, reinstall the app, and copy its token.
3. Paste the token below and click Test, then save.

Details: A bot token (xoxb-) deletes only its own posts. For other people's posts, use an admin/owner User OAuth Token (xoxp-) and enable “Delete messages posted by others”. Under User Token Scopes add chat:write, users:read, channels:history and groups:history. For own bot posts, add these under Bot Token Scopes. History scopes compare complete live threads; users:read verifies identity. Reinstall after changing scopes. Workspace permissions still apply. Credentials stay in the credential store and are never displayed.`

func (Plugin) CleanupSetup(ctx context.Context, w pluginapi.Workspace, env pluginapi.Env) (pluginapi.CleanupSetup, error) {
	out := pluginapi.CleanupSetup{Title: "Delete from Slack after archiving", WizardTitle: "Clean up Slack (optional)", Instructions: strings.Split(cleanupInstructions, "\n\nDetails: ")[0], Details: strings.Split(cleanupInstructions, "\n\nDetails: ")[1]}
	var raw map[string]any
	_ = json.Unmarshal(w.Settings, &raw)
	c := NewClient(env.Credentials["bot_token"], env.HTTP)
	team, app := str(raw["team_id"]), str(raw["app_id"])
	if !appPattern.MatchString(app) {
		if me, e := c.Call(ctx, "auth.test", nil); e == nil && me["team_id"] == team {
			if b := str(me["bot_id"]); b != "" {
				if doc, e := c.Call(ctx, "bots.info", url.Values{"bot": {b}}); e == nil {
					bot, _ := doc["bot"].(map[string]any)
					app = str(bot["app_id"])
				}
			}
		}
	}
	if id(team, "team") && appPattern.MatchString(app) {
		out.OAuthURL = "https://app.slack.com/app-settings/" + team + "/" + app + "/oauth"
	}
	reactions, _ := reactionChoices(ctx, nil, env)
	out.Reactions = reactions.Choices
	cursor := ""
	seen := map[string]bool{}
	for {
		doc, e := c.Call(ctx, "users.list", url.Values{"limit": {"200"}, "cursor": {cursor}})
		if e != nil {
			return out, errors.New("cannot load workspace members. Add users:read to Bot Token Scopes and reinstall; then retry")
		}
		members, _ := doc["members"].([]any)
		for _, v := range members {
			u, _ := v.(map[string]any)
			uid := str(u["id"])
			if !id(uid, "user") || u["deleted"] == true {
				continue
			}
			profile, _ := u["profile"].(map[string]any)
			name := str(profile["display_name"])
			if name == "" {
				name = str(u["real_name"])
			}
			if name == "" {
				name = str(u["name"])
			}
			out.Members = append(out.Members, pluginapi.Choice{ID: uid, Label: name + " (@" + str(u["name"]) + ")"})
		}
		meta, _ := doc["response_metadata"].(map[string]any)
		cursor = str(meta["next_cursor"])
		if cursor == "" {
			break
		}
		if seen[cursor] {
			return out, errors.New("member pagination repeated; retry")
		}
		seen[cursor] = true
	}
	sort.Slice(out.Members, func(i, j int) bool { return out.Members[i].Label < out.Members[j].Label })
	return out, nil
}
func (Plugin) TestCleanupCredential(ctx context.Context, w pluginapi.Workspace, env pluginapi.Env, token string, others bool) error {
	if !validToken(token, "xoxp-") && !validToken(token, "xoxb-") {
		return errors.New("paste a User OAuth Token (xoxp-), or a bot token (xoxb-) for own posts only")
	}
	if others && !validToken(token, "xoxp-") {
		return errors.New("deleting others requires an admin/owner user token (xoxp-); disable Delete messages posted by others for bot-only cleanup")
	}
	c := NewClient(token, env.HTTP)
	me, scopes, e := authTest(ctx, c, token)
	if e != nil {
		var api apiError
		if errors.As(e, &api) {
			return fmt.Errorf("slack rejected the credential (%s). Reinstall the app in this workspace and copy a fresh OAuth token", api.code)
		}
		return errors.New("cannot reach Slack to test the credential. Check your connection and retry")
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Settings, &raw)
	if me["team_id"] != raw["team_id"] || !id(me["user_id"], "user") {
		return errors.New("credential belongs to a different workspace; reinstall in this workspace and copy its OAuth token")
	}
	section := "User Token Scopes"
	if strings.HasPrefix(token, "xoxb-") {
		section = "Bot Token Scopes"
	}
	required := []string{"chat:write", "users:read", "channels:history", "groups:history"}
	missing := []string{}
	for _, s := range required {
		if !scopes[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing scopes: %s. Add them under OAuth & Permissions > %s, reinstall the app, and paste the new token", strings.Join(missing, ", "), section)
	}
	doc, e := c.Call(ctx, "users.info", url.Values{"user": {str(me["user_id"])}})
	if e != nil {
		return errors.New("cannot verify credential owner. Add users:read, reinstall, and retry")
	}
	u, _ := doc["user"].(map[string]any)
	if u["id"] != me["user_id"] {
		return errors.New("credential owner identity mismatch; copy a fresh token")
	}
	if others && u["is_admin"] != true && u["is_owner"] != true {
		return errors.New("deleting others requires an admin/owner user token; ask your workspace admin/owner to authorize and copy their User OAuth Token")
	}
	return nil
}
