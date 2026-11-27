package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type cleanupDriver struct {
	ws         pluginapi.Workspace
	client     *Client
	team, user string
	verified   bool
	bot        bool
	guard      func() error
	last       time.Time
	sleep      func(context.Context, time.Duration) error
}

func waitCleanup(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (Plugin) NewCleanupDriver(_ context.Context, w pluginapi.Workspace, env pluginapi.Env) (pluginapi.CleanupDriver, error) {
	var raw struct {
		Team string `json:"team_id"`
	}
	if json.Unmarshal(w.Settings, &raw) != nil || !id(raw.Team, "team") {
		return nil, pluginapi.ErrInvalid
	}
	token := env.Credentials["cleanup_token"]
	if !validToken(token, "xoxb-") && !validToken(token, "xoxp-") {
		return nil, errors.New("cleanup requires a bot or user credential")
	}
	return &cleanupDriver{ws: w, team: raw.Team, client: NewClient(token, env.HTTP), bot: strings.HasPrefix(token, "xoxb-"), sleep: waitCleanup}, nil
}
func (d *cleanupDriver) SetGuard(g func() error) { d.guard = g }
func (d *cleanupDriver) Verify(ctx context.Context, m pluginapi.ArchiveManifest) error {
	d.verified = false
	d.user = ""
	if m.TeamID != d.team || m.Platform != "" && m.Platform != "slack" || !d.ws.AutoChannels && !slices.Contains(d.ws.Channels, m.Channel) {
		return errors.New("cleanup workspace boundary mismatch")
	}
	me, e := d.client.Call(ctx, "auth.test", nil)
	if e != nil {
		return cleanupReadError(e, "cleanup identity request failed")
	}
	if me["team_id"] != d.team || !id(me["user_id"], "user") {
		return errors.New("cleanup identity mismatch")
	}
	if d.ws.Cleanup.AllowOthers {
		if d.bot {
			return errors.New("deleting others requires an admin/owner user token")
		}
		u, e := d.client.Call(ctx, "users.info", url.Values{"user": {str(me["user_id"])}})
		if e != nil {
			return cleanupReadError(e, "cleanup admin verification failed")
		}
		user, ok := u["user"].(map[string]any)
		if !ok || user["id"] != me["user_id"] || user["is_admin"] != true && user["is_owner"] != true {
			return errors.New("deleting others requires an admin/owner user token")
		}
	}
	d.user = str(me["user_id"])
	d.verified = true
	return nil
}
func (Plugin) CleanupReason(w pluginapi.Workspace, m pluginapi.Message) string {
	if !id(m["user"], "user") {
		return "message author missing"
	}
	switch m["subtype"] {
	case nil, "", "file_share", "thread_broadcast", "bot_message":
	default:
		return "unsupported message subtype"
	}
	if m["source_deleted_at"] != nil {
		return "source already marked deleted; reconcile archive"
	}
	return ""
}
func (d *cleanupDriver) Reason(_ context.Context, m pluginapi.Message) (string, error) {
	if !d.verified {
		return "", errors.New("cleanup identity must be verified")
	}
	if r := (Plugin{}).CleanupReason(d.ws, m); r != "" {
		return r, nil
	}
	if m["user"] != d.user && (!d.ws.Cleanup.AllowOthers || d.bot || d.ws.Cleanup.OwnOnly) {
		return "message belongs to another author; admin/owner opt-in required", nil
	}
	return "", nil
}
func (d *cleanupDriver) Pace(ctx context.Context) error {
	if d.last.IsZero() {
		return ctx.Err()
	}
	return d.sleep(ctx, time.Until(d.last.Add(60*time.Second/45+10*time.Millisecond)))
}
func cleanupContent(m pluginapi.Message) ([]byte, error) {
	raw, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return nil, pluginapi.ErrInvalid
	}
	for _, k := range []string{"reactions", "reply_count", "reply_users", "reply_users_count", "latest_reply", "is_locked", "subscribed", "last_read", "thread_ts", "is_starred", "versions", "source_deleted_at", "source_events"} {
		delete(v, k)
	}
	if blocks, ok := v["blocks"].([]any); ok {
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok && block["type"] == "rich_text" {
				delete(block, "block_id")
			}
		}
	}
	if files, ok := v["files"].([]any); ok {
		for _, f := range files {
			if file, ok := f.(map[string]any); ok {
				for _, k := range []string{"shares", "is_starred", "channels", "groups", "ims", "comments_count", "num_stars", "is_public", "public_url_shared", "display_as_bot"} {
					delete(file, k)
				}
			}
		}
	}
	return json.Marshal(v)
}
func ownerMatch(t pluginapi.Trigger, m pluginapi.Message) bool {
	if t.Who != "owner" || len(t.Owners) == 0 {
		return false
	}
	r, e := normalizeRoot(m)
	if e != nil {
		return false
	}
	for _, reaction := range r.Reactions {
		if slices.Contains(t.Reactions, reaction.Name) {
			for _, u := range reaction.Users {
				if slices.Contains(t.Owners, u) {
					return true
				}
			}
		}
	}
	return false
}
func (d *cleanupDriver) Check(ctx context.Context, m pluginapi.ArchiveManifest, archived []pluginapi.Message, done []string) error {
	if !d.verified {
		return errors.New("cleanup identity must be verified")
	}
	if m.TeamID != d.team || !id(m.Channel, "channel") || !id(m.RootTS, "ts") {
		return pluginapi.ErrInvalid
	}
	s := &Session{client: d.client, team: d.team}
	live, e := s.pages(ctx, "conversations.replies", url.Values{"channel": {m.Channel}, "ts": {m.RootTS}, "limit": {"100"}})
	if e != nil {
		var api apiError
		if errors.As(e, &api) && (api.code == "thread_not_found" || api.code == "message_not_found") {
			for _, msg := range archived {
				if !slices.Contains(done, str(msg["ts"])) {
					return errors.New("source root disappeared before cleanup completed")
				}
			}
			return nil
		}
		return cleanupReadError(e, "cannot verify complete live thread")
	}
	if len(live) == 0 || live[0]["ts"] != m.RootTS || d.ws.Cleanup.Scope != "messages" && !ownerMatch(d.ws.CleanupApproval(), live[0]) {
		return errors.New("live owner trigger missing")
	}
	count, ok := integer(live[0]["reply_count"])
	if !ok {
		count = 0
	}
	if count != int64(len(live)-1) {
		return errors.New("incomplete live thread")
	}
	byTS := map[string]pluginapi.Message{}
	for _, msg := range archived {
		byTS[str(msg["ts"])] = msg
	}
	seen := map[string]bool{}
	for i, msg := range live {
		ts := str(msg["ts"])
		prior, ok := byTS[ts]
		if !ok || seen[ts] || i > 0 && msg["thread_ts"] != m.RootTS {
			return errors.New("live thread has foreign or duplicate messages")
		}
		a, e := cleanupContent(prior)
		if e != nil {
			return e
		}
		b, e := cleanupContent(msg)
		if e != nil || !bytes.Equal(a, b) {
			return errors.New("live message differs from archive")
		}
		if d.ws.Cleanup.Scope == "messages" && ownerMatch(d.ws.CleanupApproval(), prior) && !ownerMatch(d.ws.CleanupApproval(), msg) {
			return errors.New("live delete approval missing")
		}
		seen[ts] = true
	}
	for ts := range byTS {
		if !seen[ts] && !slices.Contains(done, ts) {
			return errors.New("unexplained missing source message")
		}
	}
	return nil
}
func (d *cleanupDriver) Delete(ctx context.Context, m pluginapi.ArchiveManifest, msg pluginapi.Message) error {
	r, e := d.Reason(ctx, msg)
	if e != nil {
		return e
	}
	if r != "" {
		return errors.New(r)
	}
	if m.TeamID != d.team || !id(m.Channel, "channel") || !id(msg["ts"], "ts") {
		return pluginapi.ErrInvalid
	}
	if d.guard == nil {
		return errors.New("cleanup requires archive/live guard")
	}
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if e = d.Pace(ctx); e != nil {
				return e
			}
		}
		if e = d.guard(); e != nil {
			return e
		}
		if reason, e := d.Reason(ctx, msg); e != nil {
			return e
		} else if reason != "" {
			return errors.New(reason)
		}
		body := url.Values{"channel": {m.Channel}, "ts": {str(msg["ts"])}}.Encode()
		req, e := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/chat.delete", strings.NewReader(body))
		if e != nil {
			return errors.New("cleanup request failed")
		}
		req.Header.Set("Authorization", "Bearer "+d.client.token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "chatkeeper/0.1 (+https://github.com/bitskyai/chatkeeper)")
		d.last = time.Now()
		res, e := d.client.http.Do(req)
		if e != nil {
			return errors.New("cleanup request outcome unknown; pending retained")
		}
		b, re := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
		res.Body.Close()
		if re != nil || len(b) > int(maxJSON) {
			return errors.New("cleanup response unknown; pending retained")
		}
		if res.StatusCode == 429 {
			if attempt == 3 {
				return &pluginapi.CleanupError{Status: "rate-limited", Reason: "cleanup rate limited; resume later"}
			}
			delay, e := strconv.ParseFloat(res.Header.Get("Retry-After"), 64)
			if e != nil {
				delay = float64(int64(1) << attempt)
			}
			if delay < 0 || delay > 300 || delay != delay {
				return &pluginapi.CleanupError{Status: "rate-limited", Reason: "cleanup requires a later retry"}
			}
			if e = d.sleep(ctx, max(time.Duration(delay*float64(time.Second)), 100*time.Millisecond)); e != nil {
				return e
			}
			continue
		}
		if res.StatusCode != 200 {
			if res.StatusCode == 401 || res.StatusCode == 403 {
				return &pluginapi.CleanupError{Status: "refused", Reason: "cleanup API refused; pending retained"}
			}
			return errors.New("cleanup API outcome unknown; pending retained")
		}
		doc, e := parse(b)
		if e != nil {
			var api apiError
			if errors.As(e, &api) && api.code == "message_not_found" {
				return nil
			}
			if errors.As(e, &api) {
				return &pluginapi.CleanupError{Status: "refused", Reason: "cleanup API refused; pending retained"}
			}
			return errors.New("cleanup API response unknown; pending retained")
		}
		if doc["channel"] != m.Channel || doc["ts"] != msg["ts"] {
			return errors.New("cleanup response identity mismatch; pending retained")
		}
		return nil
	}
	return errors.New("cleanup retry limit")
}

func (Plugin) CleanupBoundary(w pluginapi.Workspace, m pluginapi.ArchiveManifest) string {
	var raw struct {
		Team string `json:"team_id"`
	}
	if json.Unmarshal(w.Settings, &raw) != nil || m.TeamID != raw.Team || m.Platform != "" && m.Platform != "slack" {
		return "archive workspace boundary mismatch"
	}
	return ""
}

func cleanupReadError(e error, reason string) error {
	if errors.Is(e, errRateLimited) {
		return &pluginapi.CleanupError{Status: "rate-limited", Reason: "cleanup read rate limit exhausted; resume later"}
	}
	return errors.New(reason)
}
