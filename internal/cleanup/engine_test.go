//go:build !windows

package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type driver struct {
	calls     []string
	check     func() error
	fail      bool
	deleteErr error
}

func (d *driver) Verify(context.Context, pluginapi.ArchiveManifest) error   { return nil }
func (d *driver) Reason(context.Context, pluginapi.Message) (string, error) { return "", nil }
func (d *driver) Check(context.Context, pluginapi.ArchiveManifest, []pluginapi.Message, []string) error {
	if d.check != nil {
		return d.check()
	}
	return nil
}
func (d *driver) Delete(_ context.Context, _ pluginapi.ArchiveManifest, m pluginapi.Message) error {
	d.calls = append(d.calls, m["ts"].(string))
	if d.deleteErr != nil {
		return d.deleteErr
	}
	if d.fail {
		return errors.New("unknown outcome")
	}
	return nil
}
func (d *driver) Pace(context.Context) error { return nil }
func fixture(t *testing.T) (pluginapi.Workspace, *storage.Folder) {
	t.Helper()
	s, e := storage.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	w := pluginapi.Workspace{ID: "fixture", Platform: "fixture", Storage: s.Path(), Channels: []string{"CFAKE"}, Trigger: pluginapi.Trigger{Who: "owner", Owners: []string{"UOWNER"}, Reactions: []string{"save"}}, Cleanup: pluginapi.CleanupPolicy{Enabled: true, MinAge: 24 * time.Hour}}
	v := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, Platform: "fixture", TeamID: "TFAKE", Channel: "CFAKE", RootTS: "1000000000.000001", MessageCount: 3}, Messages: []pluginapi.Message{{"ts": "1000000000.000001", "user": "UOWN", "text": "root", "reply_count": 2, "reactions": []any{map[string]any{"name": "save", "users": []any{"UOWNER"}, "count": 1}}}, {"ts": "1000000001.000001", "thread_ts": "1000000000.000001", "user": "UOWN", "text": "reply"}, {"ts": "1000000002.000001", "thread_ts": "1000000000.000001", "user": "UOWN", "text": "last"}}, Users: map[string]pluginapi.Message{"UOWN": {"id": "UOWN"}}, Blobs: map[string]pluginapi.Blob{}}
	_, e = s.Save(context.Background(), pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}, v)
	if e != nil {
		t.Fatal(e)
	}
	return w, s
}
func TestCleanupKeepsArchiveAfterRefresh(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	before, _ := s.Read(ctx, k)
	d := &driver{}
	r := Run(ctx, w, s, d, true)
	if r.Threads[0].Status != "deleted" || len(d.calls) != 3 {
		t.Fatalf("%+v %v", r, d.calls)
	}
	if d.calls[0] != "1000000002.000001" || d.calls[2] != k.RootTS {
		t.Fatal(d.calls)
	}
	fresh := pluginapi.Snapshot{Manifest: before.Manifest, Users: before.Users, Blobs: map[string]pluginapi.Blob{}}
	fresh.Manifest.Schema = 2
	fresh.Manifest.MessageCount = 0
	if _, e := s.Save(ctx, k, fresh); e != nil {
		t.Fatal(e)
	}
	after, e := s.Read(ctx, k)
	if e != nil {
		t.Fatal(e)
	}
	if len(after.Messages) != 3 || len(after.SourceDeletedAt) != 3 {
		t.Fatalf("archive shrank or unmarked: %+v", after)
	}
	if after.Messages[2]["source_deleted_at"] == nil {
		t.Fatal("receipt lost")
	}
	Run(ctx, w, s, d, true)
	if len(d.calls) != 3 {
		t.Fatal("rerun deleted again")
	}
}
func TestOfflineGates(t *testing.T) {
	for _, tc := range []string{"disabled", "trigger", "owner-missing", "platform", "channel", "recent", "tamper", "journal", "driver"} {
		t.Run(tc, func(t *testing.T) {
			w, s := fixture(t)
			d := &driver{}
			switch tc {
			case "disabled":
				w.Cleanup.Enabled = false
			case "trigger":
				w.Trigger.Who = "any"
			case "owner-missing":
				w.Trigger.Owners = []string{"UNOMATCH"}
			case "platform":
				w.Platform = "other"
			case "channel":
				w.Channels = []string{"COTHER"}
			case "recent":
				w.Cleanup.MinAge = 1000000 * time.Hour
			case "tamper":
				os.WriteFile(filepath.Join(s.Path(), "archive/CFAKE/1000000000.000001/messages.json"), []byte("[]"), 0600)
			case "journal":
				os.MkdirAll(filepath.Join(s.Path(), "cleanup"), 0700)
				os.WriteFile(filepath.Join(s.Path(), "cleanup/CFAKE_1000000000.000001.json"), []byte(`{"version":1}`), 0600)
			case "driver":
				d.check = func() error { return errors.New("changed") }
			}
			apply := true
			r := Run(context.Background(), w, s, d, apply)
			if len(d.calls) != 0 {
				t.Fatal(d.calls)
			}
			if tc != "disabled" && r.Threads[0].Status != "blocked" && r.Threads[0].Status != "deferred" {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestUnknownDeletePendingResume(t *testing.T) {
	w, s := fixture(t)
	d := &driver{fail: true}
	r := Run(context.Background(), w, s, d, true)
	if r.Threads[0].Status != "unknown/pending" || len(d.calls) != 1 {
		t.Fatalf("%+v", r)
	}
	d.fail = false
	Run(context.Background(), w, s, d, true)
	if len(d.calls) != 4 {
		t.Fatal(d.calls)
	}
}

func TestGuardRehashesAfterLiveRead(t *testing.T) {
	w, s := fixture(t)
	d := &driver{check: func() error {
		return os.WriteFile(filepath.Join(s.Path(), "archive/CFAKE/1000000000.000001/messages.json"), []byte("[]"), 0600)
	}}
	r := Run(context.Background(), w, s, d, true)
	if len(d.calls) != 0 || r.Threads[0].Status != "blocked" {
		t.Fatalf("deleted with tampered backup: %+v %v", r, d.calls)
	}
}
func TestJournalRejectsInvalidProgress(t *testing.T) {
	targets := []string{"reply", "root"}
	for _, raw := range []string{`{"version":1}`, `{"version":1,"proof":"p","targets":["reply","root"],"done":["reply","reply"],"pending":null}`, `{"version":1,"proof":"p","targets":["reply","root"],"done":[],"pending":"foreign"}`, `{"version":1,"proof":"other","targets":["reply","root"],"done":[],"pending":null}`, `{"version":1,"proof":"p","targets":["reply","root"],"done":null,"pending":null}`} {
		if _, e := readJournal([]byte(raw), "p", targets); e == nil {
			t.Fatal("accepted malformed journal", raw)
		}
	}
}

func TestCleanupLockBlocksConcurrentRunner(t *testing.T) {
	w, s := fixture(t)
	other, e := storage.Open(s.Path())
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.WithCleanup(context.Background(), func(*storage.CleanupView) error { close(entered); <-release; return nil })
		close(done)
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d := &driver{}
	r := Run(ctx, w, other, d, true)
	close(release)
	<-done
	if len(d.calls) != 0 || r.Threads[0].Reason == "" {
		t.Fatalf("lock bypass %+v", r)
	}
}
func TestPerThreadErrorIsolation(t *testing.T) {
	w, s := fixture(t)
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	snap, _ := s.Read(context.Background(), k)
	copy := *snap
	copy.Documents = nil
	copy.Manifest.RootTS = "1000000010.000001"
	copy.Messages = []pluginapi.Message{{"ts": copy.Manifest.RootTS, "user": "UOWN", "text": "second", "reactions": snap.Messages[0]["reactions"]}}
	copy.Manifest.MessageCount = 1
	if _, e := s.Save(context.Background(), pluginapi.Key{Channel: "CFAKE", RootTS: copy.Manifest.RootTS}, copy); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(s.Path(), "archive/CFAKE/"+k.RootTS+"/users.json"), []byte("{}"), 0600)
	d := &driver{}
	r := Run(context.Background(), w, s, d, true)
	if len(r.Threads) != 2 || r.Threads[0].Status != "blocked" || r.Threads[1].Status != "deleted" || len(d.calls) != 1 {
		t.Fatalf("%+v %v", r, d.calls)
	}
}
func TestCleanupPinnedPendingAndProof(t *testing.T) {
	w, s := fixture(t)
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	snap, _ := s.Read(context.Background(), k)
	d := &driver{fail: true}
	Run(context.Background(), w, s, d, true)
	fresh := *snap
	fresh.Documents = nil
	fresh.Messages = append([]pluginapi.Message{}, snap.Messages...)
	fresh.Messages[0] = pluginapi.Message{"ts": k.RootTS, "user": "UOWN", "text": "changed", "reply_count": 2}
	if _, e := s.Save(context.Background(), k, fresh); e != nil {
		t.Fatal("partial snapshot did not reconcile", e)
	}
	p := filepath.Join(s.Path(), "archive/CFAKE/"+k.RootTS+"/manifest.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, ' '), 0600)
	d.fail = false
	r := Run(context.Background(), w, s, d, true)
	if len(d.calls) != 1 || r.Threads[0].Status != "blocked" {
		t.Fatalf("proof changed but resumed %+v", r)
	}
}
func TestApplyBoundRefusesNewProof(t *testing.T) {
	w, s := fixture(t)
	preview := Run(context.Background(), w, s, nil, false)
	p := filepath.Join(s.Path(), "archive/CFAKE/1000000000.000001/manifest.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, ' '), 0600)
	d := &driver{}
	r := ApplyBound(context.Background(), w, s, d, preview)
	if len(d.calls) != 0 || r.Threads[0].Status != "blocked" {
		t.Fatal(r)
	}
}

func TestNewestReplyAgeGate(t *testing.T) {
	w, s := fixture(t)
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	snap, _ := s.Read(context.Background(), k)
	fresh := *snap
	fresh.Documents = nil
	fresh.Messages = append([]pluginapi.Message{}, snap.Messages...)
	fresh.Messages = append(fresh.Messages, pluginapi.Message{"ts": fmt.Sprintf("%d.000001", time.Now().Unix()), "user": "UOWN", "thread_ts": k.RootTS, "text": "new reply"})
	fresh.Messages[0] = pluginapi.Message{"ts": k.RootTS, "user": "UOWN", "text": "root", "reply_count": 3, "reactions": snap.Messages[0]["reactions"]}
	fresh.Manifest.MessageCount = 4
	if _, e := s.Save(context.Background(), k, fresh); e != nil {
		t.Fatal(e)
	}
	d := &driver{}
	r := Run(context.Background(), w, s, d, true)
	if r.Threads[0].Status != "deferred" || len(d.calls) != 0 {
		t.Fatal(r)
	}
}
func TestBackupObjectsBlockWholeThread(t *testing.T) {
	for _, name := range []string{"messages.json", "users.json", "manifest.json", "files/FFILE.bin"} {
		for _, damage := range []string{"missing", "corrupt", "truncated"} {
			t.Run(name+"/"+damage, func(t *testing.T) {
				w, s := fixture(t)
				k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
				snap, _ := s.Read(context.Background(), k)
				snap.Documents = nil
				snap.Blobs["files/FFILE.bin"] = pluginapi.Blob{Data: []byte("abc"), Size: 3, SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}
				snap.Manifest.Files = []pluginapi.Attachment{{ID: "FFILE", Path: "files/FFILE.bin", Name: "file", Size: 3, SHA256: snap.Blobs["files/FFILE.bin"].SHA256}}
				if _, e := s.Save(context.Background(), k, *snap); e != nil {
					t.Fatal(e)
				}
				p := filepath.Join(s.Path(), "archive", k.Channel, k.RootTS, name)
				switch damage {
				case "missing":
					os.Remove(p)
				case "corrupt":
					b, _ := os.ReadFile(p)
					b[0] ^= 1
					os.WriteFile(p, b, 0600)
				case "truncated":
					os.WriteFile(p, nil, 0600)
				}
				d := &driver{}
				r := Run(context.Background(), w, s, d, true)
				if len(d.calls) != 0 || r.Threads[0].Status != "blocked" {
					t.Fatalf("backup damage accepted %+v", r)
				}
			})
		}
	}
}

func TestCleanupStatusReports(t *testing.T) {
	for _, status := range []string{"refused", "rate-limited"} {
		t.Run(status, func(t *testing.T) {
			w, s := fixture(t)
			d := &driver{deleteErr: &pluginapi.CleanupError{Status: status, Reason: "denied"}}
			r := Run(context.Background(), w, s, d, true)
			if len(d.calls) != 1 || r.Threads[0].Status != status || r.Threads[0].Results[0].Status != status || r.Threads[0].Results[2].Status != "blocked" {
				t.Fatalf("%+v", r)
			}
			b, _ := os.ReadFile(filepath.Join(s.Path(), "cleanup/CFAKE_1000000000.000001.json"))
			var j journal
			if json.Unmarshal(b, &j) != nil || len(j.Done) != 0 || j.Pending == nil || *j.Pending != "1000000002.000001" {
				t.Fatal("pending lost")
			}
		})
	}
}

type panicDriver struct{ driver }

func (*panicDriver) Verify(context.Context, pluginapi.ArchiveManifest) error {
	panic("network must not run")
}
func (*panicDriver) Reason(context.Context, pluginapi.Message) (string, error) {
	panic("identity must not run")
}
func TestDryRunNoDriverCalls(t *testing.T) {
	w, s := fixture(t)
	r := Run(context.Background(), w, s, &panicDriver{}, false)
	if len(r.Threads) != 1 || r.Threads[0].Status != "dry-run" || len(r.Threads[0].Results) != 3 {
		t.Fatal(r)
	}
	for _, result := range r.Threads[0].Results {
		if result.Status != "dry-run" || result.Reason == "" {
			t.Fatal(result)
		}
	}
}

func TestApplyBoundRejectsDeferredApproval(t *testing.T) {
	w, s := fixture(t)
	preview := Run(context.Background(), w, s, nil, false)
	preview.Threads[0].Status = "deferred"
	d := &driver{}
	RunResult := ApplyBound(context.Background(), w, s, d, preview)
	if len(d.calls) != 0 || RunResult.Threads[0].Status != "blocked" {
		t.Fatal(RunResult)
	}
	// A deferred preview must never become permission across the age boundary.
	w.Cleanup.MinAge = time.Hour
	realAge := threadAge
	defer func() { threadAge = realAge }()
	calls := 0
	threadAge = func(time.Time) time.Duration {
		calls++
		if calls <= 2 {
			return 0
		}
		return 2 * time.Hour
	}
	preview = Run(context.Background(), w, s, nil, false)
	r := ApplyBound(context.Background(), w, s, d, preview)
	if len(d.calls) != 0 || r.Threads[0].Status != "blocked" {
		t.Fatal(r, d.calls)
	}

}

func TestCleanupNewReplyReconciles(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			w, s := fixture(t)
			ctx := context.Background()
			k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
			before, _ := s.Read(ctx, k)
			d := &driver{}
			if partial {
				d.check = func() error {
					if len(d.calls) == 1 {
						return errors.New("stop")
					}
					return nil
				}
			}
			Run(ctx, w, s, d, true)
			after, e := s.Read(ctx, k)
			if e != nil {
				t.Fatal(e)
			}
			n := 3
			if partial {
				n = 1
			}
			marked := 0
			for _, m := range after.Messages {
				if m["source_deleted_at"] != nil {
					marked++
				}
			}
			if marked != n {
				t.Fatalf("projected markers=%d want %d", marked, n)
			}
			fresh := *before
			fresh.Documents = nil
			fresh.Messages = append([]pluginapi.Message{}, before.Messages...)
			if partial {
				fresh.Messages = fresh.Messages[:2]
			} else {
				fresh.Messages = nil
			}
			fresh.Messages = append(fresh.Messages, pluginapi.Message{"ts": "1000000003.000001", "thread_ts": k.RootTS, "user": "UOWN", "text": "new activity"})
			if !partial {
				fresh.Messages = append([]pluginapi.Message{before.Messages[0]}, fresh.Messages...)
			}
			fresh.Messages[0] = pluginapi.Message{"ts": k.RootTS, "user": "UOWN", "text": "root", "reply_count": 1, "reactions": before.Messages[0]["reactions"]}
			fresh.Manifest.MessageCount = len(fresh.Messages)
			fresh.Manifest.Schema = 2
			if _, e = s.Save(ctx, k, fresh); e != nil {
				t.Fatal("refresh", e)
			}
			got, e := s.Read(ctx, k)
			if e != nil || len(got.Messages) != 4 {
				t.Fatalf("reply lost: %v %+v", e, got)
			}
			if got.Messages[2]["source_deleted_at"] == nil {
				t.Fatal("deleted reply marker lost")
			}
		})
	}
}

func TestCleanupNetworkDoesNotHoldArchiveLock(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	d := &driver{check: func() error {
		readCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		other, e := storage.Open(s.Path())
		if e != nil {
			return e
		}
		defer other.Close()
		_, e = other.Read(readCtx, k)
		return e
	}}
	r := Run(ctx, w, s, d, true)
	if len(d.calls) != 3 {
		t.Fatal(r)
	}
}

func TestRateLimitStopsRemainingThreads(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	snap, _ := s.Read(ctx, k)
	next := *snap
	next.Documents = nil
	next.Manifest.RootTS = "1000000010.000001"
	next.Messages = []pluginapi.Message{{"ts": next.Manifest.RootTS, "user": "UOWN", "text": "other", "reactions": snap.Messages[0]["reactions"]}}
	next.Manifest.MessageCount = 1
	if _, e := s.Save(ctx, pluginapi.Key{Channel: k.Channel, RootTS: next.Manifest.RootTS}, next); e != nil {
		t.Fatal(e)
	}
	d := &driver{deleteErr: &pluginapi.CleanupError{Status: "rate-limited", Reason: "exhausted"}}
	r := Run(ctx, w, s, d, true)
	if len(d.calls) != 1 || len(r.Threads) != 1 || r.Reason == "" {
		t.Fatal(r, d.calls)
	}
}

func TestFullyCleanedPreview(t *testing.T) {
	w, s := fixture(t)
	Run(context.Background(), w, s, &driver{}, true)
	r := Run(context.Background(), w, s, nil, false)
	if r.Threads[0].Status != "cleaned" {
		t.Fatal(r)
	}
}

func TestPendingReceiptReconcilesAfterRefresh(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	before, _ := s.Read(ctx, k)
	d := &driver{fail: true}
	Run(ctx, w, s, d, true)
	preview := Run(ctx, w, s, nil, false)
	if preview.Threads[0].Status != "unknown/pending" || preview.Threads[0].Results[0].Status != "unknown/pending" {
		t.Fatal(preview)
	}
	// The API delete succeeded but its response/receipt was lost. A complete
	// source refresh now proves that pending message absent.
	fresh := *before
	fresh.Documents = nil
	fresh.Messages = append([]pluginapi.Message{}, before.Messages[:2]...)
	fresh.Manifest.Schema = 2
	fresh.Manifest.MessageCount = 2
	fresh.Messages[0] = pluginapi.Message{"ts": k.RootTS, "user": "UOWN", "text": "root", "reply_count": 1, "reactions": before.Messages[0]["reactions"]}
	if _, e := s.Save(ctx, k, fresh); e != nil {
		t.Fatal(e)
	}
	d.fail = false
	r := Run(ctx, w, s, d, true)
	if r.Threads[0].Status != "deleted" || len(d.calls) != 3 {
		t.Fatal(r, d.calls)
	}
	got, e := s.Read(ctx, k)
	if e != nil || len(got.SourceDeletedAt) != 3 {
		t.Fatal("lost receipts", e)
	}
}

func TestConfirmedReceiptsSurviveStaleCapture(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	fetched, _ := s.Read(ctx, k)
	d := &driver{}
	Run(ctx, w, s, d, true)
	// Capture fetched these messages before deletion but publishes after receipts.
	fetched.Documents = nil
	if _, e := s.Save(ctx, k, *fetched); e != nil {
		t.Fatal(e)
	}
	got, e := s.Read(ctx, k)
	if e != nil || len(got.SourceDeletedAt) != 3 {
		t.Fatal("stale capture erased confirmed deletion", e, got.SourceDeletedAt)
	}
	for _, msg := range got.Messages {
		if msg["source_deleted_at"] == nil {
			t.Fatal("confirmed marker disappeared", msg)
		}
	}
	fetched.Messages = append(fetched.Messages, pluginapi.Message{"ts": "1000000003.000001", "user": "UOWN", "thread_ts": k.RootTS, "text": "later activity"})
	fetched.Manifest.MessageCount = 4
	fetched.Messages[0] = pluginapi.Message{"ts": k.RootTS, "user": "UOWN", "text": "root", "reply_count": 3, "reactions": fetched.Messages[0]["reactions"]}
	if _, e = s.Save(ctx, k, *fetched); e != nil {
		t.Fatal(e)
	}
	r := Run(ctx, w, s, d, true)
	if r.Threads[0].Status != "deleted" || len(d.calls) != 4 {
		t.Fatal("confirmed receipts lost during rebase", r, d.calls)
	}
}

func TestApplyBoundAlreadyCleanedIsNoop(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	d := &driver{}
	first := ApplyBound(ctx, w, s, d, Run(ctx, w, s, nil, false))
	if first.Threads[0].Status != "deleted" || len(d.calls) != 3 {
		t.Fatalf("initial apply: %+v, calls %v", first, d.calls)
	}
	preview := Run(ctx, w, s, nil, false)
	if preview.Threads[0].Status != "cleaned" {
		t.Fatalf("completed preview: %+v", preview)
	}
	// A completed journal must succeed without any source driver at all.
	result := ApplyBound(ctx, w, s, nil, preview)
	if result.Threads[0].Status != "cleaned" || result.Threads[0].Reason != "" {
		t.Fatalf("completed apply should be a no-op: %+v", result)
	}
	for _, message := range result.Threads[0].Results {
		if message.Status != "deleted" {
			t.Fatalf("lost deletion receipt: %+v", message)
		}
	}
}

func TestApplyBoundCleanedPreviewCannotAuthorizeNewDeletions(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	preview := Run(ctx, w, s, nil, false)
	preview.Threads[0].Status = "cleaned"
	d := &driver{}
	result := ApplyBound(ctx, w, s, d, preview)
	if len(d.calls) != 0 || result.Threads[0].Status != "blocked" {
		t.Fatalf("cleaned approval authorized an uncleaned archive: %+v, calls %v", result, d.calls)
	}
}

type refreshBoundaryPlugin struct {
	pluginapi.Plugin
	id      string
	refresh func()
}

func (p *refreshBoundaryPlugin) Manifest() pluginapi.Manifest {
	return pluginapi.Manifest{ID: p.id, Name: p.id, Version: "1", APIVersion: 1}
}

func (p *refreshBoundaryPlugin) CleanupBoundary(pluginapi.Workspace, pluginapi.ArchiveManifest) string {
	if p.refresh != nil {
		refresh := p.refresh
		p.refresh = nil
		refresh()
	}
	return ""
}

func TestApplyBoundCleanedPreviewCannotDeleteConcurrentReply(t *testing.T) {
	w, s := fixture(t)
	ctx := context.Background()
	k := pluginapi.Key{Channel: "CFAKE", RootTS: "1000000000.000001"}
	p := &refreshBoundaryPlugin{id: fmt.Sprintf("noop_%x", time.Now().UnixNano())}
	pluginapi.Register(p)
	w.Platform = p.id
	before, err := s.Read(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	before.Manifest.Platform = p.id
	before.Documents = nil
	if _, err := s.Save(ctx, k, *before); err != nil {
		t.Fatal(err)
	}
	d := &driver{}
	if result := Run(ctx, w, s, d, true); result.Threads[0].Status != "deleted" {
		t.Fatal(result)
	}
	preview := Run(ctx, w, s, nil, false)
	if preview.Threads[0].Status != "cleaned" {
		t.Fatal(preview)
	}
	d.calls = nil
	// Boundary validation runs after the snapshot read, outside archive locks.
	// Capture can publish a new reply while this cleaned snapshot is validated.
	p.refresh = func() {
		fresh := *before
		fresh.Documents = nil
		fresh.Manifest.Schema = 2
		fresh.Messages = []pluginapi.Message{{"ts": k.RootTS, "user": "UOWN", "text": "root", "reply_count": 1, "reactions": before.Messages[0]["reactions"]}, {"ts": "1000000003.000001", "thread_ts": k.RootTS, "user": "UOWN", "text": "unapproved new reply"}}
		fresh.Manifest.MessageCount = len(fresh.Messages)
		if _, err := s.Save(ctx, k, fresh); err != nil {
			t.Fatal(err)
		}
	}
	result := ApplyBound(ctx, w, s, d, preview)
	if result.Threads[0].Status != "cleaned" || len(d.calls) != 0 {
		t.Fatalf("cleaned approval deleted concurrent activity: %+v, calls %v", result, d.calls)
	}
	if next := Run(ctx, w, s, nil, false); next.Threads[0].Status != "dry-run" {
		t.Fatalf("new reply must require fresh approval: %+v", next)
	}
}
