package app

import (
	"context"
	"errors"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/search"
	"os"
	"path/filepath"
	"testing"
)

func TestReloadInvalidRetainsConfiguration(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	os.WriteFile(p, []byte("workspaces: []\n"), 0600)
	m := New(context.Background(), p, nil, nil)
	defer m.Close()
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	if m.Generation() != 1 {
		t.Fatal("not loaded")
	}
	os.WriteFile(p, []byte("bad: true"), 0600)
	if m.Reload() == nil {
		t.Fatal("accepted invalid config")
	}
	if m.Generation() != 1 {
		t.Fatal("replaced valid configuration")
	}
	if e := config.Update(p, "demo", nil); e != nil {
		t.Fatal(e)
	}
}

func TestRetrySearchOwnershipAfterBootstrap(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if e := os.WriteFile(path, []byte("workspaces: []\n"), 0600); e != nil {
		t.Fatal(e)
	}
	bootstrap := search.New(home)
	defer bootstrap.Close()
	if e := bootstrap.Configure(context.Background(), map[string]search.Source{}); e != nil {
		t.Fatal(e)
	}
	m := New(context.Background(), path, nil, nil)
	m.Search = search.New(home)
	defer m.Close()
	if e := m.Reload(); !errors.Is(e, search.ErrLocked) {
		t.Fatal(e)
	}
	bootstrap.Close()
	m.reloadIfChanged()
	if m.Generation() != 1 || m.Failure() != "" {
		t.Fatal("unchanged config did not retry transient writer ownership", m.Generation(), m.Failure())
	}
}
