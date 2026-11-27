package core

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// pollSession models a history API that filters by post time, the way Slack's
// conversations.history "oldest" does, while reactions can be added at any time.
type pollSession struct {
	*fixtureSession
	marked       atomic.Bool
	name         string
	replyMarked  bool
	notMember    bool
	historyCalls atomic.Int64
}

func (s *pollSession) History(_ context.Context, ch, oldest string) ([]pluginapi.RootRef, error) {
	s.historyCalls.Add(1)
	if oldest != "" {
		min, _ := strconv.ParseFloat(oldest, 64)
		if 100.000001 < min {
			return nil, nil
		}
	}
	ref := pluginapi.RootRef{Channel: ch, RootTS: "100.000001", Replies: 1}
	if s.marked.Load() {
		ref.Reactions = []pluginapi.Reaction{{Name: s.name, Count: 1}}
	}
	return []pluginapi.RootRef{ref}, nil
}
func (s *pollSession) Root(context.Context, string, string) (pluginapi.Root, error) {
	root := pluginapi.Root{TS: "100.000001"}
	if s.marked.Load() {
		root.Reactions = []pluginapi.Reaction{{Name: s.name, Count: 1}}
	}
	return root, nil
}
func (s *pollSession) InspectChannels(_ context.Context, ids []string) ([]pluginapi.ChannelStatus, error) {
	out := []pluginapi.ChannelStatus{}
	for _, id := range ids {
		out = append(out, pluginapi.ChannelStatus{ID: id, Name: "fixture", Member: !s.notMember})
	}
	return out, nil
}
func (s *pollSession) ReplyReactions(_ context.Context, roots []pluginapi.RootRef, names []string) ([]pluginapi.ReplyReaction, error) {
	if !s.replyMarked {
		return nil, nil
	}
	return []pluginapi.ReplyReaction{{Channel: roots[0].Channel, RootTS: roots[0].RootTS, TS: "101.000001", Name: names[0]}}, nil
}

// Regression for the live failure: in polling-only mode a reaction added to a
// root posted before the previous poll was never seen until the hourly full scan.
func TestPollingSeesReactionOnOlderRoot(t *testing.T) {
	s := &pollSession{fixtureSession: &fixtureSession{mode: "poll"}, name: "mark"}
	r := makeRuntime(t, s.fixtureSession, fixtureHooks{})
	now := time.Unix(100+3600, 0) // the root was posted an hour before setup
	r.Env.Now = func() time.Time { return now }
	if n, e := r.Backfill(context.Background(), s); e != nil || n != 0 {
		t.Fatal("initial full scan", n, e)
	}
	s.marked.Store(true)
	now = now.Add(2 * time.Minute)
	if n, e := r.Backfill(context.Background(), s); e != nil || n != 1 {
		t.Fatalf("incremental poll missed a reaction on an older root: %d %v", n, e)
	}
}

func TestPollingWindowAndCheckNowFullScan(t *testing.T) {
	s := &pollSession{fixtureSession: &fixtureSession{mode: "poll"}, name: "mark"}
	r := makeRuntime(t, s.fixtureSession, fixtureHooks{})
	now := time.Unix(100+3*86400, 0) // root older than the recent window
	r.Env.Now = func() time.Time { return now }
	r.Backfill(context.Background(), s)
	s.marked.Store(true)
	now = now.Add(2 * time.Minute)
	if n, _ := r.Backfill(context.Background(), s); n != 0 {
		t.Fatal("expected older-than-window roots to wait for a full scan")
	}
	report, e := r.check(context.Background(), s, checkRequest{ctx: context.Background()})
	if e != nil || !report.Full || len(report.Channels) != 1 {
		t.Fatal(report, e)
	}
	ch := report.Channels[0]
	if ch.Name != "fixture" || !ch.MemberKnown || !ch.Member || ch.Scanned != 1 || len(ch.Roots) != 1 || ch.Roots[0].Result != "archived" || ch.Roots[0].Reactions[0] != ":mark: ×1" {
		t.Fatalf("%+v", ch)
	}
	keys, _ := r.Store.Keys(context.Background())
	if len(keys) != 1 {
		t.Fatal("check now did not archive", keys)
	}
	if !strings.Contains(strings.Join(report.Hints, "\n"), "polling only") {
		t.Fatal(report.Hints)
	}
	if a := r.Activity(); len(a) == 0 || a[0].Kind != "check" {
		t.Fatal("activity", a)
	}
}

func TestCheckNowExplainsNoMatch(t *testing.T) {
	cases := []struct {
		name               string
		reaction           string
		marked, reply, out bool
		want               string
	}{
		{name: "not member", out: true, want: "not a member of #fixture (CFIXTURE)"},
		{name: "wrong reaction", reaction: "heart", marked: true, want: "Found reactions :heart:, but none is in your trigger list (:mark:)"},
		{name: "reply", reaction: "mark", reply: true, want: "Found :mark: on a reply (101.000001) in thread 100.000001"},
		{name: "nothing", reaction: "mark", want: "No reactions found on thread roots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &pollSession{fixtureSession: &fixtureSession{mode: "poll"}, name: tc.reaction, replyMarked: tc.reply, notMember: tc.out}
			s.marked.Store(tc.marked)
			r := makeRuntime(t, s.fixtureSession, fixtureHooks{})
			report, _ := r.check(context.Background(), s, checkRequest{ctx: context.Background()})
			if got := strings.Join(report.Hints, "\n"); !strings.Contains(got, tc.want) {
				t.Fatalf("missing %q in\n%s", tc.want, got)
			}
		})
	}
}

func TestMatchAnyAndAll(t *testing.T) {
	root := pluginapi.Root{TS: "1.1", Reactions: []pluginapi.Reaction{{Name: "a", Count: 1, Users: []string{"UOWNER"}}}}
	for _, tc := range []struct {
		rule pluginapi.Trigger
		want bool
	}{
		{pluginapi.Trigger{Reactions: []string{"a", "b"}}, false},
		{pluginapi.Trigger{Reactions: []string{"a", "b"}, Match: "all"}, false},
		{pluginapi.Trigger{Reactions: []string{"a", "b"}, Match: "any"}, true},
		{pluginapi.Trigger{Reactions: []string{"b", "c"}, Match: "any"}, false},
		{pluginapi.Trigger{Reactions: []string{"a", "b"}, Match: "any", Who: "owner", Owners: []string{"UOWNER"}}, true},
		{pluginapi.Trigger{Reactions: []string{"a", "b"}, Match: "any", Who: "owner", Owners: []string{"UOTHER"}}, false},
	} {
		if got, e := Matches(tc.rule, root); e != nil || got != tc.want {
			t.Errorf("%+v: %v %v", tc.rule, got, e)
		}
	}
}

func TestCheckNowThroughRunLoop(t *testing.T) {
	s := &pollSession{fixtureSession: &fixtureSession{mode: "poll"}, name: "mark"}
	s.marked.Store(true)
	r := makeRuntime(t, s.fixtureSession, fixtureHooks{})
	r.Plugin = fixturePlugin{session: s, hooks: fixtureHooks{}}
	r.Workspace.PollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitFor(t, func() bool { return r.Health().State == "running" })
	checkCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	report := r.CheckNow(checkCtx)
	if report.Error != "" || len(report.Channels) != 1 || report.Finished.IsZero() {
		t.Fatalf("%+v", report)
	}
}

func TestCheckNowFailsFastWhenNotRunning(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	report := r.CheckNow(ctx)
	if time.Since(start) > time.Second || !strings.Contains(report.Error, "not running") || len(report.Hints) == 0 {
		t.Fatalf("%+v", report)
	}
}
