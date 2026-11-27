package core

import (
	"context"
	"fmt"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"sort"
	"strings"
	"time"
)

// Activity is one recent runtime event shown on the dashboard.
type Activity struct {
	At         time.Time
	Kind, Text string
}

// CheckReport explains one on-demand full scan in platform-neutral terms.
type CheckReport struct {
	Workspace          string
	Started, Finished  time.Time
	Full               bool
	Realtime           bool
	RealtimeConnected  bool
	RealtimeDetail     string
	Interval           time.Duration
	LastPoll, NextPoll time.Time
	Trigger            pluginapi.Trigger
	Channels           []ChannelCheck
	Replies            []pluginapi.ReplyReaction
	Error              string
	Hints              []string
}

// ChannelCheck is the per-channel part of a CheckReport.
type ChannelCheck struct {
	ID, Name    string
	MemberKnown bool
	Member      bool
	Problem     string
	Scanned     int
	Roots       []RootCheck
	Omitted     int
	Error       string
}

// RootCheck lists a scanned root that carries any reaction.
type RootCheck struct {
	RootTS    string
	Reactions []string
	Matched   bool
	Result    string
}

const maxActivity = 50
const maxReportRoots = 50

type checkRequest struct {
	ctx  context.Context
	done chan CheckReport
}

func (r *Runtime) record(kind, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activity = append(r.activity, Activity{At: r.now(), Kind: kind, Text: text})
	if len(r.activity) > maxActivity {
		r.activity = append([]Activity(nil), r.activity[len(r.activity)-maxActivity:]...)
	}
}

// Activity returns recent runtime events, newest first.
func (r *Runtime) Activity() []Activity {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Activity, len(r.activity))
	for i, a := range r.activity {
		out[len(out)-1-i] = a
	}
	return out
}

func (r *Runtime) observeRealtime(s pluginapi.Session) {
	reporter, ok := s.(pluginapi.RealtimeReporter)
	if !ok {
		return
	}
	connected, detail := reporter.RealtimeStatus()
	r.mu.Lock()
	changed := connected != r.realtimeUp
	r.realtimeUp = connected
	r.health.RealtimeConnected = connected
	r.mu.Unlock()
	if !changed {
		return
	}
	if connected {
		r.record("realtime", "Live events connected")
	} else {
		r.record("realtime", strings.TrimSpace("Live events disconnected. "+detail))
	}
}

func (r *Runtime) interval() time.Duration {
	if r.Workspace.PollInterval > 0 {
		return r.Workspace.PollInterval
	}
	return 120 * time.Second
}

// CheckNow asks the running capture loop for an immediate full scan and returns a
// report explaining what was seen. It never runs concurrently with capture.
func (r *Runtime) CheckNow(ctx context.Context) CheckReport {
	report := CheckReport{Workspace: r.Workspace.ID, Trigger: r.Workspace.Trigger, Started: r.now(), Interval: r.interval()}
	req := checkRequest{ctx: ctx, done: make(chan CheckReport, 1)}
	if h := r.Health(); r.checks == nil || h.State != "running" {
		report.Error = "Capture is not running"
		if h.State != "" {
			report.Error += " (state: " + h.State
			if h.ErrorClass != "" {
				report.Error += ", last error: " + h.ErrorClass
			}
			report.Error += ")"
		}
		report.Error += ". It retries automatically; check again shortly or see Settings."
		report.hints()
		return report
	}
	select {
	case r.checks <- req:
	case <-ctx.Done():
		h := r.Health()
		report.Error = fmt.Sprintf("Capture did not respond (state: %s). It retries automatically; check again shortly.", h.State)
		if h.ErrorClass != "" {
			report.Error = fmt.Sprintf("Capture did not respond (state: %s, last error: %s). It retries automatically; check again shortly.", h.State, h.ErrorClass)
		}
		report.hints()
		return report
	}
	select {
	case report = <-req.done:
	case <-ctx.Done():
		report.Error = "The check is still running; results will appear in recent activity."
		report.hints()
	}
	return report
}

func (r *Runtime) check(parent context.Context, s pluginapi.Session, req checkRequest) (CheckReport, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(req.ctx, cancel)
	defer stop()
	r.mu.RLock()
	report := CheckReport{Workspace: r.Workspace.ID, Trigger: r.Workspace.Trigger, Started: r.now(), Interval: r.interval(), LastPoll: r.lastPoll, Realtime: r.health.Realtime}
	r.mu.RUnlock()
	if !report.LastPoll.IsZero() {
		report.NextPoll = report.LastPoll.Add(report.Interval)
	}
	if reporter, ok := s.(pluginapi.RealtimeReporter); ok {
		report.RealtimeConnected, report.RealtimeDetail = reporter.RealtimeStatus()
	}
	_, e := r.backfill(ctx, s, &report)
	report.Finished = r.now()
	if e != nil {
		report.Error = "Some reads failed: " + e.Error()
	}
	report.hints()
	r.record("check", fmt.Sprintf("Check now: %d channels, %d hints", len(report.Channels), len(report.Hints)))
	return report, e
}

func (c *CheckReport) channel(id string) *ChannelCheck {
	for i := range c.Channels {
		if c.Channels[i].ID == id {
			return &c.Channels[i]
		}
	}
	c.Channels = append(c.Channels, ChannelCheck{ID: id})
	return &c.Channels[len(c.Channels)-1]
}

func (r *Runtime) inspectChannels(ctx context.Context, s pluginapi.Session, channels []string, trace *CheckReport) {
	for _, id := range channels {
		trace.channel(id)
	}
	inspector, ok := s.(pluginapi.ChannelInspector)
	if !ok || len(channels) == 0 {
		return
	}
	statuses, e := Call(ctx, r.HookTimeout, func(c context.Context) ([]pluginapi.ChannelStatus, error) {
		return inspector.InspectChannels(c, channels)
	})
	if e != nil {
		return
	}
	for _, st := range statuses {
		ch := trace.channel(st.ID)
		ch.Name, ch.Member, ch.MemberKnown, ch.Problem = st.Name, st.Member, true, st.Problem
	}
}

func (r *Runtime) inspectReplies(ctx context.Context, s pluginapi.Session, roots []pluginapi.RootRef, trace *CheckReport) {
	inspector, ok := s.(pluginapi.ReplyInspector)
	if !ok {
		return
	}
	threads := []pluginapi.RootRef{}
	for _, root := range roots {
		if root.Replies > 0 {
			threads = append(threads, root)
		}
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].RootTS > threads[j].RootTS })
	if len(threads) > 10 {
		threads = threads[:10]
	}
	if len(threads) == 0 {
		return
	}
	found, e := Call(ctx, r.HookTimeout, func(c context.Context) ([]pluginapi.ReplyReaction, error) {
		return inspector.ReplyReactions(c, threads, r.Workspace.Trigger.Reactions)
	})
	if e == nil {
		trace.Replies = append(trace.Replies, found...)
	}
}

func (c *ChannelCheck) addRoot(k pluginapi.RootRef, matched bool) {
	if len(c.Roots) >= maxReportRoots {
		c.Omitted++
		return
	}
	names := []string{}
	for _, reaction := range k.Reactions {
		names = append(names, fmt.Sprintf(":%s: ×%d", reaction.Name, reaction.Count))
	}
	result := "not matched"
	if matched {
		result = "matched"
	}
	c.Roots = append(c.Roots, RootCheck{RootTS: k.RootTS, Reactions: names, Matched: matched, Result: result})
}

func (c *ChannelCheck) result(root string, changed, archived bool, e error) {
	var entry *RootCheck
	for i := range c.Roots {
		if c.Roots[i].RootTS == root {
			entry = &c.Roots[i]
		}
	}
	if entry == nil {
		if e == nil && !changed {
			// Previously archived threads are re-checked silently when unchanged.
			return
		}
		c.Roots = append(c.Roots, RootCheck{RootTS: root})
		entry = &c.Roots[len(c.Roots)-1]
	}
	switch {
	case e != nil:
		entry.Result = "archive failed: " + e.Error()
	case changed:
		entry.Result = "archived"
	case archived:
		entry.Result = "already archived (unchanged)"
	default:
		entry.Result = "not archived: the trigger did not match when re-checked"
	}
}

func colons(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ":" + n + ":"
	}
	return strings.Join(out, ", ")
}

func (c *CheckReport) hints() {
	add := func(format string, args ...any) { c.Hints = append(c.Hints, fmt.Sprintf(format, args...)) }
	if c.Error != "" {
		add("%s", c.Error)
	}
	rule := "any of " + colons(c.Trigger.Reactions)
	if !MatchAny(c.Trigger) && len(c.Trigger.Reactions) > 1 {
		rule = "all of " + colons(c.Trigger.Reactions)
	}
	if c.Started.IsZero() || c.Finished.IsZero() {
		return
	}
	if len(c.Channels) == 0 {
		add("No channels are being watched. Invite ChatKeeper to a channel or choose channels in setup.")
	}
	triggered, otherNames := false, map[string]bool{}
	required := map[string]bool{}
	for _, n := range c.Trigger.Reactions {
		required[n] = true
	}
	for _, ch := range c.Channels {
		label := ch.ID
		if ch.Name != "" {
			label = "#" + ch.Name + " (" + ch.ID + ")"
		}
		if ch.MemberKnown && !ch.Member {
			add("ChatKeeper is not a member of %s. Invite it to the channel, then check again.", label)
		}
		if ch.Problem != "" {
			add("%s: %s", label, ch.Problem)
		}
		if ch.Error != "" {
			add("Could not read history in %s: %s", label, ch.Error)
		}
		for _, root := range ch.Roots {
			has := false
			for _, raw := range root.Reactions {
				name, _, _ := strings.Cut(strings.TrimPrefix(raw, ":"), ":")
				if required[name] {
					has = true
				} else if name != "" {
					otherNames[name] = true
				}
			}
			if has {
				triggered = true
				if !root.Matched {
					add("Thread %s in %s has some trigger reactions, but the rule needs %s.", root.RootTS, label, rule)
				}
			}
			if strings.HasPrefix(root.Result, "archive failed") || strings.HasPrefix(root.Result, "not archived") {
				add("Thread %s in %s: %s", root.RootTS, label, root.Result)
			}
		}
	}
	matchedRoots := map[string]bool{}
	for _, ch := range c.Channels {
		for _, root := range ch.Roots {
			matchedRoots[ch.ID+"/"+root.RootTS] = root.Matched
		}
	}
	for _, reply := range c.Replies {
		if matchedRoots[reply.Channel+"/"+reply.RootTS] {
			continue
		}
		add("Found :%s: on a reply (%s) in thread %s. Only reactions on the first message of a thread count; add it there.", reply.Name, reply.TS, reply.RootTS)
	}
	if !triggered && len(c.Channels) > 0 && len(c.Replies) == 0 {
		if len(otherNames) > 0 {
			names := []string{}
			for n := range otherNames {
				names = append(names, n)
			}
			sort.Strings(names)
			add("Found reactions %s, but none is in your trigger list (%s).", colons(names), colons(c.Trigger.Reactions))
		} else {
			add("No reactions found on thread roots in the watched channels. Add %s to the first message of a thread.", rule)
		}
	}
	if !c.Realtime {
		next := ""
		if !c.NextPoll.IsZero() {
			next = fmt.Sprintf(" The last automatic check ran at %s; the next is due around %s.", c.LastPoll.UTC().Format(time.TimeOnly+" UTC"), c.NextPoll.UTC().Format(time.TimeOnly+" UTC"))
		}
		add("Live events are off (polling only): automatic checks run every %s.%s Check now scans immediately.", c.Interval, next)
	} else if !c.RealtimeConnected && c.RealtimeDetail != "" {
		add("Live events are configured but not connected: %s Polling still runs every %s.", c.RealtimeDetail, c.Interval)
	}
}
