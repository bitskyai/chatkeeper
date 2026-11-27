//go:build noslack

package all

import (
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"testing"
)

func TestExcludedRegistryHasOnlyPlanned(t *testing.T) {
	if plugins := pluginapi.Plugins(); len(plugins) != 2 {
		t.Fatalf("got %d plugins", len(plugins))
	}
	for _, p := range pluginapi.Plugins() {
		if len(p.SetupFlow()) != 0 {
			t.Fatal("excluded implementation registered")
		}
	}
}
