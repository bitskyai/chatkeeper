package core

import (
	"context"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"strings"
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
	if e := Guard(context.Background(), time.Second, func(context.Context) error { panic("secret") }); !errors.Is(e, ErrPanic) || e.Error() != ErrPanic.Error() {
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

func TestPluginErrorKeepsRedactedCause(t *testing.T) {
	e := Guard(context.Background(), time.Second, func(context.Context) error {
		return errors.New("users.info failed: Authorization: Bearer xoxb-1-2-secretvalue at https://api.example.invalid/x?token=hidden")
	})
	if !errors.Is(e, ErrPlugin) || errors.Is(e, ErrTimeout) || errors.Is(e, ErrPanic) {
		t.Fatal(e)
	}
	msg := e.Error()
	if !strings.HasPrefix(msg, "plugin failure: users.info failed") || strings.Contains(msg, "secretvalue") || strings.Contains(msg, "hidden") {
		t.Fatal(msg)
	}
	// A plugin cannot impersonate core sentinels to force a quarantine.
	if e := Guard(context.Background(), time.Second, func(context.Context) error { return ErrTimeout }); errors.Is(e, ErrTimeout) || !errors.Is(e, ErrPlugin) {
		t.Fatal(e)
	}
	if e := Guard(context.Background(), time.Second, func(context.Context) error { return context.Canceled }); e != context.Canceled {
		t.Fatal(e)
	}
	// Runtime faults keep their description; arbitrary panic values are dropped.
	e = Guard(context.Background(), time.Second, func(context.Context) error {
		list := func() []int { return nil }
		return errors.New(fmt.Sprint(list()[1]))
	})
	if !errors.Is(e, ErrPanic) || !strings.Contains(e.Error(), "index out of range") {
		t.Fatal(e)
	}
	r := &Runtime{Env: pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": "custom-credential-value"}}}
	if got := r.Cause(errors.New("rejected custom-credential-value")); strings.Contains(got, "custom-credential-value") {
		t.Fatal(got)
	}
}

func TestCauseRemovesCredentialBeforeTruncation(t *testing.T) {
	secret := "custom-credential-value-0123"
	r := &Runtime{Env: pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": secret}}}
	// Place the credential so it straddles the MaxErrorDetail boundary.
	for off := pluginapi.MaxErrorDetail - len(secret); off <= pluginapi.MaxErrorDetail+2; off++ {
		pad := strings.Repeat("a ", off/2) + strings.Repeat("b", off%2)
		e := Guard(context.Background(), time.Second, func(context.Context) error { return errors.New(pad + secret + " tail") })
		got := r.Cause(e)
		for n := 6; n <= len(secret); n++ {
			if strings.Contains(got, secret[:n]) {
				t.Fatalf("offset %d leaked %q in %q", off, secret[:n], got)
			}
		}
		if wrapped := r.Cause(fmt.Errorf("history: %w", e)); strings.Contains(wrapped, secret[:6]) || !strings.HasPrefix(wrapped, "history: plugin failure") {
			t.Fatalf("wrapped offset %d: %q", off, wrapped)
		}
	}
}

func TestCauseRemovesCredentialAfterWhitespaceCollapse(t *testing.T) {
	secret := "custom-credential-value-0123"
	r := &Runtime{Env: pluginapi.Env{Credentials: pluginapi.Credentials{"bot_token": secret}}}
	for _, n := range []int{65526, 65530, 65536, 1 << 20, (1 << 20) - 10} {
		e := Guard(context.Background(), time.Second, func(context.Context) error { return errors.New(strings.Repeat(" ", n) + secret + " tail") })
		got := r.Cause(e)
		for k := 6; k <= len(secret); k++ {
			if strings.Contains(got, secret[:k]) {
				t.Fatalf("pad %d leaked %q in %q", n, secret[:k], got)
			}
		}
		if !strings.HasPrefix(got, "plugin failure") {
			t.Fatalf("pad %d: %q", n, got)
		}
	}
}
