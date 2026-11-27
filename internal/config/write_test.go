package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdatePreservesComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("# hand written\nworkspaces: [] # keep this\n"), 0600)
	e := Update(p, "demo", map[string]any{"id": "demo", "platform": "example", "channels": "auto"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# hand written") || !strings.Contains(string(b), "# keep this") {
		t.Fatal(string(b))
	}
	if _, e = os.Stat(p + ".bak"); e != nil {
		t.Fatal(e)
	}
	if e = Update(p, "demo", nil); e != nil {
		t.Fatal(e)
	}
}
func TestUpdateKeepsNestedCommentsAndOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("workspaces:\n  - id: demo\n    # destination\n    storage:\n      path: old # backup this\n      type: folder\n    platform: example # source\n"), 0600)
	if e := Update(p, "demo", map[string]any{"id": "demo", "storage": map[string]any{"path": "new", "type": "folder"}, "platform": "example"}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	text := string(b)
	if !strings.Contains(text, "# destination") || !strings.Contains(text, "new # backup this") || strings.Index(text, "storage:") > strings.Index(text, "platform:") {
		t.Fatal(text)
	}
}
