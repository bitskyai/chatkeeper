// Package app owns the serialised running workspace set and config reload boundary.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SecretResolver interface {
	Get(string) (string, string, error)
}
type Running struct {
	Entry            config.Entry
	Runtime          *core.Runtime
	Store            *storage.Folder
	cancel           context.CancelFunc
	done             chan struct{}
	fingerprint      [32]byte
	Paused           bool
	Suspended        bool
	SuspensionReason string
}
type Manager struct {
	mu         sync.Mutex
	reloadMu   sync.Mutex
	readMu     sync.RWMutex
	ctx        context.Context
	Path       string
	Secrets    SecretResolver
	HTTP       *http.Client
	running    map[string]*Running
	generation int
	Error      string
	apiToken   string
	closed     bool
	OnCapture  func(string, pluginapi.Key)
	Search     *search.Service
	suspended  map[string]int
	paused     map[string]bool
	watched    [2]os.FileInfo
	retry      bool
	retryDelay time.Duration
	retryAt    time.Time
}

func New(ctx context.Context, path string, secrets SecretResolver, client *http.Client) *Manager {
	return &Manager{ctx: ctx, Path: path, Secrets: secrets, HTTP: client, running: map[string]*Running{}, paused: map[string]bool{}, suspended: map[string]int{}}
}
func (m *Manager) Generation() int { m.mu.Lock(); defer m.mu.Unlock(); return m.generation }
func (m *Manager) List() []*Running {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Running{}
	for _, r := range m.running {
		copy := *r
		out = append(out, &copy)
	}
	return out
}
func (m *Manager) Failure() string { m.mu.Lock(); defer m.mu.Unlock(); return m.Error }
func (m *Manager) stop(r *Running) { r.cancel(); <-r.done; _ = r.Store.Close() }
func (m *Manager) Close() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.mu.Lock()
	m.closed = true
	running := m.running
	m.running = map[string]*Running{}
	m.mu.Unlock()
	for _, r := range running {
		m.stop(r)
	}
	if m.Search != nil {
		m.Search.Close()
	}
}
func (m *Manager) Pause(id string) error {
	m.reloadMu.Lock()
	m.paused[id] = !m.paused[id]
	defer m.reloadMu.Unlock()
	if err := m.reload(); err != nil {
		m.paused[id] = !m.paused[id]
		return err
	}
	return nil
}
func (m *Manager) Reload() error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.reload()
}
func (m *Manager) reload() (err error) {
	defer func() {
		if err != nil {
			m.retry = true
			m.setError(m.Failure() + "; capture configuration retry pending")
			slog.Warn("capture configuration failed; retry pending")
		} else {
			m.retry = false
			m.retryDelay = 0
			m.retryAt = time.Time{}
		}
	}()
	m.watched = m.fileStats()
	m.mu.Lock()
	closed, current, generation := m.closed, m.running, m.generation
	m.mu.Unlock()
	if closed || m.ctx.Err() != nil {
		return errors.New("application stopped")
	}
	var resolveErr error
	resolve := func(k string) string {
		if m.Secrets == nil {
			return os.Getenv(k)
		}
		v, _, e := m.Secrets.Get(k)
		if e != nil {
			resolveErr = e
		}
		return v
	}
	cfg, e := config.LoadWithResolver(m.Path, true, resolve)
	if e != nil || resolveErr != nil {
		m.setError("Invalid configuration; running configuration retained")
		return config.ErrConfig
	}
	// Prepare all stores before replacing any running workspace.
	next := map[string]*Running{}
	opened := []*Running{}
	for _, entry := range cfg.Workspaces {
		b, _ := json.Marshal(struct {
			W pluginapi.Workspace
			C map[string]string
			P bool
			S bool
		}{entry.Workspace, entry.Credentials, m.paused[entry.Workspace.ID], m.suspended[entry.Workspace.ID] > 0})
		fp := sha256.Sum256(b)
		if prior := current[entry.Workspace.ID]; prior != nil && prior.fingerprint == fp {
			next[entry.Workspace.ID] = prior
			continue
		}
		store, e := storage.Open(entry.Workspace.Storage)
		if e != nil {
			for _, r := range opened {
				_ = r.Store.Close()
			}
			m.setError("Cannot open archive; running configuration retained")
			return errors.New("cannot open archive")
		}
		r := &Running{Entry: entry, Store: store, cancel: func() {}, done: make(chan struct{}), fingerprint: fp, Paused: m.paused[entry.Workspace.ID], Suspended: m.suspended[entry.Workspace.ID] > 0}
		if r.Suspended {
			r.SuspensionReason = "cleanup running"
		}
		r.Runtime = core.New(entry.Workspace, entry.Plugin, pluginapi.Env{Credentials: entry.Credentials, HTTP: m.HTTP}, store)
		wid := entry.Workspace.ID
		r.Runtime.OnArchive = func(k pluginapi.Key) {
			if m.Search != nil {
				if e := m.Search.Sync(m.ctx, wid, k); e != nil {
					slog.Default().Warn("search update failed; restart rebuilds index")
				}
			}

		}
		r.Runtime.OnCapture = func(k pluginapi.Key) {
			if m.OnCapture != nil {
				m.OnCapture(wid, k)
			}
		}
		opened = append(opened, r)
		next[entry.Workspace.ID] = r
	}
	sources := map[string]search.Source{}
	if m.Search != nil {
		for id, r := range next {
			source, e := search.FromPlugin(m.ctx, r.Entry.Workspace.Storage, r.Entry.Plugin)
			if e != nil {
				for _, r := range opened {
					r.Store.Close()
				}
				m.setError("Content adapter unavailable; running configuration retained")
				return search.ErrUnavailable
			}
			sources[id] = source
		}
	}
	m.readMu.Lock()
	if m.Search != nil {
		if e := m.Search.Configure(m.ctx, sources); e != nil {
			for _, r := range opened {
				r.Store.Close()
			}
			m.retry = errors.Is(e, search.ErrLocked)
			m.setError("Cannot initialize search; running configuration retained")
			m.readMu.Unlock()
			return e
		}
	}

	startContexts := map[*Running]context.Context{}
	for _, r := range opened {
		ctx, cancel := context.WithCancel(m.ctx)
		r.cancel = cancel
		startContexts[r] = ctx
		if r.Paused || r.Suspended {
			close(r.done)
		}
	}

	changed := generation == 0 || len(next) != len(current)
	for id, r := range next {
		if current[id] != r {
			changed = true
		}
	}
	m.mu.Lock()

	m.running = next
	m.apiToken = cfg.Token["bearer"]
	if changed {
		m.generation++
	}
	m.Error = ""
	m.mu.Unlock()
	m.readMu.Unlock()
	for id, r := range current {
		if next[id] != r {
			m.stop(r)
		}
	}
	for _, r := range opened {
		if !r.Paused && !r.Suspended {
			go func() { defer close(r.done); r.Runtime.Run(startContexts[r]) }()
		}
	}

	return nil
}
func (m *Manager) Watch(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.reloadIfChanged()
		}
	}
}

// SetAllPaused sets (not toggles) the paused state of every running workspace
// in one reload. persist records the affected ids while the reload lock is
// held; if the reload or persist fails, every workspace keeps its previous
// state and the error is returned.
func (m *Manager) SetAllPaused(paused bool, persist func(ids []string) error) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.mu.Lock()
	ids := make([]string, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	previous := make(map[string]bool, len(m.paused))
	for k, v := range m.paused {
		previous[k] = v
	}
	restore := func() { m.paused = previous }
	for _, id := range ids {
		m.paused[id] = paused
	}
	if err := m.reload(); err != nil {
		restore()
		return err
	}
	if err := persist(ids); err != nil {
		restore()
		_ = m.reload()
		return err
	}
	return nil
}

// SuspendCapture drains this workspace's capture runtime before source deletion.
// Leases nest independently of the user's pause choice. No manager or archive
// lock is held while the caller performs network work.
func (m *Manager) SuspendCapture(id string) (func(), error) {
	m.reloadMu.Lock()
	m.suspended[id]++
	m.mu.Lock()
	active := m.running[id] != nil
	m.mu.Unlock()
	var err error
	if active {
		err = m.reload()
	}
	if err != nil {
		m.suspended[id]--
		m.reloadMu.Unlock()
		return nil, err
	}
	m.reloadMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.reloadMu.Lock()
			defer m.reloadMu.Unlock()
			m.suspended[id]--
			m.mu.Lock()
			active := m.running[id] != nil && !m.closed
			m.mu.Unlock()
			if !active {
				return
			}
			if err := m.reload(); err != nil {
				m.retry = true
				slog.Warn("capture resume failed; configuration reload will retry", "workspace", id)
			}
		})
	}, nil
}

func (m *Manager) SetPaused(id string, paused bool) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.paused[id] = paused
}

func (m *Manager) fileStats() [2]os.FileInfo {
	var stats [2]os.FileInfo
	stats[0], _ = os.Stat(m.Path)
	stats[1], _ = os.Stat(filepath.Join(filepath.Dir(m.Path), "secrets.env"))
	// A resolver may store secrets separately from the YAML.
	if s, ok := m.Secrets.(interface{ WatchPath() string }); ok {
		stats[1], _ = os.Stat(s.WatchPath())
	}
	return stats
}
func (m *Manager) reloadIfChanged() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.retry {
		if time.Now().Before(m.retryAt) {
			return
		}
		delay := m.retryDelay
		if err := m.reload(); err != nil {
			if delay == 0 {
				delay = time.Second
			}
			m.retryDelay = min(delay*2, time.Minute)
			m.retryAt = time.Now().Add(m.retryDelay)
			slog.Warn("capture configuration retry failed", "retry_in", m.retryDelay)
		}
		return
	}
	next := m.fileStats()
	for i, old := range m.watched {
		now := next[i]
		if (old == nil) != (now == nil) || old != nil && now != nil && (!os.SameFile(old, now) || old.Size() != now.Size() || !old.ModTime().Equal(now.ModTime())) {
			_ = m.reload()
			return
		}
	}
}

func (m *Manager) setError(message string) { m.mu.Lock(); m.Error = message; m.mu.Unlock() }

// BearerToken returns the active configuration's read-only bearer credential.
// Offline/empty startup still honors token_env without loading source secrets.
func (m *Manager) BearerToken() string {
	m.mu.Lock()
	token, generation := m.apiToken, m.generation
	m.mu.Unlock()
	if generation > 0 {
		return token
	}
	env := "CHATKEEPER_API_TOKEN"
	if cfg, e := config.Load(m.Path, false); e == nil {
		env = cfg.TokenEnv
	}
	token = os.Getenv(env)
	if token == "" && env == "CHATKEEPER_API_TOKEN" {
		token = os.Getenv("THREADKEEP_API_TOKEN")
	}
	return token
}

func (m *Manager) Read(serve func()) { m.readMu.RLock(); defer m.readMu.RUnlock(); serve() }

// IsPaused includes workspaces whose capture runtime has not started yet.
func (m *Manager) IsPaused(id string) bool {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.paused[id]
}

// IsSuspended reports a temporary cleanup lease, independently of user pause.
func (m *Manager) IsSuspended(id string) bool {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.suspended[id] > 0
}

// CheckNow shares the reload boundary so checks cannot target a drained runtime.
func (m *Manager) CheckNow(ctx context.Context, id string) core.CheckReport {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.suspended[id] > 0 {
		message := "Capture is paused while cleanup runs"
		return core.CheckReport{Workspace: id, Error: message, Hints: []string{message}}
	}
	m.mu.Lock()
	r := m.running[id]
	m.mu.Unlock()
	if r == nil {
		return core.CheckReport{Workspace: id, Error: "This workspace is not running. Check Settings for errors."}
	}
	return r.Runtime.CheckNow(ctx)
}
