package slack

import (
	"strings"
	"testing"
)

// Every Web API method the archive path calls must be covered by a bot scope
// that the wizard's app manifest requests and setup verifies.
func TestManifestCoversArchiveScopes(t *testing.T) {
	scopes := map[string][]string{
		"auth.test":             nil,
		"chat.getPermalink":     nil,
		"conversations.history": {"channels:history", "groups:history"},
		"conversations.replies": {"channels:history", "groups:history"},
		"conversations.info":    {"channels:read", "groups:read"},
		"users.conversations":   {"channels:read", "groups:read"},
		"conversations.list":    {"channels:read", "groups:read"},
		"reactions.get":         {"reactions:read"},
		"users.info":            {"users:read"},
		"users.list":            {"users:read"},
		"bots.info":             {"users:read"},
		"emoji.list":            {"emoji:read"},
		"download":              {"files:read"},
	}
	required := map[string]bool{}
	for _, s := range requiredScopes {
		required[s] = true
	}
	for method := range readMethods {
		if _, ok := scopes[method]; !ok {
			t.Errorf("read method %s has no scope mapping", method)
		}
	}
	for method, need := range scopes {
		for _, scope := range need {
			if !strings.Contains(AppManifest, "      - "+scope+"\n") {
				t.Errorf("%s needs %s, which the manifest does not request", method, scope)
			}
			if !required[scope] && optionalScopes[scope] == "" {
				t.Errorf("%s needs %s, which setup does not verify", method, scope)
			}
		}
	}
}

func TestManifestCoversOwnPostCleanupScopes(t *testing.T) {
	for _, scope := range requiredScopes {
		if scope == "chat:write" {
			t.Error("archive-only setup must not require cleanup permission")
		}
	}
	for _, scope := range []string{"chat:write", "users:read", "channels:history", "groups:history"} {
		if !strings.Contains(AppManifest, "      - "+scope+"\n") {
			t.Errorf("own-post cleanup needs %s, which the manifest does not request", scope)
		}
	}
	if optionalScopes["chat:write"] == "" {
		t.Error("archive-only setup must explain the optional cleanup permission")
	}
}
