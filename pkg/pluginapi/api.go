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

// Trigger selects roots to archive. Match "all" (the default, kept for legacy
// configurations) requires every listed reaction; "any" requires at least one.
type Trigger struct {
	Reactions []string `json:"reactions" yaml:"reactions"`
	Who       string   `json:"who" yaml:"who"`
	Owners    []string `json:"owners" yaml:"owners"`
	Match     string   `json:"match,omitempty" yaml:"match,omitempty"`
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

// ChannelStatus reports whether the connected account can read a channel.
type ChannelStatus struct {
	ID, Name string
	Member   bool
	Problem  string
}

// ChannelInspector is an optional Session extension used by diagnostics.
type ChannelInspector interface {
	InspectChannels(context.Context, []string) ([]ChannelStatus, error)
}

// RealtimeReporter is an optional Session extension describing the live
// event connection. Detail is a short user-facing explanation.
type RealtimeReporter interface {
	RealtimeStatus() (connected bool, detail string)
}

// ReplyReaction is a reaction on a non-root message, reported by diagnostics only.
type ReplyReaction struct {
	Channel, RootTS, TS, Name string
}

// ReplyInspector is an optional Session extension: it lists reactions placed
// on replies of the given roots so diagnostics can explain why they never match.
type ReplyInspector interface {
	ReplyReactions(context.Context, []RootRef, []string) ([]ReplyReaction, error)
}
type Key struct{ Channel, RootTS string }
type RootRef struct {
	Channel, RootTS string
	Reactions       []Reaction
	Replies         int
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

// Event is a capture hint. ItemTS, when set and different from RootTS, names the
// message inside the thread that a reaction event was about (a reply); the
// runtime re-reads it through ItemReader before it may authorize an archive.
type Event struct{ Channel, RootTS, Kind, ItemTS string }

// ItemReader is an optional Session extension for realtime reply reactions: it
// re-reads the reactions on one message of a thread and must verify that the
// message belongs to rootTS. It returns a zero Root when the message is gone.
type ItemReader interface {
	Item(ctx context.Context, channel, rootTS, ts string) (Root, error)
}
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
	// CleanupProof links this generation to the journal whose receipts it folded.
	CleanupProof string            `json:"cleanup_proof,omitempty"`
	Schema       int               `json:"schema"`
	Platform     string            `json:"platform,omitempty"`
	TeamID       string            `json:"team_id"`
	Channel      string            `json:"channel"`
	RootTS       string            `json:"root_ts"`
	Permalink    string            `json:"permalink"`
	MessageCount int               `json:"message_count"`
	Objects      map[string]Object `json:"objects"`
	Files        []Attachment      `json:"files"`
	// Gaps lists items the source referenced but the capture could not store
	// (an unreadable file or author, for example). The rest of the thread is kept.
	Gaps []Gap `json:"gaps,omitempty"`
}

// Gap is one referenced item that is missing from an archive, with a short,
// credential-free reason suitable for display.
type Gap struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason"`
}
type Snapshot struct {
	// RawDocuments contains original transport response bytes; Prepare seals them into Documents.
	RawDocuments map[string][]byte
	// CleanupStatus summarizes the journal state for archive readers.
	CleanupStatus string `json:"cleanup_status,omitempty"`
	// SourceDeletedAt projects verified journal and persisted deletion markers.
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

// Choice is a selectable option. Glyph, Image and Group are optional display hints
// (for example an emoji character, a custom image URL and a picker category).
type Choice struct {
	ID, Label           string
	Glyph, Image, Group string
	Keywords            string
}

// Check is one line of a connection test result.
type Check struct {
	Label, Detail string
	OK            bool
	Optional      bool
}

// ReactionView describes a stored reaction for display.
type ReactionView struct {
	Name, Glyph, Image string
	// MatchNames are trigger names that match this reaction, supplied by the platform.
	MatchNames []string
	Count      int
}

// ReactionRenderer is an optional ContentHooks extension for viewers.
type ReactionRenderer interface {
	Reactions(context.Context, Message) ([]ReactionView, error)
}
type SetupState map[string]json.RawMessage
type StepResult struct {
	OK          bool
	FieldErrors map[string]string
	Messages    []string
	Data        json.RawMessage
	Choices     []Choice
	// Links and Checks are optional: Prepare may compute links from earlier
	// steps, and Test/Run may report itemized connection results.
	Links  []Link
	Checks []Check
}

// SetupStep is one wizard page. Run validates a submission. Optional hooks:
// Prepare runs when the page is shown (dynamic links, choices); Test runs the
// "Test connection" action without advancing. Skip, when set, labels an explicit
// fallback action that clears this step's secret fields and then calls Run.
type SetupStep struct {
	ID, Title, Kind, Body string
	Links                 []Link
	Schema                json.RawMessage
	Run                   func(context.Context, SetupState, json.RawMessage, Env) (StepResult, error)
	Prepare               func(context.Context, SetupState, Env) (StepResult, error)
	Test                  func(context.Context, SetupState, json.RawMessage, Env) (StepResult, error)
	Skip, SkipNote        string
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
