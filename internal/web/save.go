package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// Draft is a completed setup flow ready to be saved as a workspace. References
// maps a credential to an environment variable the config should name instead
// of storing the value.
type Draft struct {
	ID, Storage string
	Data        pluginapi.SetupState
	Credentials pluginapi.Credentials
	References  map[string]string
}

// StorageRoot is the default parent folder for archives: ~/ChatKeeper on a
// desktop, <home>/data on a server.
func StorageRoot(home, mode string) string {
	if mode == "desktop" {
		if dir, e := os.UserHomeDir(); e == nil {
			return filepath.Join(dir, "ChatKeeper")
		}
	}
	return filepath.Join(home, "data")
}

// credentialName is the secret store key for a workspace credential.
func credentialName(wid, field string) string {
	return "CHATKEEPER_" + strings.ToUpper(hex.EncodeToString([]byte(wid))) + "_" + strings.ToUpper(field)
}

// BuildEntry turns a draft into a config entry and lets the plugin validate it.
func BuildEntry(ctx context.Context, p pluginapi.Plugin, d Draft) (map[string]any, error) {
	entry := map[string]any{"id": d.ID, "platform": p.Manifest().ID, "storage": map[string]any{"type": "folder", "path": d.Storage}}
	for k, v := range d.Data {
		var value any
		_ = json.Unmarshal(v, &value)
		entry[k] = value
	}
	for _, f := range schemaFields(p.CredentialSchema()) {
		name := credentialName(d.ID, f.Name)
		if d.References[f.Name] != "" {
			name = d.References[f.Name]
		}
		entry[f.Name+"_env"] = name
	}
	b, _ := json.Marshal(entry)
	if _, e := p.ParseWorkspace(ctx, b, d.Credentials); e != nil {
		return nil, errors.New("invalid workspace settings")
	}
	return entry, nil
}

// SaveEntry stores the draft's credentials in the secret store (a skipped
// optional credential clears an earlier value) and writes the validated entry.
func SaveEntry(configPath string, store *secrets.Store, p pluginapi.Plugin, d Draft, entry map[string]any) error {
	known := map[string]bool{}
	for _, f := range schemaFields(p.CredentialSchema()) {
		known[f.Name] = true
	}
	for k, v := range d.Credentials {
		if v != "" && !known[k] {
			return errors.New("unknown credential")
		}
	}
	for _, f := range schemaFields(p.CredentialSchema()) {
		k := f.Name
		if d.References[k] != "" {
			continue
		}
		name, ok := entry[k+"_env"].(string)
		if !ok || name == "" {
			return errors.New("missing credential reference")
		}
		v := d.Credentials[k]
		if v == "" {
			if old, _, _ := store.Get(name); old == "" {
				continue
			}
		}
		if e := store.Set(name, v); e != nil {
			return e
		}
	}
	return validateAndWrite(configPath, store, d.ID, entry)
}
