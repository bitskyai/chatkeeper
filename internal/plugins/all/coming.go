package all

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// Planned registrations make availability discoverable without platform lists in the UI.
type planned struct{ id, name string }

func init() {
	pluginapi.Register(planned{"discord", "Discord"})
	pluginapi.Register(planned{"telegram", "Telegram"})
}
func (p planned) Manifest() pluginapi.Manifest {
	return pluginapi.Manifest{ID: p.id, Name: p.name, Version: "0.0.0", APIVersion: pluginapi.APIVersion}
}
func (planned) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (planned) CredentialSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (planned) SetupFlow() []pluginapi.SetupStep { return nil }
func (planned) ParseWorkspace(context.Context, json.RawMessage, pluginapi.Credentials) (pluginapi.Settings, error) {
	return nil, pluginapi.ErrUnsupported
}
func (planned) ValidateWorkspaces(context.Context, []pluginapi.Settings) error {
	return pluginapi.ErrUnsupported
}
func (planned) NewSession(context.Context, pluginapi.Workspace, pluginapi.Env) (pluginapi.Session, error) {
	return nil, pluginapi.ErrUnsupported
}
func (planned) NewCleanupDriver(context.Context, pluginapi.Workspace, pluginapi.Env) (pluginapi.CleanupDriver, error) {
	return nil, pluginapi.ErrUnsupported
}
func (planned) Content() pluginapi.ContentHooks { return nil }
