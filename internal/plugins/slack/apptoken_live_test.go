//go:build live && !windows

package slack_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// TestRealAppTokenGuide checks the app-level token step against a real
// workspace: the deep link names the bot token's app, the real xapp- token's
// Socket Mode hello names the same app, and bad tokens get the right fix.
// Ids and tokens are compared, never logged.
func TestRealAppTokenGuide(t *testing.T) {
	bot, appTok := os.Getenv("SLACK_BOT_TOKEN"), os.Getenv("SLACK_APP_TOKEN")
	if bot == "" || appTok == "" {
		t.Skip("set SLACK_BOT_TOKEN and SLACK_APP_TOKEN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var step pluginapi.SetupStep
	for _, s := range (slack.Plugin{}).SetupFlow() {
		if s.ID == "app_token" {
			step = s
		}
	}
	env := pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": bot, "app_token": appTok}, HTTP: http.DefaultClient}
	prep, e := step.Prepare(ctx, nil, env)
	if e != nil || len(prep.Links) != 2 || !strings.HasSuffix(prep.Links[0].URL, "/general") {
		t.Fatal("app id not resolved from the bot token")
	}
	linked := strings.TrimSuffix(strings.TrimPrefix(prep.Links[0].URL, "https://api.slack.com/apps/"), "/general")
	res, e := step.Test(ctx, nil, nil, env)
	if e != nil || !res.OK {
		t.Fatal("real app-level token rejected", res.Messages)
	}
	same := res.Checks[len(res.Checks)-1]
	if same.Label != "Same Slack app" || !same.OK || same.Detail != linked {
		t.Fatal("deep link app differs from the app-level token's app")
	}
	t.Log("deep link app matches the app-level token's Socket Mode hello app")
	env.Credentials["app_token"] = "xapp-1-A000BOGUS-000-bogus"
	res, _ = step.Test(ctx, nil, nil, env)
	if res.OK || !strings.Contains(strings.Join(res.Messages, " "), "rejected the app-level token") || len(res.Links) != 2 {
		t.Fatal("bogus token fix", res.Messages)
	}
	t.Log("bogus token:", res.Messages[0])
	// A real token of the wrong type reaching Slack (the wizard blocks it earlier).
	if check, ok := slack.SocketCheck(ctx, slack.NewClient("", http.DefaultClient), bot); ok || !strings.Contains(check.Detail, "not an app-level token") {
		t.Fatal("wrong token type fix", check.Detail)
	}
	t.Log("bot token sent to apps.connections.open: not_allowed_token_type mapped")
	env.Credentials["app_token"] = bot
	if res, _ := step.Test(ctx, nil, nil, env); res.OK || res.FieldErrors["app_token"] == "" {
		t.Fatal("bot token pasted as app token not caught")
	}
}
