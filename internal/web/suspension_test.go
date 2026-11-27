//go:build !noslack && !windows

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/plugins/slack/slacktest"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/zalando/go-keyring"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSettingsDuringSuspensionPreserveUserChoiceAfterRestart(t *testing.T) {
	keyring.MockInit()
	t.Setenv("SLACK_APP_TOKEN", "")
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint(paused), func(t *testing.T) {
			t.Setenv("SLACK_BOT_TOKEN", "xoxb-synthetic")
			home := t.TempDir()
			fake := slacktest.New(t)
			cfg := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  channels: [CFIXTURE]\n  storage: {path: %q}\n", filepath.Join(home, "archive"))
			if e := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0600); e != nil {
				t.Fatal(e)
			}
			opts := Options{Home: home, Host: "127.0.0.1", Mode: "desktop", HTTP: fake.Client()}
			a, e := NewApp(opts)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { a.Close() }()
			resume, e := a.Manager.SuspendCapture("fixture")
			if e != nil {
				t.Fatal(e)
			}
			send := func(path string) *httptest.ResponseRecorder {
				v := url.Values{"workspace": {"fixture"}}
				if path == "/settings/credentials" {
					v.Set("app_token", "xapp-synthetic")
				}
				r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				a.settings(w, r, "csrf")
				return w
			}
			// Pause and unpause while the lease is held. Saving credentials and checking
			// concurrently must neither restart capture nor persist the cleanup lease.
			toggles := 2
			if paused {
				toggles = 1
			}
			for i := 0; i < toggles; i++ {
				if w := send("/settings/pause"); w.Code != 303 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			var wg sync.WaitGroup
			for _, path := range []string{"/settings/check", "/settings/credentials"} {
				wg.Go(func() {
					if w := send(path); w.Code != 303 {
						t.Error(w.Code, w.Body.String())
					}
				})
			}
			wg.Wait()
			if value, _, e := a.Secrets.Get("SLACK_APP_TOKEN"); e != nil || value != "xapp-synthetic" {
				t.Fatal("credential save did not persist", e)
			}
			r := a.Manager.List()[0]
			if r.Paused != paused || !r.Suspended {
				t.Fatal("settings changed user choice or resumed capture", r.Paused, r.Suspended)
			}
			a.reportMu.Lock()
			report := a.reports["fixture"]
			a.reportMu.Unlock()
			if report == nil || !strings.Contains(report.Error, "paused while cleanup runs") {
				t.Fatal("check did not explain cleanup suspension", report)
			}
			page := httptest.NewRecorder()
			a.dashboard(page, httptest.NewRequest("GET", "/settings/", nil), "csrf", true)
			if !paused && !strings.Contains(page.Body.String(), "Paused for cleanup") {
				t.Fatal("suspension absent from UI")
			}
			resume()
			if a.Manager.IsSuspended("fixture") || a.Manager.IsPaused("fixture") != paused {
				t.Fatal("release changed user choice")
			}
			a.Close()
			a, e = NewApp(opts)
			if e != nil {
				t.Fatal(e)
			}
			r = a.Manager.List()[0]
			if r.Paused != paused || r.Suspended {
				t.Fatal("restart persisted cleanup suspension", r.Paused, r.Suspended)
			}
		})
	}
}

func TestScheduledCleanupCancellationRestoresCapture(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-synthetic")
	t.Setenv("CANCEL_CLEANUP_TOKEN", "xoxb-cleanup-synthetic")
	home := t.TempDir()
	fake := slacktest.New(t)
	cfg := fmt.Sprintf("workspaces:\n- id: fixture\n  platform: slack\n  team_id: TFIXTURE\n  channels: [CFIXTURE]\n  storage: {path: %q}\n  cleanup: {enabled: true, token_env: CANCEL_CLEANUP_TOKEN, approval: {who: owner, owners: [UOWNER], reactions: [wastebasket]}}\n", filepath.Join(home, "archive"))
	if e := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	var a *App
	// Cancel an in-flight identity verification after capture has drained.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fake.Overrides["auth.test"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer xoxb-cleanup-synthetic" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, `{"ok":true,"team_id":"TFIXTURE","user_id":"UBOT"}`)
	}
	fake.Overrides["conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "100.000001", "user": "UBOT", "text": "test", "reactions": []any{map[string]any{"name": "wastebasket", "users": []string{"UOWNER"}, "count": 1}}}}})
	}
	var e error
	a, e = NewApp(Options{Home: home, Host: "127.0.0.1", Mode: "desktop", HTTP: fake.Client()})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	r := a.Manager.List()[0]
	// The archive is written while capture is suspended so it cannot be refreshed
	// before the cancelled cleanup has verified the source.
	resume, e := a.Manager.SuspendCapture("fixture")
	if e != nil {
		t.Fatal(e)
	}
	msg := pluginapi.Message{"ts": "100.000001", "user": "UBOT", "text": "test", "reactions": []any{map[string]any{"name": "wastebasket", "users": []string{"UOWNER"}, "count": 1}}}
	snap := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, Platform: "slack", TeamID: "TFIXTURE", Channel: "CFIXTURE", RootTS: "100.000001", MessageCount: 1}, Messages: []pluginapi.Message{msg}, Users: map[string]pluginapi.Message{"UBOT": {"id": "UBOT"}}}
	if _, e := a.Manager.List()[0].Store.Save(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}, snap); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); a.applyScheduled(ctx, r.Entry) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("cleanup did not reach verification")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup ignored cancellation")
	}
	resume()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := a.Manager.List()[0]
		if !r.Suspended && !r.Paused && r.Runtime.Health().State == "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled cleanup left capture stopped", a.Manager.Failure())
}

func TestManualAndScheduledCleanupClaimIsAtomic(t *testing.T) {
	a, e := NewApp(Options{Home: t.TempDir(), Host: "127.0.0.1", Mode: "desktop"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	start := make(chan struct{})
	results := make(chan int64, 20)
	for i := 0; i < 20; i++ {
		go func() {
			<-start
			id, e := a.claimCleanupRun("fixture", "Starting cleanup")
			if e != nil {
				t.Error(e)
			}
			results <- id
		}()
	}
	close(start)
	winners := 0
	for i := 0; i < 20; i++ {
		if <-results != 0 {
			winners++
		}
	}
	if winners != 1 {
		t.Fatal("overlapping manual/scheduled runs", winners)
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM cleanup_runs").Scan(&count)
	if count != 1 {
		t.Fatal("skipped claims advanced schedule", count)
	}
	a.db.Exec("UPDATE cleanup_runs SET status='complete'")
	if id, e := a.claimCleanupRun("fixture", "Starting automatic cleanup"); e != nil || id == 0 {
		t.Fatal("completed run blocked next claim", id, e)
	}
}
