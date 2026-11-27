//go:build !noslack

package app_test

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/app"
	"github.com/bitskyai/chatkeeper/internal/config"
	_ "github.com/bitskyai/chatkeeper/internal/plugins/all"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkspaceDiffAndSecretRotation(t *testing.T) {
	home := t.TempDir()
	s := &secrets.Store{Home: home}
	for _, k := range []string{"BOT_A", "BOT_B"} {
		if e := s.Set(k, "xoxb-synthetic"); e != nil {
			t.Fatal(e)
		}
	}
	p := filepath.Join(home, "config.yaml")
	write := func(ids ...string) {
		t.Helper()
		data := "workspaces:\n"
		if len(ids) == 0 {
			data = "workspaces: []\n"
		}
		for _, id := range ids {
			data += fmt.Sprintf("- id: %s\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: BOT_%s\n  storage: {path: %q}\n", id, map[string]string{"a": "A", "b": "B"}[id], filepath.Join(home, id))
		}
		if e := os.WriteFile(p, []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	fake := slacktest.New(t)
	m := app.New(context.Background(), p, s, fake.Client())
	defer m.Close()
	reload := func() {
		t.Helper()
		if e := m.Reload(); e != nil {
			t.Fatal(e)
		}
	}
	runtime := func(id string) *app.Running {
		for _, r := range m.List() {
			if r.Entry.Workspace.ID == id {
				return r
			}
		}
		return nil
	}
	write("a")
	reload()
	a := runtime("a").Runtime
	write("a", "b")
	reload()
	if runtime("a").Runtime != a || len(m.List()) != 2 {
		t.Fatal("addition restarted unchanged workspace")
	}
	b := runtime("b").Runtime
	if e := config.Update(p, "a", map[string]any{"id": "a", "platform": "slack", "team_id": "TFIXTURE", "bot_token_env": "BOT_A", "storage": map[string]any{"path": filepath.Join(home, "a")}, "poll_interval": 60}); e != nil {
		t.Fatal(e)
	}
	reload()
	if runtime("a").Runtime == a || runtime("b").Runtime != b {
		t.Fatal("settings change did not isolate restart")
	}
	a = runtime("a").Runtime
	if e := s.Set("BOT_A", "xoxb-rotated"); e != nil {
		t.Fatal(e)
	}
	reload()
	if runtime("a").Runtime == a || runtime("b").Runtime != b {
		t.Fatal("rotation did not isolate restart")
	}
	a = runtime("a").Runtime
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, append(raw, []byte("unknown: true\n")...), 0600)
	if m.Reload() == nil || runtime("a").Runtime != a {
		t.Fatal("invalid config replaced running set")
	}
	write("b")
	reload()
	if len(m.List()) != 1 || runtime("b").Runtime != b {
		t.Fatal("removal restarted unchanged workspace")
	}
	if _, e := os.Stat(filepath.Join(home, "a", "archive")); e != nil {
		t.Fatal("removal deleted archives")
	}
	saved, _ := os.ReadFile(p)
	os.WriteFile(p, []byte("invalid: true\n"), 0600)
	if m.Pause("b") == nil {
		t.Fatal("invalid config accepted during pause")
	}
	if runtime("b").Paused {
		t.Fatal("failed pause changed runtime")
	}
	os.WriteFile(p, saved, 0600)
	if e := m.Pause("b"); e != nil {
		t.Fatal(e)
	}
	if !runtime("b").Paused {
		t.Fatal("pause failed")
	}
	write()
	reload()
	if len(m.List()) != 0 {
		t.Fatal("last workspace not removed")
	}
}
func TestFileWatcherFollowsSymlinkSwap(t *testing.T) {
	home := t.TempDir()
	first := filepath.Join(home, "first.yaml")
	second := filepath.Join(home, "second.yaml")
	p := filepath.Join(home, "config.yaml")
	for _, path := range []string{first, second} {
		os.WriteFile(path, []byte("workspaces: []\n"), 0600)
	}
	if e := os.Symlink(first, p); e != nil {
		t.Skip(e)
	}
	m := app.New(context.Background(), p, nil, nil)
	defer m.Close()
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Watch(ctx)
	os.WriteFile(second, []byte("unknown: true\n"), 0600)
	os.Remove(p)
	os.Symlink(second, p)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if m.Failure() != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher did not follow swapped config symlink")
}

type countedSecrets struct{ reads atomic.Int32 }

func (s *countedSecrets) Get(k string) (string, string, error) {
	s.reads.Add(1)
	if k != "SYNTHETIC_BOT" {
		return "", "fake", nil
	}
	return "xoxb-synthetic", "fake", nil
}
func TestWatcherDoesNotReadUnchangedSecrets(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(home, "archive"))
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	s := &countedSecrets{}
	fake := slacktest.New(t)
	m := app.New(context.Background(), p, s, fake.Client())
	defer m.Close()
	m.SetPaused("fixture", true)
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	before := s.reads.Load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Watch(ctx); close(done) }()
	time.Sleep(4500 * time.Millisecond)
	cancel()
	<-done
	if s.reads.Load() != before {
		t.Fatal("unchanged watcher read credentials", s.reads.Load()-before)
	}
}

func TestCaptureSuspensionPreservesNestedAndUserPause(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(home, "archive"))
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	fake := slacktest.New(t)
	m := app.New(context.Background(), p, &countedSecrets{}, fake.Client())
	defer m.Close()
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	before := m.List()[0]
	resume1, e := m.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	if m.List()[0].Paused || m.IsPaused("fixture") {
		t.Fatal("cleanup leaked into user pause")
	}
	if !m.List()[0].Suspended {
		t.Fatal("capture still running during cleanup")
	}
	if m.List()[0].Runtime == before.Runtime {
		t.Fatal("capture runtime was not drained")
	}
	resume2, e := m.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	resume1()
	resume1()
	if !m.List()[0].Suspended {
		t.Fatal("nested cleanup resumed capture early")
	}
	m.SetPaused("fixture", true)
	resume2()
	if !m.List()[0].Paused {
		t.Fatal("cleanup erased explicit user pause")
	}
	if e := m.Pause("fixture"); e != nil {
		t.Fatal(e)
	}
	if m.List()[0].Paused {
		t.Fatal("user could not resume after cleanup")
	}
	resume3, e := m.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	resume3()
	if m.List()[0].Paused {
		t.Fatal("cleanup did not restore active capture")
	}
}

func TestCaptureSuspensionBeforeRuntimeStarts(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(home, "archive"))
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	fake := slacktest.New(t)
	m := app.New(context.Background(), p, &countedSecrets{}, fake.Client())
	defer m.Close()
	resume, e := m.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	if !m.List()[0].Suspended {
		t.Fatal("startup bypassed active cleanup lease")
	}
	resume()
	if m.List()[0].Paused {
		t.Fatal("startup remained paused after cleanup")
	}
}

// Failed secret resolution must keep retrying without a file change.
func TestResumeRetriesRepeatedFailures(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  bot_token_env: SYNTHETIC_BOT\n  storage: {path: %q}\n", filepath.Join(home, "archive"))
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	s := &failingSecrets{}
	fake := slacktest.New(t)
	m := app.New(context.Background(), p, s, fake.Client())
	defer m.Close()
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	resume, e := m.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	s.fail.Store(true)
	resume()
	if m.Failure() == "" {
		t.Fatal("resume failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Watch(ctx)
	// Force at least two failed attempts, then recover without changing YAML.
	deadline := time.Now().Add(15 * time.Second)
	for s.failures.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.failures.Load() < 3 {
		t.Fatal("resume retry stopped after second failure")
	}
	s.fail.Store(false)
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r := m.List()[0]
		if !r.Suspended && !r.Paused && r.Runtime.Health().State == "running" && m.Failure() == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("resume never recovered", m.Failure())
}

type failingSecrets struct {
	fail     atomic.Bool
	failures atomic.Int32
}

func (s *failingSecrets) Get(k string) (string, string, error) {
	if s.fail.Load() {
		if k == "SYNTHETIC_BOT" {
			s.failures.Add(1)
		}
		return "", "", fmt.Errorf("synthetic resolution failure")
	}
	return (&countedSecrets{}).Get(k)
}

func TestReplyReactionDuringSuspensionRecoveredByFullBackfill(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.yaml")
	t.Setenv("SYNTHETIC_BOT", "xoxb-synthetic")
	t.Setenv("SYNTHETIC_APP", "xapp-synthetic")
	data := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  channels: [CFIXTURE]\n  bot_token_env: SYNTHETIC_BOT\n  app_token_env: SYNTHETIC_APP\n  trigger: {who: any, reactions: [bookmark]}\n  storage: {path: %q}\n", filepath.Join(home, "archive"))
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	fake := slacktest.New(t)
	m := app.New(context.Background(), p, nil, fake.Client())
	defer m.Close()
	if e := m.Reload(); e != nil {
		t.Fatal(e)
	}
	resume, e := m.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	fake.SetReaction("bookmark")
	fake.MarkReply(true)
	resume()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		keys, e := m.List()[0].Store.Keys(context.Background())
		if e == nil && len(keys) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reply reaction added during cleanup lost on resume")
}
