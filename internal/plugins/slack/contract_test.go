package slack_test

import (
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi/plugintest"
	"testing"
)

func TestPluginContract(t *testing.T) {
	fake := slacktest.New(t)
	plugintest.Check(t, slack.Plugin{}, pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFIXTURE"}`)}, pluginapi.Env{HTTP: fake.Client(), Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic", "app_token": "xapp-synthetic"}})
}
