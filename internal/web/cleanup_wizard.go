package web

import (
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
)

// offerCleanupStep is isolated from the source connection/token wizard steps.
func (a *App) offerCleanupStep(w http.ResponseWriter, r *http.Request, csrf string, z *wizard, p pluginapi.Plugin) bool {
	if r.FormValue("cleanup_done") == "true" || z.Workspace == "" {
		return false
	}
	if _, ok := p.(pluginapi.CleanupConfigurator); !ok {
		return false
	}
	r.Form.Set("workspace", z.Workspace)
	// Finish offers an optional page. Its form saves through the same Settings route.
	copy := r.Clone(r.Context())
	copy.Method = "GET"
	a.cleanupSettings(w, copy, csrf, true)
	return true
}
