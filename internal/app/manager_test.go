package app

import (
	"context"
	"errors"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCaptureSuspensionWaitsForRuntimeDrain(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(p, []byte("workspaces: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(home, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), p, nil, nil)
	defer m.Close()
	stopped, drained := make(chan struct{}), make(chan struct{})
	m.running["fixture"] = &Running{Store: store, cancel: func() { close(stopped) }, done: drained}
	returned := make(chan error, 1)
	go func() {
		resume, err := m.SuspendCapture("fixture")
		if err == nil {
			resume()
		}
		returned <- err
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("capture not canceled")
	}
	select {
	case err := <-returned:
		t.Fatalf("returned before capture drained: %v", err)
	default:
	}
	close(drained)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("suspension did not finish after drain")
	}
}
