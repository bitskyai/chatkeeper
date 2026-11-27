package storage

import (
	"bytes"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"strings"
	"testing"
)

func TestRefreshNeverDrops(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	original := fixture(t)
	if _, e := s.Save(ctx, key, original); e != nil {
		t.Fatal(e)
	}
	shorter := fixture(t)
	shorter.Documents = nil
	shorter.Messages = shorter.Messages[:1]
	shorter.Messages[0]["reply_count"] = 0
	shorter.Manifest.MessageCount = 1
	if _, e := s.Save(ctx, key, shorter); e != nil {
		t.Fatal(e)
	}
	got, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Messages) != len(original.Messages) {
		t.Fatal("refresh dropped archived reply")
	}
	if got.Messages[1]["source_deleted_at"] == nil {
		t.Fatal("missing deletion marker")
	}
}

func TestEditReappearAndExplicitRemoval(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	original := fixture(t)
	if _, e := s.Save(ctx, key, original); e != nil {
		t.Fatal(e)
	}
	priorBytes, _ := Canonical(original.Messages[1])
	priorHash := digest(priorBytes)
	edited := fixture(t)
	edited.Documents = nil
	edited.Messages[1]["text"] = "Edited synthetic reply"
	edited.Messages[1]["edited"] = map[string]any{"ts": "102.000001"}
	if _, e := s.Save(ctx, key, edited); e != nil {
		t.Fatal(e)
	}
	got, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	versions := got.Messages[1]["versions"].([]any)
	version := versions[0].(map[string]any)
	if version["sha256"] != priorHash || version["edited_at"] != "102.000001" {
		t.Fatal(version)
	}
	raw, _ := Canonical(version["content"])
	if digest(raw) != priorHash {
		t.Fatal("prior content hash changed")
	}
	missing := pluginapi.Snapshot{Manifest: got.Manifest, Messages: []pluginapi.Message{}, Users: map[string]pluginapi.Message{}, Blobs: map[string]pluginapi.Blob{}}
	missing.Manifest.Schema = 2
	missing.Manifest.MessageCount = 0
	missing.Manifest.Files = []pluginapi.Attachment{}
	if _, e = s.Save(ctx, key, missing); e != nil {
		t.Fatal(e)
	}
	deleted, _ := s.Read(ctx, key)
	for _, m := range deleted.Messages {
		if m["source_deleted_at"] == nil {
			t.Fatal("root/reply not marked")
		}
	}
	first := deleted.Messages[0]["source_deleted_at"]
	if changed, e := s.Save(ctx, key, missing); e != nil || changed {
		t.Fatal("repeated absence changed archive", changed, e)
	}
	if _, e = s.Save(ctx, key, edited); e != nil {
		t.Fatal(e)
	}
	got, e = s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	if got.Manifest.Schema != 2 || got.Messages[0]["source_deleted_at"] != nil {
		t.Fatal("reappearance not recorded")
	}
	events := got.Messages[0]["source_events"].([]any)
	if events[0].(map[string]any)["source_deleted_at"] != first {
		t.Fatal(events)
	}
	versions = got.Messages[1]["versions"].([]any)
	if len(versions) != 1 || versions[0].(map[string]any)["sha256"] != priorHash {
		t.Fatal("history lost", versions)
	}
	if e = s.RemoveArchive(ctx, key); e != nil {
		t.Fatal(e)
	}
	if got, e = s.Read(ctx, key); e != nil || got != nil {
		t.Fatal("explicit removal failed", got, e)
	}
}

func TestAdditionalGoldenRoundTrips(t *testing.T) {
	for _, name := range []string{"no-files", "multi-reply"} {
		t.Run(name, func(t *testing.T) {
			src, e := Open("testdata/golden-" + name)
			if e != nil {
				t.Fatal(e)
			}
			defer src.Close()
			v, e := src.Read(ctx, key)
			if e != nil || v == nil {
				t.Fatal(e)
			}
			dst, e := Open(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer dst.Close()
			if _, e = dst.Save(ctx, key, *v); e != nil {
				t.Fatal(e)
			}
			got, e := dst.Read(ctx, key)
			if e != nil {
				t.Fatal(e)
			}
			for name, want := range v.Documents {
				if !bytes.Equal(got.Documents[name], want) {
					t.Fatal("golden bytes changed", name)
				}
			}
		})
	}
}
func TestRejectTamperedHistory(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.Save(ctx, key, fixture(t))
	v := fixture(t)
	v.Documents = nil
	v.Messages[1]["text"] = "Synthetic edit"
	if _, e := s.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	got, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	got.Messages[1]["versions"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	got.Documents = nil
	bad, e := Prepare(*got)
	if e != nil {
		t.Fatal(e)
	}
	if e = Validate(key, bad); e == nil {
		t.Fatal("tampered prior hash accepted")
	}
}

func TestSavingArchivedDeletionIsNoop(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.Save(ctx, key, fixture(t))
	fresh := fixture(t)
	fresh.Documents = nil
	fresh.Messages = fresh.Messages[:1]
	fresh.Messages[0]["reply_count"] = 0
	fresh.Manifest.MessageCount = 1
	if _, e := s.Save(ctx, key, fresh); e != nil {
		t.Fatal(e)
	}
	archived, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	archived.Documents = nil
	archived.Messages[1]["source_deleted_at"] = "2099-01-01T00:00:00Z"
	if changed, e := s.Save(ctx, key, *archived); e != nil || changed {
		t.Fatal("archive round trip silently restored source", changed, e)
	}
}

func TestMergeKeepsGapsUntilRecovered(t *testing.T) {
	base := fixture(t)
	reply := base.Messages[1]["ts"].(string)
	fileGap := pluginapi.Gap{Kind: "file", ID: "FMISSING", Message: reply, Reason: "download failed"}
	userGap := pluginapi.Gap{Kind: "user", ID: "UMISSING", Message: reply, Reason: "author details unavailable"}
	anonGap := pluginapi.Gap{Kind: "file", Message: reply, Reason: "attachment without a valid file id"}
	old := fixture(t)
	old.Manifest.Gaps = []pluginapi.Gap{fileGap, userGap, anonGap}
	has := func(gaps []pluginapi.Gap, g pluginapi.Gap) bool {
		for _, x := range gaps {
			if gapKey(x) == gapKey(g) {
				return true
			}
		}
		return false
	}
	// Deleting the reply, or editing its attachment away, is not a recovery.
	deleted := fixture(t)
	deleted.Messages = deleted.Messages[:1]
	edited := fixture(t)
	edited.Messages[1]["text"] = "Edited synthetic reply without the file"
	for name, fresh := range map[string]pluginapi.Snapshot{"deleted": deleted, "edited": edited} {
		fresh.Manifest.Gaps = nil
		got, e := merge(old, fresh, "2026-11-27T00:00:00Z")
		if e != nil {
			t.Fatal(name, e)
		}
		if !has(got.Manifest.Gaps, fileGap) || !has(got.Manifest.Gaps, userGap) {
			t.Fatal(name, "unresolved gap dropped", got.Manifest.Gaps)
		}
		if name == "deleted" && !has(got.Manifest.Gaps, anonGap) || name == "edited" && has(got.Manifest.Gaps, anonGap) {
			t.Fatal(name, "anonymous gap", got.Manifest.Gaps)
		}
	}
	// The same items fetched successfully resolve the earlier gaps.
	recovered := fixture(t)
	recovered.Manifest.Gaps = nil
	recovered.Manifest.Files = append(recovered.Manifest.Files, pluginapi.Attachment{ID: "FMISSING", Path: "files/FMISSING.bin"})
	recovered.Users = map[string]pluginapi.Message{"UMISSING": {"id": "UMISSING"}}
	got, e := merge(old, recovered, "2026-11-27T00:00:00Z")
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Manifest.Gaps) != 0 {
		t.Fatal("recovered gaps kept", got.Manifest.Gaps)
	}
	// A gap that recurs is reported once, with the fresh reason.
	again := fixture(t)
	again.Manifest.Gaps = []pluginapi.Gap{{Kind: "file", ID: "FMISSING", Message: reply, Reason: "download failed again"}}
	got, _ = merge(old, again, "2026-11-27T00:00:00Z")
	n := 0
	for _, g := range got.Manifest.Gaps {
		if g.ID == "FMISSING" {
			n++
			if g.Reason != "download failed again" {
				t.Fatal(g)
			}
		}
	}
	if n != 1 {
		t.Fatal(got.Manifest.Gaps)
	}
}
