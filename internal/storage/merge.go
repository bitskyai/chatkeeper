package storage

import (
	"bytes"
	"context"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"path"
	"sort"
	"strings"
)

// sourceContent excludes archive annotations. Versions contain complete source
// messages so their canonical SHA-256 can be checked independently of new text.
func sourceContent(m pluginapi.Message) pluginapi.Message {
	out := pluginapi.Message{}
	for k, v := range m {
		if k != "versions" && k != "source_deleted_at" && k != "source_events" {
			out[k] = v
		}
	}
	return out
}
func cloneMessages(in []pluginapi.Message) ([]pluginapi.Message, error) {
	b, e := Canonical(in)
	if e != nil {
		return nil, e
	}
	var out []pluginapi.Message
	e = decode(b, &out)
	return out, e
}
func merge(old, fresh pluginapi.Snapshot, now string) (pluginapi.Snapshot, error) {
	prior, e := cloneMessages(old.Messages)
	if e != nil {
		return fresh, e
	}
	incoming, e := cloneMessages(fresh.Messages)
	if e != nil {
		return fresh, e
	}
	byTS := map[string]pluginapi.Message{}
	for _, m := range incoming {
		byTS[m["ts"].(string)] = m
	}
	out := []pluginapi.Message{}
	schema := old.Manifest.Schema
	for _, prev := range prior {
		ts := prev["ts"].(string)
		next, exists := byTS[ts]
		delete(byTS, ts)
		if !exists {
			if prev["source_deleted_at"] == nil {
				prev["source_deleted_at"] = now
				schema = 2
			}
			out = append(out, prev)
			continue
		}
		// Ignore the same volatile fields as Save's no-op comparison.
		a, err := comparison(pluginapi.Snapshot{Messages: []pluginapi.Message{sourceContent(prev)}})
		if err != nil {
			return fresh, err
		}
		b, err := comparison(pluginapi.Snapshot{Messages: []pluginapi.Message{sourceContent(next)}})
		if err != nil {
			return fresh, err
		}
		if versions, ok := prev["versions"]; ok {
			next["versions"] = versions
		}
		if events, ok := prev["source_events"]; ok {
			next["source_events"] = events
		}
		if !bytes.Equal(a, b) {
			content := sourceContent(prev)
			raw, e := Canonical(content)
			if e != nil {
				return fresh, e
			}
			versions, _ := next["versions"].([]any)
			editedAt := now
			if edited, ok := next["edited"].(map[string]any); ok {
				if ts, ok := edited["ts"].(string); ok {
					editedAt = ts
				}
			}
			next["versions"] = append(versions, map[string]any{"edited_at": editedAt, "content": map[string]any(content), "sha256": digest(raw)})
			schema = 2
		}
		if deleted, ok := prev["source_deleted_at"]; ok && next["source_deleted_at"] == nil {
			events, _ := next["source_events"].([]any)
			next["source_events"] = append(events, map[string]any{"kind": "reappeared", "at": now, "source_deleted_at": deleted})
			delete(next, "source_deleted_at")
			schema = 2
		}
		if deleted, ok := prev["source_deleted_at"]; ok && next["source_deleted_at"] != nil {
			next["source_deleted_at"] = deleted
		}
		out = append(out, next)
	}
	for _, m := range incoming {
		if _, ok := byTS[m["ts"].(string)]; ok {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i]["ts"].(string), out[j]["ts"].(string)
		if a == b {
			return false
		}
		if a == old.Manifest.RootTS {
			return true
		}
		if b == old.Manifest.RootTS {
			return false
		}
		aa, _, _ := strings.Cut(a, ".")
		bb, _, _ := strings.Cut(b, ".")
		if len(aa) != len(bb) {
			return len(aa) < len(bb)
		}
		return a < b
	})
	fresh.Messages = out
	fresh.Manifest.MessageCount = len(out)
	fresh.Manifest.Schema = schema
	// A refresh may also lose author metadata and attachments: union both inventories.
	users := map[string]pluginapi.Message{}
	for k, v := range old.Users {
		users[k] = v
	}
	for k, v := range fresh.Users {
		users[k] = v
	}
	fresh.Users = users
	rawUsers := map[string]any{}
	for k, v := range old.RawUsers {
		rawUsers[k] = v
	}
	for k, v := range fresh.RawUsers {
		rawUsers[k] = v
	}
	fresh.RawUsers = rawUsers
	blobs := map[string]pluginapi.Blob{}
	for k, v := range old.Blobs {
		blobs[k] = v
	}
	for k, v := range fresh.Blobs {
		if prior, ok := blobs[k]; ok && prior.SHA256 != v.SHA256 {
			return fresh, ErrIntegrity
		}
		blobs[k] = v
	}
	fresh.Blobs = blobs
	files := map[string]pluginapi.Attachment{}
	for _, v := range old.Manifest.Files {
		files[v.ID] = v
	}
	for _, v := range fresh.Manifest.Files {
		files[v.ID] = v
	}
	fresh.Manifest.Files = []pluginapi.Attachment{}
	for _, v := range files {
		fresh.Manifest.Files = append(fresh.Manifest.Files, v)
	}
	sort.Slice(fresh.Manifest.Files, func(i, j int) bool { return fresh.Manifest.Files[i].ID < fresh.Manifest.Files[j].ID })
	return fresh, nil
}

// RemoveArchive is the explicit user/API deletion boundary. Refresh never calls it.
// The HTTP deletion API remains a disabled stub until user authorization is wired.
func (s *Folder) RemoveArchive(ctx context.Context, k pluginapi.Key) error {
	if s.readOnly {
		return ErrIntegrity
	}
	unlock, e := s.locked(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	target, e := s.target(k)
	if e != nil {
		return e
	}
	if e = s.root.RemoveAll(target); e != nil {
		return e
	}
	delete(s.failures, k)
	return s.syncDir(path.Dir(target))
}

func validateHistory(schema int, m pluginapi.Message) error {
	for _, key := range []string{"source_deleted_at", "versions", "source_events"} {
		if _, ok := m[key]; ok && schema != 2 {
			return ErrIntegrity
		}
	}
	if value, ok := m["source_deleted_at"]; ok {
		if s, ok := value.(string); !ok || s == "" {
			return ErrIntegrity
		}
	}
	if value, ok := m["versions"]; ok {
		versions, ok := value.([]any)
		if !ok || len(versions) == 0 {
			return ErrIntegrity
		}
		for _, value := range versions {
			v, ok := value.(map[string]any)
			if !ok {
				return ErrIntegrity
			}
			at, ok := v["edited_at"].(string)
			if !ok || at == "" {
				return ErrIntegrity
			}
			content, ok := v["content"].(map[string]any)
			if !ok || content["ts"] != m["ts"] {
				return ErrIntegrity
			}
			for _, key := range []string{"versions", "source_events", "source_deleted_at"} {
				if _, ok := content[key]; ok {
					return ErrIntegrity
				}
			}
			raw, e := Canonical(content)
			if e != nil || v["sha256"] != digest(raw) {
				return ErrIntegrity
			}
		}
	}
	if value, ok := m["source_events"]; ok {
		events, ok := value.([]any)
		if !ok || len(events) == 0 {
			return ErrIntegrity
		}
		for _, value := range events {
			v, ok := value.(map[string]any)
			if !ok || v["kind"] != "reappeared" {
				return ErrIntegrity
			}
			for _, key := range []string{"at", "source_deleted_at"} {
				if s, ok := v[key].(string); !ok || s == "" {
					return ErrIntegrity
				}
			}
		}
	}
	return nil
}
