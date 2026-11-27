package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type fixturePlugin struct {
	pluginapi.Plugin
	session pluginapi.Session
	hooks   pluginapi.ContentHooks
}

func (p fixturePlugin) NewSession(context.Context, pluginapi.Workspace, pluginapi.Env) (pluginapi.Session, error) {
	return p.session, nil
}
func (p fixturePlugin) Content() pluginapi.ContentHooks { return p.hooks }

type fixtureHooks struct {
	pluginapi.ContentHooks
	hang <-chan struct{}
}

func (h fixtureHooks) Validate(context.Context, pluginapi.Snapshot) error {
	if h.hang != nil {
		<-h.hang
	}
	return nil
}

type fixtureSession struct {
	pluginapi.Session
	mode        string
	snapshot    *pluginapi.Snapshot
	floodReady  chan struct{}
	rootEntered chan struct{}
	history     atomic.Int64
	rootCalls   atomic.Int64
	oldest      atomic.Value
	hang        <-chan struct{}
	failHistory atomic.Bool
	remove      atomic.Bool
	badSnapshot atomic.Bool
}

func (s *fixtureSession) Capabilities() pluginapi.Capabilities {
	return pluginapi.Capabilities{ReadHistory: true, Realtime: s.mode != "poll"}
}
func (s *fixtureSession) VerifyIdentity(context.Context) (pluginapi.Identity, error) {
	return pluginapi.Identity{TeamID: "TFIXTURE"}, nil
}
func (s *fixtureSession) Channels(context.Context) ([]pluginapi.Channel, error) {
	return []pluginapi.Channel{{ID: "CFIXTURE"}}, nil
}
func (s *fixtureSession) History(_ context.Context, ch, oldest string) ([]pluginapi.RootRef, error) {
	s.history.Add(1)
	if s.floodReady != nil {
		<-s.floodReady
	}
	if s.mode == "startHang" {
		panic("synthetic hook failure")
	}
	s.oldest.Store(oldest)
	if s.mode == "historyHang" {
		<-s.hang
	}
	if s.failHistory.Load() {
		return nil, ErrPlugin
	}
	return []pluginapi.RootRef{{Channel: ch, RootTS: "100.000001", Reactions: []pluginapi.Reaction{{Name: "mark", Count: 1}}}}, nil
}
func (s *fixtureSession) Root(ctx context.Context, _ string, _ string) (pluginapi.Root, error) {
	n := s.rootCalls.Add(1)
	if s.rootEntered != nil {
		if n == 1 {
			close(s.rootEntered)
		}
		<-ctx.Done()
		return pluginapi.Root{}, ctx.Err()
	}
	if s.remove.Load() {
		return pluginapi.Root{}, nil
	}
	return pluginapi.Root{TS: "100.000001", Reactions: []pluginapi.Reaction{{Name: "mark", Count: 1}}}, nil
}
func (s *fixtureSession) Snapshot(context.Context, string, pluginapi.Root, *pluginapi.Snapshot) (pluginapi.Snapshot, error) {
	if s.snapshot != nil {
		return *s.snapshot, nil
	}
	if s.badSnapshot.Load() {
		return pluginapi.Snapshot{}, ErrPlugin
	}
	src, e := storage.Open("../storage/testdata/golden")
	if e != nil {
		return pluginapi.Snapshot{}, e
	}
	defer src.Close()
	v, e := src.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	if e != nil {
		return pluginapi.Snapshot{}, e
	}
	return *v, nil
}
func (s *fixtureSession) Start(ctx context.Context, sink pluginapi.EventSink) error {
	switch s.mode {
	case "panic", "panicStopHang":
		panic("synthetic private payload")
	case "startHang":
		<-s.hang
	case "flood":
		// History waits until this burst fills the ordinary and reserved slots.
		for i := 0; i < 2048; i++ {
			sink(pluginapi.Event{Channel: "CFIXTURE", RootTS: fmt.Sprintf("%d.000001", 100+i), Kind: "reaction"})
		}
		close(s.floodReady)
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s *fixtureSession) Stop(context.Context) error {
	if s.mode == "stopHang" || s.mode == "panicStopHang" {
		<-s.hang
	}
	return nil
}
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func makeRuntime(t *testing.T, s *fixtureSession, h fixtureHooks) *Runtime {
	t.Helper()
	store, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	ws := pluginapi.Workspace{ID: "fixture", Channels: []string{"CFIXTURE"}, Trigger: pluginapi.Trigger{Reactions: []string{"mark"}}, PollInterval: 10 * time.Millisecond}
	r := New(ws, fixturePlugin{session: s, hooks: h}, pluginapi.Env{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, store)
	r.HookTimeout = time.Second
	r.StopTimeout = 20 * time.Millisecond
	r.Backoff = 10 * time.Millisecond
	return r
}
func TestWorkspaceIsolation(t *testing.T) {
	// Load disk fixtures before entering fake time; flock retries involve external processes.
	src, err := storage.Open("../storage/testdata/golden")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := src.Read(context.Background(), pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"})
	src.Close()
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"panic", "historyHang", "hookHang", "flood", "stopHang", "startHang"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				bad := &fixtureSession{mode: mode, hang: release, snapshot: snapshot}
				hooks := fixtureHooks{}
				if mode == "flood" {
					bad.floodReady = make(chan struct{})
					bad.rootEntered = make(chan struct{})
				}
				if mode == "hookHang" {
					hooks.hang = release
				}
				a := makeRuntime(t, bad, hooks)
				a.HookTimeout = 20 * time.Millisecond
				good := &fixtureSession{mode: "poll", snapshot: snapshot}
				b := makeRuntime(t, good, fixtureHooks{})
				b.HookTimeout = time.Second
				captured := make(chan struct{})
				b.OnCapture = func(pluginapi.Key) { close(captured) }
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { defer close(done); Supervisor{Runtimes: []*Runtime{a, b}}.Run(ctx) }()
				if mode == "flood" {
					<-captured
					<-bad.rootEntered
					if a.Health().State != "running" {
						t.Fatal("flood repair timed out before cancellation", a.Health())
					}
				} else {
					waitFor(t, func() bool { return good.history.Load() >= 3 && !b.Health().LastCapture.IsZero() })
				}
				if mode == "historyHang" || mode == "hookHang" || mode == "startHang" {
					waitFor(t, func() bool { return a.Health().State == "quarantined" })
				}
				if mode == "flood" {
					<-bad.rootEntered
					if _, generation := a.Queue.Repairs(); generation == 0 {
						t.Fatal("flood did not overflow")
					}
				}
				if a.Queue.Len() > 1025 {
					t.Fatal("queue unbounded")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("supervisor hung")
				}
				synctest.Wait()
				if mode == "flood" && bad.rootCalls.Load() != 1 {
					t.Fatal("repair continued after cancellation", bad.rootCalls.Load())
				}
				if mode == "stopHang" && a.Health().State != "quarantined" {
					t.Fatal(a.Health())
				}
			})
		})
	}
}
func TestBackfillRepairsAndIncremental(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	now := time.Unix(1800000000, 0)
	r.Env.Now = func() time.Time { return now }
	if n, e := r.Backfill(context.Background(), s); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	now = now.Add(time.Minute)
	if n, e := r.Backfill(context.Background(), s); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if s.oldest.Load() != "1799999700.000000" {
		t.Fatal(s.oldest.Load())
	}
	s.failHistory.Store(true)
	before := s.rootCalls.Load()
	if _, e := r.Backfill(context.Background(), s); e == nil || s.rootCalls.Load() <= before {
		t.Fatal("history failure skipped old archive")
	}
	s.failHistory.Store(false)
	now = now.Add(time.Hour)
	r.Backfill(context.Background(), s)
	if s.oldest.Load() != "" {
		t.Fatal("full repair missing")
	}
}
func TestTriggerRemovalDuringSnapshot(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	proxy := removingSession{fixtureSession: s}
	if changed, e := r.Refresh(context.Background(), proxy, pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}); e != nil || changed {
		t.Fatal(changed, e)
	}
	keys, _ := r.Store.Keys(context.Background())
	if len(keys) != 0 {
		t.Fatal(keys)
	}
}

type removingSession struct{ *fixtureSession }

func (s removingSession) Snapshot(ctx context.Context, ch string, root pluginapi.Root, prev *pluginapi.Snapshot) (pluginapi.Snapshot, error) {
	v, e := s.fixtureSession.Snapshot(ctx, ch, root, prev)
	s.remove.Store(true)
	return v, e
}
func TestCaptureFailureRetainsOld(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	if _, e := r.Refresh(context.Background(), s, k); e != nil {
		t.Fatal(e)
	}
	old, _ := r.Store.Read(context.Background(), k)
	before, _ := json.Marshal(old.Manifest)
	s.badSnapshot.Store(true)
	if _, e := r.Refresh(context.Background(), s, k); e == nil {
		t.Fatal("failure accepted")
	}
	current, _ := r.Store.Read(context.Background(), k)
	after, _ := json.Marshal(current.Manifest)
	if string(before) != string(after) {
		t.Fatal("old archive replaced")
	}
}
func TestHistorySkipsUnmarkedCandidates(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	session := unmarkedHistory{fixtureSession: s}
	if n, e := r.Backfill(context.Background(), session); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if s.rootCalls.Load() != 0 {
		t.Fatal("unmarked history fetched roots")
	}
}

type unmarkedHistory struct{ *fixtureSession }

func (s unmarkedHistory) History(context.Context, string, string) ([]pluginapi.RootRef, error) {
	return []pluginapi.RootRef{{Channel: "CFIXTURE", RootTS: "100.000001"}}, nil
}
func TestStopTimeoutQuarantinesFailedStart(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := &fixtureSession{mode: "panicStopHang", hang: release}
	r := makeRuntime(t, s, fixtureHooks{})
	var created atomic.Int64
	r.Plugin = countingPlugin{fixturePlugin: fixturePlugin{session: s, hooks: fixtureHooks{}}, created: &created}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if created.Load() != 1 || r.Health().State != "quarantined" {
		t.Fatal("restarted after hung Stop", created.Load(), r.Health())
	}
}

type countingPlugin struct {
	fixturePlugin
	created *atomic.Int64
}

func (p countingPlugin) NewSession(c context.Context, w pluginapi.Workspace, e pluginapi.Env) (pluginapi.Session, error) {
	p.created.Add(1)
	return p.fixturePlugin.NewSession(c, w, e)
}
func TestUnsupportedHistoryAndUnmarkedEvents(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	if n, e := r.Backfill(context.Background(), noHistory{s}); e != nil || n != 0 || s.history.Load() != 0 {
		t.Fatal(n, e)
	}
	r.Workspace.PollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	waitFor(t, func() bool { return !r.Health().LastCapture.IsZero() })
	before := s.rootCalls.Load()
	r.Queue.Push(pluginapi.Event{Channel: "CFIXTURE", RootTS: "900.000001", Kind: "message"})
	r.Queue.Push(pluginapi.Event{Channel: "CFOREIGN", RootTS: "100.000001", Kind: "reaction"})
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	if s.rootCalls.Load() != before {
		t.Fatal("unwatched or unmarked event fetched thread")
	}
}

type noHistory struct{ *fixtureSession }

func (noHistory) Capabilities() pluginapi.Capabilities { return pluginapi.Capabilities{} }
func TestCaptureLogsIdentityOnlyOnce(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	var out strings.Builder
	r.Env.Log = pluginapi.RedactingLogger(slog.New(slog.NewTextHandler(&out, nil)), pluginapi.Credentials{"bot_token": "synthetic-sensitive"})
	if _, e := r.Backfill(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Backfill(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	log := out.String()
	if strings.Count(log, "thread archived") != 1 || !strings.Contains(log, "CFIXTURE") || !strings.Contains(log, "100.000001") || strings.Contains(log, "Synthetic") || strings.Contains(log, "sensitive") {
		t.Fatal(log)
	}
}
func TestStartHangQuarantinesOnFailure(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := &fixtureSession{mode: "startHang", hang: release}
	r := makeRuntime(t, s, fixtureHooks{})
	var created atomic.Int64
	r.Plugin = countingPlugin{fixturePlugin: fixturePlugin{session: s, hooks: fixtureHooks{}}, created: &created}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if created.Load() != 1 || r.Health().State != "quarantined" {
		t.Fatal("restarted while Start still alive", created.Load(), r.Health())
	}
}

func TestMissingRootRetainsArchive(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	if _, e := r.Refresh(context.Background(), s, k); e != nil {
		t.Fatal(e)
	}
	old, _ := r.Store.Read(context.Background(), k)
	s.remove.Store(true)
	if changed, e := r.Refresh(context.Background(), s, k); e != nil || !changed {
		t.Fatal(changed, e)
	}
	got, e := r.Store.Read(context.Background(), k)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Messages) != len(old.Messages) || got.Messages[0]["source_deleted_at"] == nil {
		t.Fatal("missing root dropped archive")
	}
}
func TestFailedFullBackfillRetriesFull(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	s.failHistory.Store(true)
	if _, e := r.Backfill(context.Background(), s); e == nil {
		t.Fatal("expected history failure")
	}
	if !r.lastFull.IsZero() {
		t.Fatal("failed sweep advanced lastFull")
	}
	s.failHistory.Store(false)
	if _, e := r.Backfill(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if s.oldest.Load() != "" {
		t.Fatal("retry was incremental")
	}
}
func TestOverflowLogsAndRepairsDroppedKey(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	r.Queue = NewQueue(1)
	var out strings.Builder
	r.Env.Log = slog.New(slog.NewTextHandler(&out, nil))
	r.enqueue(pluginapi.Event{Channel: "CFIXTURE", RootTS: "200.000001", Kind: "reaction"})
	if r.enqueue(pluginapi.Event{Channel: "CFIXTURE", RootTS: "100.000001", Kind: "reaction"}) {
		t.Fatal("overflow accepted")
	}
	keys, generation := r.Queue.Repairs()
	if len(keys) != 1 || generation == 0 || !strings.Contains(out.String(), "repair scheduled") {
		t.Fatal("drop untracked", keys, generation, out.String())
	}
	if _, e := r.Backfill(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	keys, generation = r.Queue.Repairs()
	if len(keys) != 0 || generation != 0 {
		t.Fatal("repair not cleared")
	}
}

func TestArchivedReappearanceWithoutTrigger(t *testing.T) {
	s := &fixtureSession{mode: "poll"}
	r := makeRuntime(t, s, fixtureHooks{})
	k := pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}
	if _, e := r.Refresh(context.Background(), s, k); e != nil {
		t.Fatal(e)
	}
	s.remove.Store(true)
	if _, e := r.Refresh(context.Background(), s, k); e != nil {
		t.Fatal(e)
	}
	s.remove.Store(false)
	if _, e := r.Refresh(context.Background(), unmarkedRoot{s}, k); e != nil {
		t.Fatal(e)
	}
	got, e := r.Store.Read(context.Background(), k)
	if e != nil {
		t.Fatal(e)
	}
	if got.Messages[0]["source_deleted_at"] != nil || got.Messages[0]["source_events"] == nil {
		t.Fatal("unmarked reappearance ignored")
	}
}

type unmarkedRoot struct{ *fixtureSession }

func (s unmarkedRoot) Root(context.Context, string, string) (pluginapi.Root, error) {
	return pluginapi.Root{TS: "100.000001"}, nil
}

// Cancellation during repair must stop before touching the remaining dropped keys.
func TestBackfillCancellationStopsRepair(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &fixtureSession{mode: "poll"}
		r := makeRuntime(t, s, fixtureHooks{})
		r.Queue = NewQueue(32)
		for i := 0; i < 64; i++ {
			r.Queue.Push(pluginapi.Event{Channel: "CFIXTURE", RootTS: fmt.Sprintf("%d.000001", 200+i), Kind: "reaction"})
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		session := cancelRepairSession{fixtureSession: s, cancel: cancel}
		_, err := r.Backfill(ctx, session)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
		synctest.Wait()
		if got := s.rootCalls.Load(); got != 1 {
			t.Fatalf("continued repair after cancellation: %d root calls", got)
		}
		_, generation := r.Queue.Repairs()
		if generation == 0 {
			t.Fatal("canceled repair cleared pending drops")
		}
	})
}

type cancelRepairSession struct {
	*fixtureSession
	cancel context.CancelFunc
}

func (s cancelRepairSession) Root(context.Context, string, string) (pluginapi.Root, error) {
	s.rootCalls.Add(1)
	s.cancel()
	return pluginapi.Root{}, context.Canceled
}

func TestCallCanceledContextSkipsHook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int64
		_, err := Call(ctx, time.Second, func(context.Context) (int, error) {
			calls.Add(1)
			return 1, nil
		})
		synctest.Wait()
		if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
			t.Fatalf("canceled call: err=%v hooks=%d", err, calls.Load())
		}
	})
}
