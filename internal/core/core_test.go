package core

import (
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"testing"
	"time"
)

func TestTrigger(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule pluginapi.Trigger
		root pluginapi.Root
		want bool
		err  bool
	}{
		{"all", pluginapi.Trigger{Reactions: []string{"one", "two"}, Who: "any"}, pluginapi.Root{TS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 1}, {Name: "two", Count: 1}}}, true, false},
		{"missing", pluginapi.Trigger{Reactions: []string{"one", "two"}}, pluginapi.Root{TS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 1}}}, false, false},
		{"reply", pluginapi.Trigger{Reactions: []string{"one"}}, pluginapi.Root{TS: "2", ThreadTS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 1}}}, false, false},
		{"owner", pluginapi.Trigger{Reactions: []string{"one", "two"}, Who: "owner", Owners: []string{"UA"}}, pluginapi.Root{TS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 1, Users: []string{"UA"}}, {Name: "two", Count: 1, Users: []string{"UA"}}}}, true, false},
		{"different owners", pluginapi.Trigger{Reactions: []string{"one", "two"}, Who: "owner", Owners: []string{"UA", "UB"}}, pluginapi.Root{TS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 1, Users: []string{"UA"}}, {Name: "two", Count: 1, Users: []string{"UB"}}}}, false, false},
		{"incomplete", pluginapi.Trigger{Reactions: []string{"one"}, Who: "owner", Owners: []string{"UA"}}, pluginapi.Root{TS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 2, Users: []string{"UB"}}}}, false, true},
		{"duplicate", pluginapi.Trigger{Reactions: []string{"one"}}, pluginapi.Root{TS: "1", Reactions: []pluginapi.Reaction{{Name: "one", Count: 1}, {Name: "one", Count: 1}}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := Matches(tc.rule, tc.root)
			if got != tc.want || (e != nil) != tc.err {
				t.Fatal(got, e)
			}
		})
	}
}
func TestQueueBoundAndCoalescing(t *testing.T) {
	q := NewQueue(2)
	a := pluginapi.Event{Channel: "C1", RootTS: "1", Kind: "message"}
	if !q.Push(a) {
		t.Fatal("initial enqueue")
	}
	if !q.Push(a) {
		t.Fatal("duplicate not coalesced")
	}
	q.Push(pluginapi.Event{Channel: "C2", RootTS: "2"})
	if q.Push(pluginapi.Event{Channel: "C3", RootTS: "3"}) {
		t.Fatal("overflow")
	}
	q.Push(pluginapi.Event{Channel: "C1", RootTS: "1", Kind: "reaction"})
	if q.Len() != 3 {
		t.Fatal(q.Len())
	}
	e, err := q.Pop(context.Background())
	if err != nil || e.Kind != "reaction" {
		t.Fatal(e, err)
	}
}
func TestGuardPanicAndHang(t *testing.T) {
	if e := Guard(context.Background(), time.Second, func(context.Context) error { panic("secret") }); e != ErrPanic {
		t.Fatal(e)
	}
	done := make(chan struct{})
	defer close(done)
	if e := Guard(context.Background(), 10*time.Millisecond, func(context.Context) error { <-done; return nil }); e != ErrTimeout {
		t.Fatal(e)
	}
}
func TestBackfillQueueCoalescing(t *testing.T) {
	q := NewQueue(2)
	for i := 0; i < 5; i++ {
		if !q.Push(pluginapi.Event{Kind: "backfill"}) || !q.Push(pluginapi.Event{Channel: "CFIXTURE", RootTS: "100.000001", Kind: "reaction"}) {
			t.Fatal("duplicate occupied capacity")
		}
	}
	if q.Len() != 2 {
		t.Fatal(q.Len())
	}
	e, _ := q.Pop(context.Background())
	if e.Kind != "backfill" {
		t.Fatal(e)
	}
	e, _ = q.Pop(context.Background())
	if e.Kind != "reaction" {
		t.Fatal(e)
	}
	q.Push(pluginapi.Event{Kind: "backfill"})
	if q.Len() != 1 {
		t.Fatal(q.Len())
	}
}

func TestBackfillReservedUnderDistinctFlood(t *testing.T) {
	q := NewQueue(2)
	q.Push(pluginapi.Event{Channel: "C1", RootTS: "1"})
	q.Push(pluginapi.Event{Channel: "C2", RootTS: "2"})
	if !q.Push(pluginapi.Event{Kind: "backfill"}) {
		t.Fatal("backfill refused under flood")
	}
}

func TestQueueKeepsReplyItemWhenCoalescing(t *testing.T) {
	q := NewQueue(4)
	q.Push(pluginapi.Event{Channel: "C1", RootTS: "1.0", Kind: "reaction", ItemTS: "2.0"})
	q.Push(pluginapi.Event{Channel: "C1", RootTS: "1.0", Kind: "reaction"})
	e, err := q.Pop(context.Background())
	if err != nil || e.ItemTS != "2.0" || q.Len() != 1 {
		t.Fatal(e, err)
	}
}

func TestQueuePreservesDistinctReplyCandidates(t *testing.T) {
	q := NewQueue(4)
	for _, item := range []string{"2.0", "3.0", "2.0"} {
		q.Push(pluginapi.Event{Channel: "C1", RootTS: "1.0", Kind: "reaction", ItemTS: item})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seen := map[string]bool{}
	for q.Len() > 0 {
		e, err := q.Pop(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen[e.ItemTS] = true
	}
	if !seen["2.0"] || !seen["3.0"] || len(seen) != 2 {
		t.Fatal("reply candidates overwritten", seen)
	}
}
