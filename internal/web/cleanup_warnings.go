package web

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"time"
)

// Warnings supplement the engine's offline preview without changing its digest.
// Network identity reads here never authorize deletion or replace apply's checks.
func (a *App) cleanupWarnings(ctx context.Context, entry config.Entry, live bool) []string {
	ws := entry.Workspace
	out := []string{}
	approval := ws.CleanupApproval()
	if len(approval.Reactions) == 0 {
		out = append(out, "No delete approval reaction configured")
	}
	if approval.Who != "owner" || len(approval.Owners) == 0 {
		out = append(out, "No cleanup owners configured; pick yourself in Settings")
	}
	token, _, err := a.Secrets.Get(ws.Cleanup.TokenEnv)
	if err != nil || token == "" {
		return append(out, "Deletion credential missing; paste and test it in Settings")
	}
	if !live || ws.Cleanup.Approval == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := entry.Plugin.NewCleanupDriver(ctx, ws, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}, HTTP: a.opts.HTTP})
	if err != nil {
		return append(out, "Deletion credential invalid; paste and test it in Settings")
	}
	store, err := storage.Open(ws.Storage)
	if err != nil {
		return out
	}
	defer store.Close()
	keys, _ := store.Keys(ctx)
	for _, key := range keys {
		snap, err := store.Read(ctx, key)
		if err != nil {
			continue
		}
		if err = d.Verify(ctx, snap.Manifest); err != nil {
			return append(out, err.Error())
		}
		for _, msg := range snap.Messages {
			reason, err := d.Reason(ctx, msg)
			if err != nil {
				return append(out, "Cannot verify message identity; test deletion credential in Settings")
			}
			if reason != "" {
				out = append(out, fmt.Sprintf("%s / %s: %s", key.Channel, msg["ts"], reason))
			}
		}
	}
	return out
}
