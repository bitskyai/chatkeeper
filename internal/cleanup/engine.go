// Package cleanup implements explicit archive-then-delete; the capture runtime never imports it.
package cleanup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"runtime"
	"slices"
	"sort"
	"time"
)

type Result struct {
	TS     string `json:"ts"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
type Thread struct {
	Channel string   `json:"channel"`
	RootTS  string   `json:"root_ts"`
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Proof   string   `json:"proof,omitempty"`
	Results []Result `json:"results"`
}
type Report struct {
	Workspace string   `json:"workspace"`
	Enabled   bool     `json:"enabled"`
	Applied   bool     `json:"applied"`
	Reason    string   `json:"reason,omitempty"`
	Threads   []Thread `json:"threads"`
}
type journal struct {
	Version int               `json:"version"`
	Proof   string            `json:"proof"`
	Targets []string          `json:"targets"`
	Done    []string          `json:"done"`
	Pending *string           `json:"pending"`
	Deleted map[string]string `json:"source_deleted_at,omitempty"`
}

var threadAge = time.Since

func hash(b []byte) string   { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Digest(r Report) string { b, _ := json.Marshal(r); return hash(b) }
func readJournal(b []byte, proof string, targets []string) (*journal, error) {
	j := &journal{Version: 1, Proof: proof, Targets: targets, Done: []string{}, Deleted: map[string]string{}}
	if b == nil {
		return j, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return nil, errors.New("invalid cleanup journal")
	}
	for _, key := range []string{"version", "proof", "targets", "done", "pending"} {
		if _, ok := fields[key]; !ok {
			return nil, errors.New("invalid cleanup journal fields")
		}
	}
	j = &journal{}
	if json.Unmarshal(b, j) != nil || j.Version != 1 || j.Done == nil || j.Targets == nil || j.Proof != proof || !slices.Equal(j.Targets, targets) || len(j.Done) > len(targets) || !validProgress(j, targets) {
		return nil, errors.New("invalid cleanup journal or archive proof changed")
	}
	if j.Deleted == nil {
		j.Deleted = map[string]string{}
	}
	return j, nil
}

func validProgress(j *journal, targets []string) bool {
	seen := map[string]bool{}
	for _, ts := range j.Done {
		if seen[ts] || !slices.Contains(targets, ts) {
			return false
		}
		seen[ts] = true
	}
	return j.Pending == nil || slices.Contains(targets, *j.Pending) && !seen[*j.Pending]
}

// Run performs no driver calls during preview. Network calls never hold archive locks.
func Run(ctx context.Context, w pluginapi.Workspace, s *storage.Folder, d pluginapi.CleanupDriver, apply bool, progress ...func(Report)) Report {
	return run(ctx, w, s, d, apply, nil, progress...)
}

// ApplyBound refuses any snapshot proof absent from the approved offline preview.
func ApplyBound(ctx context.Context, w pluginapi.Workspace, s *storage.Folder, d pluginapi.CleanupDriver, preview Report) Report {
	return run(ctx, w, s, d, true, &preview)
}
func ApplyBoundProgress(ctx context.Context, w pluginapi.Workspace, s *storage.Folder, d pluginapi.CleanupDriver, preview Report, progress func(Report)) Report {
	return run(ctx, w, s, d, true, &preview, progress)
}
func run(ctx context.Context, w pluginapi.Workspace, s *storage.Folder, d pluginapi.CleanupDriver, apply bool, expected *Report, progress ...func(Report)) Report {
	r := Report{Workspace: w.ID, Enabled: w.Cleanup.Enabled, Applied: w.Cleanup.Enabled && apply, Threads: []Thread{}}
	if !w.Cleanup.Enabled {
		r.Reason = "cleanup disabled"
		return r
	}
	if apply && runtime.GOOS == "windows" {
		r.Reason = "destructive cleanup requires durable directory sync; unavailable on Windows"
		return r
	}
	keys, e := s.Keys(ctx)
	if e != nil {
		r.Reason = "cannot enumerate archive"
		return r
	}
	for _, k := range keys {
		t := Thread{Channel: k.Channel, RootTS: k.RootTS, Status: "blocked", Results: []Result{}}
		e = s.WithCleanup(ctx, func(v *storage.CleanupView) error {
			if expected != nil {
				fresh := Thread{Channel: k.Channel, RootTS: k.RootTS, Status: "blocked", Results: []Result{}}
				if e := runThread(ctx, w, k, v, nil, false, &fresh); e != nil {
					return e
				}
				approved := false
				for _, old := range expected.Threads {
					if old.Channel == k.Channel && old.RootTS == k.RootTS && old.Proof == fresh.Proof && old.Status == "dry-run" && fresh.Status == "dry-run" {
						approved = true
					}
				}
				if !approved {
					return errors.New("preview changed; preview again")
				}
			}
			return runThread(ctx, w, k, v, d, apply, &t)
		})
		if e != nil {
			t.Reason = e.Error()
			if t.Status == "blocked" {
				for _, result := range t.Results {
					if result.Status == "deleted" {
						t.Status = "partial"
						break
					}
				}
			}
			var ce *pluginapi.CleanupError
			if errors.As(e, &ce) {
				t.Status = ce.Status
			}
			for i := range t.Results {
				if t.Results[i].Status == "blocked" {
					t.Results[i].Reason = t.Reason
				}
			}
		}
		r.Threads = append(r.Threads, t)
		for _, notify := range progress {
			notify(r)
		}
		if t.Status == "rate-limited" {
			r.Reason = "rate limit exhausted; remaining threads skipped"
			break
		}
	}
	for _, f := range s.Failures() {
		found := false
		for _, t := range r.Threads {
			if t.Channel == f.Key.Channel && t.RootTS == f.Key.RootTS {
				found = true
			}
		}
		if !found {
			r.Threads = append(r.Threads, Thread{Channel: f.Key.Channel, RootTS: f.Key.RootTS, Status: "blocked", Reason: "archive integrity failure"})
		}
	}
	return r
}
func runThread(ctx context.Context, w pluginapi.Workspace, k pluginapi.Key, v *storage.CleanupView, d pluginapi.CleanupDriver, apply bool, t *Thread) (runErr error) {
	defer func() {
		if recover() != nil {
			runErr = errors.New("cleanup driver or storage operation panicked")
		}
	}()
	snap, e := v.Read(ctx, k)
	if e != nil || snap == nil {
		return errors.New("archive integrity verification failed")
	}
	m := snap.Manifest
	if m.Platform != "" && m.Platform != w.Platform || !w.AutoChannels && !slices.Contains(w.Channels, k.Channel) {
		return errors.New("archive workspace/channel boundary mismatch")
	}
	if p, ok := pluginapi.Lookup(w.Platform); ok {
		if boundary, ok := p.(pluginapi.CleanupBoundary); ok {
			if reason := boundary.CleanupBoundary(w, m); reason != "" {
				return errors.New(reason)
			}
		}
	}
	t.Proof = hash(snap.Documents["manifest.json"])
	ordered := append([]pluginapi.Message{}, snap.Messages[1:]...)
	sort.Slice(ordered, func(i, j int) bool {
		a, _ := messageTime(fmt.Sprint(ordered[i]["ts"]))
		b, _ := messageTime(fmt.Sprint(ordered[j]["ts"]))
		return a.After(b)
	})
	ordered = append(ordered, snap.Messages[0])
	targets := []string{}
	for _, msg := range ordered {
		ts, _ := msg["ts"].(string)
		targets = append(targets, ts)
		t.Results = append(t.Results, Result{TS: ts, Status: "blocked"})
	}
	var newest time.Time
	for _, msg := range snap.Messages {
		stamp, e := messageTime(fmt.Sprint(msg["ts"]))
		if e != nil {
			return e
		}
		if stamp.After(newest) {
			newest = stamp
		}
	}

	b, e := v.Journal(k)
	if e != nil {
		return errors.New("invalid cleanup journal")
	}
	// A refresh folds confirmed receipts into schema-2 messages and records
	// the old proof in its manifest. Rebase only that verified journal.
	rebased := false
	if b != nil && snap.Manifest.CleanupProof != "" {
		var prior journal
		if json.Unmarshal(b, &prior) != nil {
			return errors.New("invalid cleanup journal")
		}
		if prior.Proof != t.Proof && prior.Proof == snap.Manifest.CleanupProof {
			prior.Proof = t.Proof
			prior.Targets = targets
			confirmed := append([]string{}, prior.Done...)
			prior.Done = []string{}
			if prior.Deleted == nil {
				prior.Deleted = map[string]string{}
			}
			rebased = true
			for _, msg := range ordered {
				if msg["source_deleted_at"] != nil || slices.Contains(confirmed, fmt.Sprint(msg["ts"])) {
					ts := fmt.Sprint(msg["ts"])
					prior.Done = append(prior.Done, ts)
					if at, ok := msg["source_deleted_at"].(string); ok {
						prior.Deleted[ts] = at
					}
				}
			}
			if prior.Pending != nil && slices.Contains(prior.Done, *prior.Pending) {
				prior.Pending = nil
			}
			b, _ = json.Marshal(prior)
		}
	}
	j, e := readJournal(b, t.Proof, targets)
	if e != nil {
		return e
	}
	if w.Trigger.Who != "owner" || len(w.Trigger.Owners) == 0 || !OwnerTrigger(w.Trigger, snap.Messages[0]) {
		return errors.New("cleanup requires archived owner trigger")
	}
	for i, msg := range ordered {
		if slices.Contains(j.Done, targets[i]) {
			t.Results[i].Status = "deleted"
			continue
		}
		if p, ok := pluginapi.Lookup(w.Platform); ok {
			if offline, ok := p.(pluginapi.CleanupOffline); ok {
				if reason := offline.CleanupReason(w, msg); reason != "" {
					return errors.New(reason)
				}
			}
		}
	}
	if apply && rebased {
		rebasedBytes, _ := json.Marshal(j)
		if e = v.CompareJournal(ctx, k, t.Proof, rebasedBytes); e != nil {
			return e
		}
	}
	if !apply && j.Pending != nil {
		t.Status = "unknown/pending"
		t.Reason = "unresolved source deletion; refresh archive or resume cleanup from CLI"
		for i := range t.Results {
			if t.Results[i].TS == *j.Pending {
				t.Results[i].Status = "unknown/pending"
			}
		}
		return nil
	}
	if len(j.Done) == len(targets) {
		t.Status = "cleaned"
		return nil
	}
	if threadAge(newest) < w.Cleanup.MinAge {
		t.Status = "deferred"
		t.Reason = "thread is too recent"
		for i := range t.Results {
			t.Results[i].Status = "deferred"
		}
		return nil
	}
	if !apply {
		t.Status = "dry-run"
		for i := range t.Results {
			if t.Results[i].Status == "blocked" {
				t.Results[i].Status = "dry-run"
				t.Results[i].Reason = "author identity check deferred until apply"
			}
		}
		return nil
	}
	if len(j.Done) == len(targets) {
		t.Status = "cleaned"
		return nil
	}
	if d == nil {
		return errors.New("cleanup driver unavailable")
	}
	if e = d.Verify(ctx, m); e != nil {
		return e
	}
	for i, msg := range ordered {
		if slices.Contains(j.Done, targets[i]) {
			continue
		}
		reason, e := d.Reason(ctx, msg)
		if e != nil {
			return e
		}
		if reason != "" {
			return errors.New(reason)
		}
	}
	save := func() error {
		b, e := json.Marshal(j)
		if e != nil {
			return e
		}
		return v.SaveJournal(k, b)
	}
	for i, msg := range ordered {
		if slices.Contains(j.Done, targets[i]) {
			continue
		}
		guard := func() error {
			fresh, e := v.Read(ctx, k)
			if e != nil || fresh == nil || hash(fresh.Documents["manifest.json"]) != j.Proof {
				return errors.New("archive changed during cleanup")
			}
			missing := append([]string{}, j.Done...)
			if j.Pending != nil {
				missing = append(missing, *j.Pending)
			}
			if e = d.Check(ctx, m, snap.Messages, missing); e != nil {
				return e
			}
			reason, e := d.Reason(ctx, msg)
			if e != nil {
				return e
			}
			if reason != "" {
				return errors.New(reason)
			}
			fresh, e = v.Read(ctx, k)
			if e != nil || fresh == nil || hash(fresh.Documents["manifest.json"]) != j.Proof {
				return errors.New("archive changed during live verification")
			}
			return nil
		}
		// The driver invokes this same guard before every rate-limit retry.
		if gd, ok := d.(interface{ SetGuard(func() error) }); ok {
			gd.SetGuard(guard)
			defer gd.SetGuard(nil)
		}
		if e = d.Pace(ctx); e != nil {
			return e
		}
		if e = guard(); e != nil {
			return e
		}
		ts := targets[i]
		j.Pending = &ts
		pendingBytes, _ := json.Marshal(j)
		if e = v.CompareJournal(ctx, k, j.Proof, pendingBytes); e != nil {
			return e
		}
		t.Status = "unknown/pending"
		t.Results[i].Status = "unknown/pending"
		if e = d.Delete(ctx, m, msg); e != nil {
			t.Results[i].Reason = e.Error()
			t.Status = "unknown/pending"
			t.Results[i].Status = "unknown/pending"
			var ce *pluginapi.CleanupError
			if errors.As(e, &ce) {
				t.Results[i].Status = ce.Status
				t.Results[i].Reason = ce.Reason
			}
			return e
		}
		j.Done = append(j.Done, ts)
		j.Pending = nil
		j.Deleted[ts] = time.Now().UTC().Format(time.RFC3339Nano)
		if e = save(); e != nil {
			return e
		}
		t.Results[i].Status = "deleted"
		t.Status = "blocked"
	}
	t.Status = "deleted"
	return nil
}

// OwnerTrigger requires an actual listed owner actor, never an aggregate reaction count.
func OwnerTrigger(t pluginapi.Trigger, m pluginapi.Message) bool {
	raw, _ := json.Marshal(m["reactions"])
	var rs []struct {
		Name  string   `json:"name"`
		Users []string `json:"users"`
	}
	if json.Unmarshal(raw, &rs) != nil {
		return false
	}
	for _, r := range rs {
		if slices.Contains(t.Reactions, r.Name) {
			for _, u := range r.Users {
				if slices.Contains(t.Owners, u) {
					return true
				}
			}
		}
	}
	return false
}
