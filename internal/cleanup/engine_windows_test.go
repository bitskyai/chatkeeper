package cleanup

import (
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"testing"
)

func TestWindowsApplyFailsClosedBeforeStorageOrNetwork(t *testing.T) {
	r := Run(context.Background(), pluginapi.Workspace{Cleanup: pluginapi.CleanupPolicy{Enabled: true}}, nil, nil, true)
	if r.Reason == "" {
		t.Fatal("Windows apply did not fail closed")
	}
}
