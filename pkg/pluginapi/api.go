// Package pluginapi is the versioned, platform-neutral contract for built-in plugins.
// Sessions must honor context cancellation and must not leave goroutines behind.
package pluginapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

const APIVersion = 1

var ErrUnsupported = errors.New("unsupported capability")
var ErrInvalid = errors.New("invalid content")

type Credentials map[string]string

func (Credentials) Format(s fmt.State, _ rune)   { _, _ = io.WriteString(s, "[redacted]") }
func (Credentials) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Credentials) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type Capabilities struct{ ReadHistory, Realtime, Delete bool }
type Manifest struct {
	ID, Name, Version string
	APIVersion        int
	Capabilities      Capabilities
	ClaimsLegacy      bool

	DefaultReactions []string
}
type Settings json.RawMessage
type Trigger struct {
	Reactions []string `json:"reactions" yaml:"reactions"`
	Who       string   `json:"who" yaml:"who"`
	Owners    []string `json:"owners" yaml:"owners"`
}
type CleanupPolicy struct {
	Enabled     bool
	AllowOthers bool
	OwnOnly     bool
	MinAge      time.Duration
	TokenEnv    string
}
type Workspace struct {
	Cleanup               CleanupPolicy
	ID, Platform, Storage string
	Channels              []string
	AutoChannels          bool
	Trigger               Trigger
	PollInterval          time.Duration
	Settings              Settings
}
type Env struct {
	Credentials Credentials
	Log         *slog.Logger
	HTTP        *http.Client
	StateDir    string
	Now         func() time.Time
}
type Identity struct{ TeamID, UserID string }
type Channel struct{ ID, Name string }
type Key struct{ Channel, RootTS string }
type RootRef struct {
	Channel, RootTS string
	Reactions       []Reaction
}
type Message map[string]any
type Reaction struct {
	Name  string
	Users []string
	Count int
}
type Root struct {
	TS, ThreadTS string
	Reactions    []Reaction
	Raw          Message
}
type Event struct{ Channel, RootTS, Kind string }
type EventSink func(Event) bool

// Blob carries inline bytes or a confined, verified local file reference.
// Both forms are plain serializable data; a future RPC adapter can stream refs.
type Blob struct {
	Size   int64
	SHA256 string
	Data   []byte
	Source *FileSource
}
type FileSource struct{ Root, Path string }
type Object struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Attachment struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}
type ArchiveManifest struct {
	Schema       int               `json:"schema"`
	Platform     string            `json:"platform,omitempty"`
	TeamID       string            `json:"team_id"`
	Channel      string            `json:"channel"`
	RootTS       string            `json:"root_ts"`
	Permalink    string            `json:"permalink"`
	MessageCount int               `json:"message_count"`
	Objects      map[string]Object `json:"objects"`
	Files        []Attachment      `json:"files"`
}
type Snapshot struct {
	// SourceDeletedAt is a durable journal overlay; original object bytes remain immutable.
	SourceDeletedAt map[string]string `json:"source_deleted_at,omitempty"`
	Manifest        ArchiveManifest
	Messages        []Message
	Users           map[string]Message
	// RawUsers retains optional malformed non-author metadata without rendering it.
	RawUsers  map[string]any
	Blobs     map[string]Blob
	Documents map[string][]byte
}
type ContentHooks interface {
	Validate(context.Context, Snapshot) error
	Text(context.Context, Message, map[string]Message) (string, error)
	Author(context.Context, Message) (string, error)
}
type Plugin interface {
	Manifest() Manifest
	ConfigSchema() json.RawMessage
	CredentialSchema() json.RawMessage
	ParseWorkspace(context.Context, json.RawMessage, Credentials) (Settings, error)
	ValidateWorkspaces(context.Context, []Settings) error
	SetupFlow() []SetupStep
	NewSession(context.Context, Workspace, Env) (Session, error)
	NewCleanupDriver(context.Context, Workspace, Env) (CleanupDriver, error)
	Content() ContentHooks
}
type Session interface {
	Capabilities() Capabilities
	VerifyIdentity(context.Context) (Identity, error)
	Channels(context.Context) ([]Channel, error)
	History(context.Context, string, string) ([]RootRef, error)
	Root(context.Context, string, string) (Root, error)
	Snapshot(context.Context, string, Root, *Snapshot) (Snapshot, error)
	Start(context.Context, EventSink) error
	Stop(context.Context) error
	Reconfigure(context.Context, Workspace) (bool, error)
}
type CleanupDriver interface {
	Verify(context.Context, ArchiveManifest) error
	Reason(context.Context, Message) (string, error)
	Check(context.Context, ArchiveManifest, []Message, []string) error
	Delete(context.Context, ArchiveManifest, Message) error
	Pace(context.Context) error
}
type Link struct{ Label, URL string }
type Choice struct{ ID, Label string }
type SetupState map[string]json.RawMessage
type StepResult struct {
	OK          bool
	FieldErrors map[string]string
	Messages    []string
	Data        json.RawMessage
	Choices     []Choice
}
type SetupStep struct {
	ID, Title, Kind, Body string
	Links                 []Link
	Schema                json.RawMessage
	Run                   func(context.Context, SetupState, json.RawMessage, Env) (StepResult, error)
}

var registry = struct {
	sync.RWMutex
	plugins map[string]Plugin
}{plugins: map[string]Plugin{}}

func Register(p Plugin) {
	m := p.Manifest()
	if err := ValidateManifest(m); err != nil {
		panic(err)
	}
	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.plugins[m.ID]; ok {
		panic("duplicate plugin id")
	}
	if m.ClaimsLegacy {
		for _, prior := range registry.plugins {
			if prior.Manifest().ClaimsLegacy {
				panic("duplicate legacy claim")
			}
		}
	}
	registry.plugins[m.ID] = p
}
func Plugins() []Plugin {
	registry.RLock()
	defer registry.RUnlock()
	ids := make([]string, 0, len(registry.plugins))
	for id := range registry.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Plugin, 0, len(ids))
	for _, id := range ids {
		out = append(out, registry.plugins[id])
	}
	return out
}
func Lookup(id string) (Plugin, bool) {
	registry.RLock()
	defer registry.RUnlock()
	p, ok := registry.plugins[id]
	return p, ok
}
func Legacy() (Plugin, bool) {
	for _, p := range Plugins() {
		if p.Manifest().ClaimsLegacy {
			return p, true
		}
	}
	return nil, false
}
func ValidateManifest(m Manifest) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`).MatchString(m.ID) || m.Name == "" || m.Version == "" || m.APIVersion < 1 {
		return errors.New("invalid plugin manifest")
	}
	return nil
}

// ValidateSchema accepts only the form-rendering subset; parsing remains authoritative.
func ValidateSchema(raw json.RawMessage) error {
	var node map[string]any
	if json.Unmarshal(raw, &node) != nil || node == nil {
		return errors.New("invalid schema")
	}
	return schema(node, false)
}
func schema(n map[string]any, item bool) error {
	allowed := map[string]bool{}
	for _, k := range []string{"type", "properties", "items", "enum", "title", "description", "default", "examples", "pattern", "minimum", "maximum", "required", "x-secret", "x-env-default", "x-widget", "x-options-from"} {
		allowed[k] = true
	}
	for k := range n {
		if !allowed[k] {
			return errors.New("unsupported schema keyword")
		}
	}
	typ, _ := n["type"].(string)
	switch typ {
	case "object", "string", "integer", "number", "boolean", "array":
	case "":
		if _, ok := n["enum"]; !ok {
			return errors.New("missing schema type")
		}
	default:
		return errors.New("unsupported schema type")
	}
	if item && typ != "string" && typ != "" {
		return errors.New("array must contain strings")
	}
	if props, ok := n["properties"]; ok {
		p, valid := props.(map[string]any)
		if !valid || typ != "object" {
			return errors.New("invalid schema properties")
		}
		for _, v := range p {
			m, ok := v.(map[string]any)
			if !ok {
				return errors.New("invalid property")
			}
			if err := schema(m, false); err != nil {
				return err
			}
		}
	}
	if examples, present := n["examples"]; present {
		values, ok := examples.([]any)
		if !ok || typ != "string" {
			return errors.New("examples must be strings")
		}
		for _, v := range values {
			if _, ok := v.(string); !ok {
				return errors.New("examples must be strings")
			}
		}
	}
	if typ == "array" {
		m, ok := n["items"].(map[string]any)
		if !ok {
			return errors.New("array requires items")
		}
		return schema(m, true)
	}
	return nil
}

// CleanupOffline checks platform policy without credentials or network access.
type CleanupOffline interface {
	CleanupReason(Workspace, Message) string
}

// CleanupStatus classifies safe mutation failures without leaking transport details.
type CleanupError struct{ Status, Reason string }

func (e *CleanupError) Error() string { return e.Reason }

// CleanupBoundary validates source-specific workspace identity offline.
type CleanupBoundary interface {
	CleanupBoundary(Workspace, ArchiveManifest) string
}
