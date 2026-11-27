// Package search provides read-only queries over a derived archive index.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"regexp"
	"sort"
	"sync"
	"time"
)

var ErrQuery = errors.New("invalid search query or stale cursor")
var ErrUnavailable = errors.New("archive search unavailable")
var ErrLocked = fmt.Errorf("%w: index writer already active", ErrUnavailable)
var ErrNotFound = errors.New("thread not found")
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var threadID = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

type Source struct {
	Path     string
	Content  pluginapi.ContentHooks
	Identity string
}
type workspace struct {
	source         Source
	store          *storage.Folder
	db             *sql.DB
	fallbackLogged bool
	readerUnlock   func()
	textBudget     *int
}
type Service struct {
	mu         sync.Mutex
	home       string
	readOnly   bool
	unlock     func()
	workspaces map[string]*workspace
}

func New(home string) *Service { return &Service{home: home, workspaces: map[string]*workspace{}} }

type Query struct {
	Q         string `json:"q"`
	Workspace string `json:"workspace,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Author    string `json:"author,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	After     string `json:"after,omitempty"`
}
type Hit struct {
	Workspace     string  `json:"workspace"`
	Channel       string  `json:"channel"`
	RootTS        string  `json:"root_ts"`
	Score         float64 `json:"score"`
	Snippet       string  `json:"snippet"`
	Permalink     string  `json:"permalink"`
	SourceDeleted bool    `json:"source_deleted"`
	PriorVersions bool    `json:"prior_versions"`
}
type Page struct {
	Results    []Hit   `json:"results"`
	NextCursor *string `json:"next_cursor"`
}
type Thread struct {
	Workspace string                    `json:"workspace"`
	Channel   string                    `json:"channel"`
	RootTS    string                    `json:"root_ts"`
	Manifest  pluginapi.ArchiveManifest `json:"manifest"`
	Messages  []pluginapi.Message       `json:"messages"`
	Users     map[string]any            `json:"users"`
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workspaces {
		w.releaseReader()
		if w.db != nil {
			w.db.Close()
		}
		w.store.Close()
	}
	s.workspaces = map[string]*workspace{}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
}

func (s *Service) ListWorkspaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := []string{}
	for n := range s.workspaces {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
func (s *Service) GetThread(ctx context.Context, id, ch, ts string) (Thread, error) {
	out := Thread{}
	if !identifier.MatchString(id) || !threadID.MatchString(ch) || !threadID.MatchString(ts) {
		return out, ErrQuery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workspaces[id]
	if w == nil {
		return out, ErrNotFound
	}
	e := w.store.Inspect(ctx, pluginapi.Key{Channel: ch, RootTS: ts}, func(v *pluginapi.Snapshot) error {
		if v == nil {
			return ErrNotFound
		}
		if core.Guard(ctx, 10*time.Second, func(c context.Context) error { return w.source.Content.Validate(c, *v) }) != nil {
			return ErrUnavailable
		}
		users := v.RawUsers
		if users == nil {
			users = map[string]any{}
			for k, v := range v.Users {
				users[k] = v
			}
		}
		out = Thread{id, ch, ts, v.Manifest, v.Messages, users}
		return nil
	})
	if e != nil && !errors.Is(e, ErrNotFound) {
		return Thread{}, ErrUnavailable
	}
	return out, e
}
