//go:build !noslack

package main

import (
	"bytes"
	"github.com/bitskyai/chatkeeper/internal/cleanup"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupOfflineCLI(t *testing.T) {
	home := t.TempDir()
	name := filepath.Join(home, "config.yaml")
	os.Mkdir(filepath.Join(home, "archive"), 0700)
	os.WriteFile(name, []byte("workspaces:\n  - id: fixture\n    team_id: TFAKE\n    channels: [CFAKE]\n    storage: {type: folder, path: archive}\n"), 0600)
	var out bytes.Buffer
	e := execute([]string{"cleanup", "--workspace", "fixture", "--config", name}, &out)
	if e != nil || !strings.Contains(out.String(), "cleanup disabled") {
		t.Fatalf("%s %v", out.String(), e)
	}
}

func TestCleanupApplyWorkspaceFailureExit(t *testing.T) {
	var out bytes.Buffer
	if e := writeCleanupReport(&out, cleanup.Report{Workspace: "fixture", Reason: "cannot enumerate archive"}, true); e == nil {
		t.Fatal("apply workspace failure returned success")
	}
}
