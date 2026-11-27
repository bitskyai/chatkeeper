package web

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/internal/cleanup"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"os"
	"time"
)

// A single worker serializes automatic runs. Manual and automatic apply still
// share the storage cleanup lock and compare-before-delete journal.
func (a *App) scheduleCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.runScheduledCleanup(ctx, now)
		}
	}
}
func (a *App) runScheduledCleanup(ctx context.Context, now time.Time) {
	cfg, e := config.Load(a.opts.Config, false)
	if e != nil {
		report, _ := json.Marshal(cleanup.Report{Reason: "Automatic cleanup: cannot load configuration: " + e.Error()})
		_, _ = a.db.Exec("INSERT INTO cleanup_runs(workspace,at,report,status) VALUES('',unixepoch(),?,'complete')", string(report))
		return
	}
	for _, entry := range cfg.Workspaces {
		ws := entry.Workspace
		if a.Manager.IsPaused(ws.ID) || a.Manager.IsSuspended(ws.ID) || !ws.Cleanup.Enabled || ws.Cleanup.Every <= 0 {
			continue
		}
		var active int
		if a.db.QueryRow("SELECT COUNT(*) FROM cleanup_runs WHERE workspace=? AND status='running'", ws.ID).Scan(&active) != nil || active > 0 {
			continue
		}
		var last int64
		if a.db.QueryRow("SELECT COALESCE(MAX(at),0) FROM cleanup_runs WHERE workspace=?", ws.ID).Scan(&last) != nil {
			continue
		}
		// The first run waits a full interval from enable/change, even after old runs.
		if ws.Cleanup.ScheduleSince.IsZero() {
			if info, e := configInfo(a.opts.Config); e == nil {
				if info > last {
					last = info
				}
			}
		}
		origin := time.Unix(last, 0)
		if ws.Cleanup.ScheduleSince.After(origin) {
			origin = ws.Cleanup.ScheduleSince
		}
		if now.Sub(origin) < ws.Cleanup.Every {
			continue
		}
		a.applyScheduled(ctx, entry)
	}
}
func (a *App) applyScheduled(ctx context.Context, entry config.Entry) {
	ws := entry.Workspace
	runID, e := a.claimCleanupRun(ws.ID, "Starting automatic cleanup")
	if e != nil || runID == 0 {
		return
	}
	resume, err := a.Manager.SuspendCapture(ws.ID)
	if err != nil {
		report, _ := json.Marshal(cleanup.Report{Workspace: ws.ID, Reason: "Automatic cleanup: cannot suspend capture"})
		_, _ = a.db.Exec("UPDATE cleanup_runs SET report=?,status='complete' WHERE id=?", string(report), runID)
		return
	}
	defer resume()
	report := cleanup.Report{Workspace: ws.ID, Reason: "Automatic cleanup: archive unavailable"}
	store, e := storage.Open(ws.Storage)
	if e == nil {
		defer store.Close()
		preview := cleanup.Run(ctx, ws, store, nil, false)
		token, _, err := a.Secrets.Get(ws.Cleanup.TokenEnv)
		if err == nil {
			var driver pluginapi.CleanupDriver
			driver, err = entry.Plugin.NewCleanupDriver(ctx, ws, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}, HTTP: a.opts.HTTP})
			if err == nil {
				report = cleanup.ApplyBoundProgress(ctx, ws, store, driver, preview, func(progress cleanup.Report) {
					b, _ := json.Marshal(progress)
					_, _ = a.db.Exec("UPDATE cleanup_runs SET report=? WHERE id=?", string(b), runID)
					if a.Manager.Search != nil && len(progress.Threads) > 0 {
						t := progress.Threads[len(progress.Threads)-1]
						_ = a.Manager.Search.Sync(ctx, ws.ID, pluginapi.Key{Channel: t.Channel, RootTS: t.RootTS})
					}
				})
			}
		}
		if err != nil {
			report.Reason = "Automatic cleanup: deletion credential missing or invalid; fix in Settings"
		}
	}
	b, _ := json.Marshal(report)
	_, _ = a.db.Exec("UPDATE cleanup_runs SET report=?,status='complete' WHERE id=?", string(b), runID)
}

func configInfo(path string) (int64, error) {
	info, e := os.Stat(path)
	if e != nil {
		return 0, e
	}
	return info.ModTime().Unix(), nil
}
