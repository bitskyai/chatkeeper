package web

import (
	"github.com/bitskyai/chatkeeper/internal/config"
)

// Warnings supplement the engine's offline preview without changing its digest.
// Live verification remains in apply, with background progress.
func (a *App) cleanupWarnings(entry config.Entry) []string {
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
	return out
}
