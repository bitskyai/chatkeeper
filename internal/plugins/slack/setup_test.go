package slack_test

import (
	"context"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"strings"
	"testing"
)

func TestSetupValidation(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "scope"}[missing], func(t *testing.T) {
			f := slacktest.New(t)
			if missing {
				f.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-OAuth-Scopes", "channels:read")
					_, _ = w.Write([]byte(`{"ok":true,"team_id":"TFIXTURE","user_id":"UBOT"}`))
				}
			}
			steps := slack.Plugin{}.SetupFlow()
			if len(steps) != 6 {
				t.Fatalf("no wizard: %d", len(steps))
			}
			res, e := steps[2].Run(context.Background(), nil, nil, pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "xoxb-synthetic"}, HTTP: f.Client()})
			if e != nil {
				t.Fatal(e)
			}
			if missing {
				if res.OK || !strings.Contains(strings.Join(res.Messages, " "), "files:read") {
					t.Fatal(res)
				}
			} else {
				if !res.OK || !strings.Contains(strings.Join(res.Messages, " "), "polling") {
					t.Fatal(res)
				}
			}
		})
	}
}
