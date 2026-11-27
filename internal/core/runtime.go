// Package core coordinates platform-neutral capture. Hints never authorize a save:
// root reactions are fetched again after each complete snapshot.
package core

import (
	"context"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"log/slog"
	"sync"
	"time"
)

type Health struct {
	State, ErrorClass string
	LastCapture       time.Time
	Realtime          bool
}
type Runtime struct {
	OnCapture                         func(pluginapi.Key)
	OnArchive                         func(pluginapi.Key)
	Workspace                         pluginapi.Workspace
	Plugin                            pluginapi.Plugin
	Env                               pluginapi.Env
	Store                             storage.Store
	Queue                             *Queue
	HookTimeout, StopTimeout, Backoff time.Duration
	mu                                sync.RWMutex
	health                            Health
	marked                            map[pluginapi.Key]bool
	active                            map[string]bool
	watermarks                        map[string]time.Time
	lastFull                          time.Time
}

func New(ws pluginapi.Workspace, p pluginapi.Plugin, env pluginapi.Env, store storage.Store) *Runtime {
	scoped := pluginapi.Credentials{}
	for k, v := range env.Credentials {
		scoped[k] = v
	}
	env.Credentials = scoped
	env.Log = pluginapi.RedactingLogger(env.Log, scoped)
	return &Runtime{Workspace: ws, Plugin: p, Env: env, Store: store, Queue: NewQueue(1024), HookTimeout: 30 * time.Second, StopTimeout: 10 * time.Second, Backoff: 5 * time.Second, marked: map[pluginapi.Key]bool{}, active: map[string]bool{}, watermarks: map[string]time.Time{}}
}
func (r *Runtime) Health() Health { r.mu.RLock(); defer r.mu.RUnlock(); return r.health }
func (r *Runtime) state(state, class string) {
	r.mu.Lock()
	r.health.State = state
	r.health.ErrorClass = class
	r.mu.Unlock()
}
func class(e error) string {
	switch {
	case errors.Is(e, ErrPanic):
		return "panic"
	case errors.Is(e, ErrTimeout):
		return "timeout"
	default:
		return "capture"
	}
}
func (r *Runtime) now() time.Time {
	if r.Env.Now != nil {
		return r.Env.Now()
	}
	return time.Now()
}
func (r *Runtime) Refresh(ctx context.Context, s pluginapi.Session, k pluginapi.Key) (bool, error) {
	root, e := Call(ctx, r.HookTimeout, func(c context.Context) (pluginapi.Root, error) { return s.Root(c, k.Channel, k.RootTS) })
	if e != nil {
		return false, e
	}
	previous, e := r.Store.Read(ctx, k)
	if e != nil {
		return false, e
	}
	if root.TS == "" {
		return r.observeMissing(ctx, k, previous)
	}
	matched, e := Matches(r.Workspace.Trigger, root)
	if e != nil {
		return false, e
	}
	if !matched && previous == nil {
		delete(r.marked, k)
		return false, nil
	}
	snapshot, e := Call(ctx, r.HookTimeout, func(c context.Context) (pluginapi.Snapshot, error) { return s.Snapshot(c, k.Channel, root, previous) })
	if e != nil {
		return false, e
	}
	e = Guard(ctx, r.HookTimeout, func(c context.Context) error { return r.Plugin.Content().Validate(c, snapshot) })
	if e != nil {
		return false, e
	}
	root, e = Call(ctx, r.HookTimeout, func(c context.Context) (pluginapi.Root, error) { return s.Root(c, k.Channel, k.RootTS) })
	if e != nil {
		return false, e
	}
	if root.TS == "" {
		return r.observeMissing(ctx, k, previous)
	}
	matched, e = Matches(r.Workspace.Trigger, root)
	if e != nil {
		return false, e
	}
	if !matched && previous == nil {
		delete(r.marked, k)
		return false, nil
	}
	changed, e := r.Store.Save(ctx, k, snapshot)
	if e != nil {
		return false, e
	}
	r.marked[k] = true
	if r.OnArchive != nil {
		r.OnArchive(k)
	}
	if changed {
		if r.OnCapture != nil {
			r.OnCapture(k)
		}
		r.mu.Lock()
		r.health.LastCapture = r.now()
		r.mu.Unlock()
		logger := r.Env.Log
		if logger == nil {
			logger = slog.Default()
		}
		logger.Info("thread archived", "workspace", r.Workspace.ID, "channel", k.Channel, "root", k.RootTS)
	}
	return changed, nil
}
func fatal(e error) bool { return errors.Is(e, ErrPanic) || errors.Is(e, ErrTimeout) }
func (r *Runtime) Backfill(ctx context.Context, s pluginapi.Session) (int, error) {
	caps, e := Call(ctx, r.HookTimeout, func(context.Context) (pluginapi.Capabilities, error) { return s.Capabilities(), nil })
	if e != nil {
		return 0, e
	}
	if !caps.ReadHistory {
		return 0, nil
	}
	if r.Workspace.AutoChannels {
		channels, e := Call(ctx, r.HookTimeout, s.Channels)
		if e != nil {
			return 0, e
		}
		r.active = map[string]bool{}
		for _, ch := range channels {
			r.active[ch.ID] = true
		}
	} else {
		for _, ch := range r.Workspace.Channels {
			r.active[ch] = true
		}
	}
	keys, e := r.Store.Keys(ctx)
	if e != nil {
		return 0, e
	}
	now := r.now()
	repairKeys, repairGeneration := r.Queue.Repairs()
	full := repairGeneration > 0 || r.lastFull.IsZero() || now.Sub(r.lastFull) >= time.Hour
	changed := 0
	var lastErr error
	for channel := range r.active {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		oldest := ""
		if !full {
			water := r.watermarks[channel].Add(-5 * time.Minute)
			if water.Unix() > 0 {
				oldest = fmt.Sprintf("%d.%06d", water.Unix(), water.Nanosecond()/1000)
			}
		}
		roots, e := Call(ctx, r.HookTimeout, func(c context.Context) ([]pluginapi.RootRef, error) { return s.History(c, channel, oldest) })
		if fatal(e) {
			return changed, e
		}
		if e != nil {
			lastErr = e
		} else {
			r.watermarks[channel] = now
		}
		set := map[pluginapi.Key]bool{}
		for _, k := range roots {
			if err := ctx.Err(); err != nil {
				return changed, err
			}
			if k.Channel == channel {
				// History is only a candidate filter; Refresh re-reads authority.
				rule := r.Workspace.Trigger
				rule.Who = "any"
				matched, matchErr := Matches(rule, pluginapi.Root{TS: k.RootTS, Reactions: k.Reactions})
				if matchErr != nil {
					lastErr = matchErr
					continue
				}
				if matched {
					set[pluginapi.Key{Channel: k.Channel, RootTS: k.RootTS}] = true
				}
			}
		}
		for _, k := range append(keys, repairKeys...) {
			if err := ctx.Err(); err != nil {
				return changed, err
			}
			if k.Channel == channel {
				set[k] = true
			}
		}
		for k := range set {
			if err := ctx.Err(); err != nil {
				return changed, err
			}
			ok, e := r.Refresh(ctx, s, k)
			if fatal(e) {
				return changed, e
			}
			if e != nil {
				lastErr = e
				continue
			}
			if ok {
				changed++
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return changed, err
	}
	if full && lastErr == nil {
		r.lastFull = now
	}
	if lastErr == nil {
		r.Queue.Repaired(repairGeneration)
	}
	return changed, lastErr
}
func (r *Runtime) Run(ctx context.Context) {
	delay := r.Backoff
	for ctx.Err() == nil {
		r.state("starting", "")
		e := r.runOnce(ctx)
		if r.Health().State == "quarantined" {
			return
		}
		if ctx.Err() != nil {
			if r.Health().State != "quarantined" {
				r.state("stopped", "")
			}
			return
		}
		if errors.Is(e, ErrTimeout) {
			r.state("quarantined", "timeout")
			return
		}
		r.state("failed", class(e))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Minute)
	}
}
func (r *Runtime) runOnce(ctx context.Context) error {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	s, e := Call(child, r.HookTimeout, func(c context.Context) (pluginapi.Session, error) { return r.Plugin.NewSession(c, r.Workspace, r.Env) })
	if e != nil {
		return e
	}
	var started <-chan struct{}
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), r.StopTimeout)
		defer stopCancel()
		if Guard(stopCtx, r.StopTimeout, s.Stop) != nil {
			r.state("quarantined", "stop")
		}
		if started != nil {
			select {
			case <-started:
			case <-stopCtx.Done():
				r.state("quarantined", "start")
			}
		}
	}()
	_, e = Call(child, r.HookTimeout, s.VerifyIdentity)
	if e != nil {
		return e
	}
	caps, e := Call(child, r.HookTimeout, func(context.Context) (pluginapi.Capabilities, error) { return s.Capabilities(), nil })
	if e != nil {
		return e
	}
	r.mu.Lock()
	r.health.Realtime = caps.Realtime
	r.mu.Unlock()
	r.state("running", "")
	realtime := make(chan error, 1)
	if caps.Realtime {
		startDone := make(chan struct{})
		started = startDone
		go func() {
			defer close(startDone)
			var startErr error
			defer func() {
				if recover() != nil {
					startErr = ErrPanic
				}
				realtime <- startErr
			}()
			startErr = s.Start(child, r.enqueue)
			if startErr != nil {
				startErr = ErrPlugin
			}
		}()
	}
	interval := r.Workspace.PollInterval
	if interval <= 0 {
		interval = 120 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.Queue.Push(pluginapi.Event{Kind: "backfill"})
	// One consumer owns mutable capture state. Timer enqueueing stays responsive to floods.
	go func() {
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				r.Queue.Push(pluginapi.Event{Kind: "backfill"})
			}
		}
	}()
	for {
		select {
		case e := <-realtime:
			if e == nil {
				e = ErrPlugin
			}
			return e
		default:
		}
		popCtx, popCancel := context.WithTimeout(child, 100*time.Millisecond)
		event, e := r.Queue.Pop(popCtx)
		popCancel()
		if e != nil {
			if child.Err() != nil {
				return child.Err()
			}
			continue
		}
		if event.Kind == "backfill" {
			_, e = r.Backfill(child, s)
		} else {
			key := pluginapi.Key{Channel: event.Channel, RootTS: event.RootTS}
			if !r.active[key.Channel] || event.Kind != "reaction" && !r.marked[key] {
				continue
			}
			_, e = r.Refresh(child, s, key)
		}
		if fatal(e) {
			return e
		}
		if e != nil {
			r.state("running", class(e))
		} else {
			r.state("running", "")
		}
	}
}

type Supervisor struct{ Runtimes []*Runtime }

func (s Supervisor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range s.Runtimes {
		wg.Add(1)
		go func() { defer wg.Done(); r.Run(ctx) }()
	}
	wg.Wait()
}

func (r *Runtime) enqueue(e pluginapi.Event) bool {
	ok := r.Queue.Push(e)
	if !ok {
		logger := r.Env.Log
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("capture queue overflow; full repair scheduled")
	}
	return ok
}

func (r *Runtime) observeMissing(ctx context.Context, k pluginapi.Key, previous *pluginapi.Snapshot) (bool, error) {
	if previous == nil {
		return false, nil
	}
	r.marked[k] = true
	missing := pluginapi.Snapshot{Manifest: previous.Manifest, Messages: []pluginapi.Message{}, Users: map[string]pluginapi.Message{}, Blobs: map[string]pluginapi.Blob{}}
	missing.Manifest.Schema = 2
	missing.Manifest.MessageCount = 0
	missing.Manifest.Files = []pluginapi.Attachment{}
	changed, e := r.Store.Save(ctx, k, missing)
	if e == nil && r.OnArchive != nil {
		r.OnArchive(k)
	}
	return changed, e
}
