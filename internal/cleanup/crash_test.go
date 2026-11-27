//go:build !windows

package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type crashDriver struct{ dir, phase string }

func (d *crashDriver) Verify(context.Context, pluginapi.ArchiveManifest) error   { return nil }
func (d *crashDriver) Reason(context.Context, pluginapi.Message) (string, error) { return "", nil }
func (d *crashDriver) Pace(context.Context) error                                { return nil }
func (d *crashDriver) stop(phase string) {
	if d.phase == phase {
		os.WriteFile(filepath.Join(d.dir, "ready"), []byte(phase), 0600)
		for {
			time.Sleep(time.Hour)
		}
	}
}
func (d *crashDriver) Check(_ context.Context, _ pluginapi.ArchiveManifest, msgs []pluginapi.Message, missing []string) error {
	b, _ := os.ReadFile(filepath.Join(d.dir, "source.json"))
	var deleted []string
	json.Unmarshal(b, &deleted)
	for _, ts := range deleted {
		if !slices.Contains(missing, ts) {
			return fmt.Errorf("absent target not journalled: %s", ts)
		}
	}
	d.stop("before-pending")
	if len(deleted) == 1 {
		d.stop("after-done")
	}
	return nil
}
func (d *crashDriver) Delete(_ context.Context, m pluginapi.ArchiveManifest, msg pluginapi.Message) error {
	raw, e := os.ReadFile(filepath.Join(d.dir, "cleanup", m.Channel+"_"+m.RootTS+".json"))
	if e != nil {
		return e
	}
	var j journal
	if json.Unmarshal(raw, &j) != nil || j.Pending == nil || *j.Pending != msg["ts"] {
		return fmt.Errorf("delete without durable pending")
	}
	d.stop("after-pending")
	b, _ := os.ReadFile(filepath.Join(d.dir, "source.json"))
	var deleted []string
	json.Unmarshal(b, &deleted)
	ts := msg["ts"].(string)
	if !slices.Contains(deleted, ts) {
		deleted = append(deleted, ts)
		b, _ = json.Marshal(deleted)
		f, e := os.OpenFile(filepath.Join(d.dir, "source.json"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(b)
		if e == nil {
			e = f.Sync()
		}
		f.Close()
		if e != nil {
			return e
		}
	}
	d.stop("after-delete")
	return nil
}
func TestCrashResumeBoundaries(t *testing.T) {
	if dir := os.Getenv("CHATKEEPER_CLEANUP_CRASH_CHILD"); dir != "" {
		b, _ := os.ReadFile(filepath.Join(dir, "workspace.json"))
		var w pluginapi.Workspace
		if json.Unmarshal(b, &w) != nil {
			t.Fatal("workspace")
		}
		s, e := storage.Open(dir)
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		Run(context.Background(), w, s, &crashDriver{dir: dir, phase: os.Getenv("CHATKEEPER_CLEANUP_CRASH_PHASE")}, true)
		return
	}
	for _, phase := range []string{"before-pending", "after-pending", "after-delete", "after-done"} {
		t.Run(phase, func(t *testing.T) {
			w, s := fixture(t)
			b, _ := json.Marshal(w)
			os.WriteFile(filepath.Join(s.Path(), "workspace.json"), b, 0600)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashResumeBoundaries$")
			var childOutput bytes.Buffer
			cmd.Stdout = &childOutput
			cmd.Stderr = &childOutput
			cmd.Env = append(os.Environ(), "CHATKEEPER_CLEANUP_CRASH_CHILD="+s.Path(), "CHATKEEPER_CLEANUP_CRASH_PHASE="+phase)
			if e := cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, e := os.Stat(filepath.Join(s.Path(), "ready")); e == nil {
					break
				}
				if time.Now().After(deadline) {
					cmd.Process.Kill()
					cmd.Wait()
					t.Fatalf("boundary not reached: %s", childOutput.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			cmd.Process.Kill()
			cmd.Wait()
			r := Run(context.Background(), w, s, &crashDriver{dir: s.Path()}, true)
			if len(r.Threads) != 1 || r.Threads[0].Status != "deleted" {
				t.Fatalf("resume %+v", r)
			}
			b, _ = os.ReadFile(filepath.Join(s.Path(), "source.json"))
			var deleted []string
			json.Unmarshal(b, &deleted)
			want := []string{"1000000002.000001", "1000000001.000001", "1000000000.000001"}
			if !slices.Equal(deleted, want) {
				t.Fatal(deleted)
			}
			snap, e := s.Read(context.Background(), pluginapi.Key{Channel: "CFAKE", RootTS: want[2]})
			if e != nil || len(snap.Messages) != 3 || len(snap.SourceDeletedAt) != 3 {
				t.Fatal("archive retention", e)
			}
		})
	}
}
