package core

import (
	"bytes"
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"log/slog"
	"strings"
	"testing"
)

func TestUnchangedThreadHasPersistentResult(t *testing.T) {
	var out bytes.Buffer
	r := &Runtime{Workspace: pluginapi.Workspace{ID: "fixture"}, Env: pluginapi.Env{Log: slog.New(slog.NewJSONHandler(&out, nil))}, failures: map[pluginapi.Key]string{}}
	r.noteResult(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, false, nil)
	if !strings.Contains(out.String(), `"result":"unchanged"`) || !strings.Contains(out.String(), `"root":"100.000001"`) {
		t.Fatal("missing unchanged thread result", out.String())
	}
}
