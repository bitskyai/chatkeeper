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
	"sort"
	"strings"
	"sync"
	"time"
)

type Health struct {
	State, ErrorClass string
	// LastError is the redacted cause of the most recent failure, if any.
	LastError         string
	LastCapture       time.Time
	Realtime          bool
	RealtimeConnected bool
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
	lastPoll                          time.Time
	checks                            chan checkRequest
	activity                          []Activity
	realtimeUp                        bool
	// failures remembers the last reported cause per thread so a persistent
	// failure is logged and shown once per change, not on every poll.
	failures map[pluginapi.Key]string
	// gaps holds items the last capture of a thread could not store.
	gaps        map[pluginapi.Key][]pluginapi.Gap
	lastPollErr string
}

func New(ws pluginapi.Workspace, p pluginapi.Plugin, env pluginapi.Env, store storage.Store) *Runtime {
	scoped := pluginapi.Credentials{}
	for k, v := range env.Credentials {
		scoped[k] = v
	}
	env.Credentials = scoped
	env.Log = pluginapi.RedactingLogger(env.Log, scoped)
	return &Runtime{Workspace: ws, Plugin: p, Env: env, Store: store, Queue: NewQueue(1024), HookTimeout: 30 * time.Second, StopTimeout: 10 * time.Second, Backoff: 5 * time.Second, marked: map[pluginapi.Key]bool{}, failures: map[pluginapi.Key]string{}, gaps: map[pluginapi.Key][]pluginapi.Gap{}, active: map[string]bool{}, watermarks: map[string]time.Time{}, checks: make(chan checkRequest)}
}
func (r *Runtime) Health() Health { r.mu.RLock(); defer r.mu.RUnlock(); return r.health }

// Cause renders an error for users and logs: credential values of this
// workspace and credential-shaped text are removed and the length is bounded.
func (r *Runtime) Cause(e error) string {
	if e == nil {
		return ""
	}
	secrets := make([]string, 0, len(r.Env.Credentials))
	for _, v := range r.Env.Credentials {
		secrets = append(secrets, v)
	}
	// Remove this workspace's credentials from the untruncated plugin cause,
	// then truncate: truncating first could keep a credential's prefix.
	var pe *PluginError
	if errors.As(e, &pe) && pe.detail != "" {
		if msg := e.Error(); strings.HasSuffix(msg, pe.Error()) {
			return pluginapi.Redact(strings.TrimSuffix(msg, pe.Error())+ErrPlugin.Error()+": "+pe.detail, secrets...)
		}
	}
	return pluginapi.Redact(e.Error(), secrets...)
}
func (r *Runtime) logger() *slog.Logger {
	if r.Env.Log != nil {
		return r.Env.Log
	}
	return slog.Default()
}
func (r *Runtime) setLastError(cause string) {
	r.mu.Lock()
	r.health.LastError = cause
	r.mu.Unlock()
}

// noteResult logs and records a thread capture failure once per distinct cause,
// and records a successful capture that stored only part of the thread.
func (r *Runtime) noteResult(ctx context.Context, k pluginapi.Key, changed bool, e error) {
	result := "unchanged"
	if changed {
		result = "archived"
	}
	if e != nil {
		result = "failed"
	}
	r.logger().Info("thread archive result", "workspace", r.Workspace.ID, "channel", k.Channel, "root", k.RootTS, "result", result)

	if e != nil {
		if ctx.Err() != nil || errors.Is(e, context.Canceled) {
			return
		}
		cause := r.Cause(e)
		r.setLastError(cause)
		if r.failures[k] == cause {
			return
		}
		r.failures[k] = cause
		r.logger().Warn("thread archive failed", "workspace", r.Workspace.ID, "channel", k.Channel, "root", k.RootTS, "error", cause)
		r.record("error", "Archive failed for "+k.Channel+" / "+k.RootTS+": "+cause)
		return
	}
	if _, failed := r.failures[k]; failed {
		delete(r.failures, k)
		r.logger().Info("thread archive recovered", "workspace", r.Workspace.ID, "channel", k.Channel, "root", k.RootTS)
	}
	if gaps := r.gaps[k]; changed && len(gaps) > 0 {
		summary := GapSummary(gaps)
		r.logger().Warn("thread archived with missing items", "workspace", r.Workspace.ID, "channel", k.Channel, "root", k.RootTS, "missing", len(gaps), "detail", summary)
		r.record("archive", "Archived thread "+k.Channel+" / "+k.RootTS+"; "+summary)
	}
}

// GapSummary describes missing archive items in one bounded line.
func GapSummary(gaps []pluginapi.Gap) string {
	if len(gaps) == 0 {
		return ""
	}
	parts := []string{}
	for i, g := range gaps {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(gaps)-i))
			break
		}
		item := g.Kind
		if g.ID != "" {
			item += " " + g.ID
		}
		parts = append(parts, item+" ("+g.Reason+")")
	}
	noun := "item"
	if len(gaps) != 1 {
		noun = "items"
	}
	return pluginapi.Redact(fmt.Sprintf("%d %s not captured: %s", len(gaps), noun, strings.Join(parts, "; ")))
}
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
	return r.refresh(ctx, s, k, "")
}
func (r *Runtime) refresh(ctx context.Context, s pluginapi.Session, k pluginapi.Key, item string) (bool, error) {
	changed, e := r.capture(ctx, s, k, item)
	r.noteResult(ctx, k, changed, e)
	return changed, e
}

// triggered applies the rule to the root and, for a realtime reaction on a
// reply (item), to that reply as re-read from the source. Polling passes no
// item, so it only ever matches reactions on the root.
func (r *Runtime) triggered(ctx context.Context, s pluginapi.Session, k pluginapi.Key, item string, root pluginapi.Root) (bool, error) {
	matched, e := Matches(r.Workspace.Trigger, root)
	if e != nil || matched || item == "" || item == k.RootTS {
		return matched, e
	}
	reader, ok := s.(pluginapi.ItemReader)
	if !ok {
		return false, nil
	}
	reply, e := Call(ctx, r.HookTimeout, func(c context.Context) (pluginapi.Root, error) { return reader.Item(c, k.Channel, k.RootTS, item) })
	if e != nil || reply.TS != item {
		return false, e
	}
	return Matches(r.Workspace.Trigger, pluginapi.Root{TS: k.RootTS, Reactions: reply.Reactions})
}
func (r *Runtime) capture(ctx context.Context, s pluginapi.Session, k pluginapi.Key, item string) (bool, error) {
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
	matched, e := r.triggered(ctx, s, k, item, root)
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
	matched, e = r.triggered(ctx, s, k, item, root)
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
	if len(snapshot.Manifest.Gaps) > 0 {
		r.gaps[k] = snapshot.Manifest.Gaps
	} else {
		delete(r.gaps, k)
	}
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
		r.logger().Info("thread archived", "workspace", r.Workspace.ID, "channel", k.Channel, "root", k.RootTS, "missing_items", len(snapshot.Manifest.Gaps))
		r.record("archive", "Archived thread "+k.Channel+" / "+k.RootTS)
	}
	return changed, nil
}
func fatal(e error) bool { return errors.Is(e, ErrPanic) || errors.Is(e, ErrTimeout) }
func (r *Runtime) Backfill(ctx context.Context, s pluginapi.Session) (int, error) {
	return r.backfill(ctx, s, nil)
}

// RecentWindow bounds incremental polls. A reaction can be added to a root of any
// age, and history reads filter by post time, so each poll rescans roots posted in
// this window; older roots are rescanned by the hourly full pass or Check now.
const RecentWindow = 24 * time.Hour

func (r *Runtime) backfill(ctx context.Context, s pluginapi.Session, trace *CheckReport) (int, error) {
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
	full := trace != nil || repairGeneration > 0 || r.lastFull.IsZero() || now.Sub(r.lastFull) >= time.Hour
	changed := 0
	var lastErr error
	channels := make([]string, 0, len(r.active))
	for channel := range r.active {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	if trace != nil {
		trace.Full = full
		r.inspectChannels(ctx, s, channels, trace)
	}
	scanned, candidates := 0, 0
	for _, channel := range channels {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		var report *ChannelCheck
		if trace != nil {
			report = trace.channel(channel)
		}
		oldest := ""
		if !full {
			water := r.watermarks[channel].Add(-5 * time.Minute)
			if recent := now.Add(-RecentWindow); recent.Before(water) {
				water = recent
			}
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
			if report != nil {
				report.Error = r.Cause(e)
			}
		} else {
			r.watermarks[channel] = now
		}
		scanned += len(roots)
		if report != nil {
			report.Scanned = len(roots)
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
				if report != nil && len(k.Reactions) > 0 {
					report.addRoot(k, matched)
				}
				if matchErr != nil {
					lastErr = matchErr
					continue
				}
				if matched {
					set[pluginapi.Key{Channel: k.Channel, RootTS: k.RootTS}] = true
				}
			}
		}
		if trace != nil {
			r.inspectReplies(ctx, s, roots, trace)
		}
		candidates += len(set)
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return changed, err
			}
			if k.Channel == channel {
				set[k] = true
			}
		}
		// Overflow repair must re-read reply authority too: history only carries root reactions.
		for _, event := range repairKeys {
			if event.Channel != channel || event.ItemTS == "" {
				continue
			}
			k := pluginapi.Key{Channel: event.Channel, RootTS: event.RootTS}
			ok, err := r.refresh(ctx, s, k, event.ItemTS)
			if fatal(err) {
				return changed, err
			}
			if err != nil {
				lastErr = err
			} else if ok {
				changed++
			}
		}
		for _, event := range repairKeys {
			if event.Channel == channel && event.ItemTS == "" {
				set[pluginapi.Key{Channel: event.Channel, RootTS: event.RootTS}] = true
			}
		}
		for k := range set {
			if err := ctx.Err(); err != nil {
				return changed, err
			}
			ok, e := r.Refresh(ctx, s, k)
			if report != nil {
				report.result(k.RootTS, ok, r.marked[k], r.Cause(e), r.gaps[k])
			}
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
	r.mu.Lock()
	r.lastPoll = now
	r.mu.Unlock()
	summary := fmt.Sprintf("Checked %d channels, %d thread roots, %d matched, %d archived or updated", len(channels), scanned, candidates, changed)
	if lastErr != nil {
		cause := r.Cause(lastErr)
		summary += "; some reads failed: " + cause
		r.setLastError(cause)
		if cause != r.lastPollErr {
			r.logger().Warn("poll had failures", "workspace", r.Workspace.ID, "error", cause)
		}
		r.lastPollErr = cause
	} else {
		r.lastPollErr = ""
		r.setLastError("")
	}
	r.record("poll", summary)
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
		if e != nil {
			cause := r.Cause(e)
			r.setLastError(cause)
			r.logger().Warn("workspace session failed", "workspace", r.Workspace.ID, "class", class(e), "error", cause)
			r.record("error", "Session failed ("+class(e)+"): "+cause)
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
				if v := recover(); v != nil {
					startErr = containPanic(v)
				}
				realtime <- startErr
			}()
			startErr = Contain(s.Start(child, r.enqueue))
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
				e = &PluginError{Cause: "live event connection ended"}
			}
			return e
		case req := <-r.checks:
			report, e := r.check(child, s, req)
			req.done <- report
			if fatal(e) {
				return e
			}
			continue
		default:
		}
		r.observeRealtime(s)
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
			_, e = r.refresh(child, s, key, event.ItemTS)
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
		r.logger().Warn("capture queue overflow; full repair scheduled")
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
