package slack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxJSON int64 = 16 << 20
const maxFile int64 = 100 << 20
const maxSnapshot int64 = 512 << 20

var errAPI = errors.New("API read failed")

type apiError struct{ code string }

func (e apiError) Error() string { return "API read failed: " + e.code }

var readMethods = map[string]bool{"auth.test": true, "conversations.history": true, "conversations.replies": true, "reactions.get": true, "users.info": true, "users.list": true, "chat.getPermalink": true, "users.conversations": true, "conversations.list": true, "conversations.info": true, "emoji.list": true, "bots.info": true}

type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string, transport *http.Client) *Client {
	c := http.Client{Timeout: 60 * time.Second}
	if transport != nil {
		c = *transport
		if c.Timeout == 0 {
			c.Timeout = 60 * time.Second
		}
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{token: token, http: &c}
}

// errRateLimited is returned while an API Retry-After cooldown is in effect and
// the caller cannot wait that long; the next poll tries again.
var errRateLimited = errors.New("API rate limited; waiting for Retry-After before the next read")

// cooldowns remembers 429 Retry-After deadlines per token so they are honored
// across polls and across session restarts, not only within one request.
var cooldowns sync.Map

const defaultCooldown = 60 * time.Second
const maxCooldown = time.Duration(1<<63 - 1)

func cooldownKey(token string) string { return sha([]byte(token)) }
func (c *Client) cooldown() time.Duration {
	v, ok := cooldowns.Load(cooldownKey(c.token))
	if !ok {
		return 0
	}
	return time.Until(v.(time.Time))
}
func (c *Client) coolUntil(wait time.Duration) {
	until := time.Now().Add(wait)
	key := cooldownKey(c.token)
	if v, ok := cooldowns.Load(key); ok && v.(time.Time).After(until) {
		return
	}
	cooldowns.Store(key, until)
}

// waitCooldown sleeps through an active cooldown when the context allows it,
// and fails fast otherwise so a poll never stalls into a hook timeout.
func (c *Client) waitCooldown(ctx context.Context) error {
	wait := c.cooldown()
	if wait <= 0 {
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)-time.Second < wait {
		return errRateLimited
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}
func (c *Client) get(ctx context.Context, method, rawURL, token string, limit int64, file bool) ([]byte, error) {
	retries, redirects := 0, 0
	for {
		if e := c.waitCooldown(ctx); e != nil {
			return nil, e
		}
		if file && (redirects == 0 && FileURL(rawURL) != nil || redirects > 0 && fileRedirectURL(rawURL) != nil) {
			return nil, pluginapi.ErrInvalid
		}
		req, e := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if e != nil {
			return nil, errAPI
		}
		if file {
			u, _ := url.Parse(rawURL)
			if u.Hostname() != "files.slack.com" {
				token = ""
			}
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("User-Agent", "chatkeeper/0.1 (+https://github.com/bitskyai/chatkeeper)")
		if method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		transport := *c.http
		if file {
			transport.Jar = nil
		}
		res, e := transport.Do(req)
		if e != nil {
			return nil, errAPI
		}
		if res.StatusCode == 429 {
			res.Body.Close()
			seconds, e := strconv.Atoi(res.Header.Get("Retry-After"))
			wait := defaultCooldown
			if e == nil && seconds >= 0 {
				// Clamp before multiplication to avoid overflow; never shorten a valid wait.
				wait = maxCooldown
				if uint64(seconds) <= uint64(maxCooldown/time.Second) {
					wait = time.Duration(seconds) * time.Second
				}
			}
			c.coolUntil(wait)
			if retries >= 3 {
				return nil, errRateLimited
			}
			retries++
			continue
		}
		if file && res.StatusCode >= 300 && res.StatusCode < 400 {
			res.Body.Close()
			redirects++
			if redirects > 4 {
				return nil, errAPI
			}
			base, _ := url.Parse(rawURL)
			loc, e := url.Parse(res.Header.Get("Location"))
			if e != nil || res.Header.Get("Location") == "" {
				return nil, errAPI
			}
			rawURL = base.ResolveReference(loc).String()
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, errAPI
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
		res.Body.Close()
		if e != nil || int64(len(b)) > limit || res.ContentLength >= 0 && res.ContentLength != int64(len(b)) {
			return nil, errAPI
		}
		return b, nil
	}
}
func parse(b []byte) (map[string]any, error) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&v) != nil || v == nil {
		return nil, errAPI
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errAPI
	}
	if v["ok"] != true {
		code := str(v["error"])
		if !safeCode.MatchString(code) {
			code = "unknown_error"
		}
		return nil, apiError{code}
	}
	return v, nil
}

var safeCode = patternsCode()

func (c *Client) Call(ctx context.Context, method string, params url.Values) (map[string]any, error) {
	if !readMethods[method] {
		return nil, errAPI
	}
	b, e := c.get(ctx, "GET", "https://slack.com/api/"+method+"?"+params.Encode(), c.token, maxJSON, false)
	if e != nil {
		return nil, e
	}
	return parse(b)
}
func (c *Client) Download(ctx context.Context, rawURL string) ([]byte, error) {
	return c.get(ctx, "GET", rawURL, c.token, maxFile, true)
}

type Session struct {
	client         *Client
	team, appToken string
	live           liveState
	// triggers are the configured reaction names (tone suffixes folded) used
	// to decide which realtime reaction events are worth a thread-root lookup.
	triggers map[string]bool
	roots    rootCache
}

// rootCache maps a reacted message to its thread root, bounded in size.
type rootCache struct {
	mu sync.Mutex
	m  map[string]string
}

const maxRootCache = 4096

func (c *rootCache) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}
func (c *rootCache) put(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxRootCache {
		c.m = map[string]string{}
	}
	c.m[k] = v
}

// skinTone matches Slack's tone-modified reaction names, e.g. "+1::skin-tone-3".
var skinTone = regexp.MustCompile(`^(.+)::skin-tone-[2-6]$`)

// baseReaction strips a skin-tone suffix.
func baseReaction(name string) string {
	if m := skinTone.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// foldTones adds, for every tone-modified reaction, its base name carrying the
// combined users and counts, so a configured "+1" matches any tone while a
// configured "+1::skin-tone-3" still matches only that tone.
func foldTones(reactions []pluginapi.Reaction) []pluginapi.Reaction {
	index := map[string]int{}
	for i, r := range reactions {
		index[r.Name] = i
	}
	out := append([]pluginapi.Reaction{}, reactions...)
	type agg struct {
		users []string
		seen  map[string]bool
		extra int
	}
	folded := map[string]*agg{}
	order := []string{}
	add := func(base string, r pluginapi.Reaction) {
		a := folded[base]
		if a == nil {
			a = &agg{seen: map[string]bool{}}
			folded[base] = a
			order = append(order, base)
		}
		for _, u := range r.Users {
			if !a.seen[u] {
				a.seen[u] = true
				a.users = append(a.users, u)
			}
		}
		a.extra += max(0, r.Count-len(r.Users))
	}
	for _, r := range reactions {
		if base := baseReaction(r.Name); base != r.Name {
			if _, ok := folded[base]; !ok {
				if i, has := index[base]; has {
					add(base, reactions[i])
				}
			}
			add(base, r)
		}
	}
	for _, base := range order {
		a := folded[base]
		merged := pluginapi.Reaction{Name: base, Users: a.users, Count: len(a.users) + a.extra}
		if i, has := index[base]; has {
			out[i] = merged
		} else {
			out = append(out, merged)
		}
	}
	return out
}

// liveState is the Socket Mode connection status shown by diagnostics.
type liveState struct {
	mu        sync.Mutex
	connected bool
	detail    string
}

func (l *liveState) set(connected bool, detail string) {
	l.mu.Lock()
	l.connected, l.detail = connected, detail
	l.mu.Unlock()
}
func (s *Session) RealtimeStatus() (bool, string) {
	if s.appToken == "" {
		return false, "No app-level token is configured."
	}
	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	if !s.live.connected && s.live.detail == "" {
		return false, "Socket Mode is connecting."
	}
	return s.live.connected, s.live.detail
}

// InspectChannels reports names and bot membership for diagnostics.
func (s *Session) InspectChannels(ctx context.Context, ids []string) ([]pluginapi.ChannelStatus, error) {
	out := []pluginapi.ChannelStatus{}
	for _, cid := range ids {
		if !id(cid, "channel") {
			continue
		}
		st := pluginapi.ChannelStatus{ID: cid}
		doc, e := s.client.Call(ctx, "conversations.info", url.Values{"channel": {cid}})
		var api apiError
		switch {
		case errors.As(e, &api) && api.code == "channel_not_found":
			st.Problem = "channel not found or private without the bot: invite @ChatKeeper with /invite @ChatKeeper"
		case e != nil:
			st.Problem = "could not read channel details: " + e.Error()
		default:
			ch, _ := doc["channel"].(map[string]any)
			st.Name = str(ch["name"])
			st.Member = ch["is_member"] == true
			if ch["is_archived"] == true {
				st.Problem = "channel is archived"
			}
		}
		out = append(out, st)
		if len(out) >= 200 {
			break
		}
	}
	return out, nil
}

// ReplyReactions lists trigger reactions placed on replies rather than roots.
func (s *Session) ReplyReactions(ctx context.Context, roots []pluginapi.RootRef, names []string) ([]pluginapi.ReplyReaction, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := []pluginapi.ReplyReaction{}
	for _, root := range roots {
		if !id(root.Channel, "channel") || !id(root.RootTS, "ts") {
			continue
		}
		doc, e := s.client.Call(ctx, "conversations.replies", url.Values{"channel": {root.Channel}, "ts": {root.RootTS}, "limit": {"200"}})
		if e != nil {
			return out, nil
		}
		entries, _ := doc["messages"].([]any)
		for _, raw := range entries {
			m, ok := raw.(map[string]any)
			if !ok || str(m["ts"]) == root.RootTS {
				continue
			}
			reply, e := normalizeRoot(map[string]any{"ts": "0.0", "reactions": m["reactions"]})
			if e != nil {
				continue
			}
			for _, r := range foldTones(reply.Reactions) {
				if want[r.Name] {
					out = append(out, pluginapi.ReplyReaction{Channel: root.Channel, RootTS: root.RootTS, TS: str(m["ts"]), Name: r.Name})
				}
			}
		}
	}
	return out, nil
}

func (s *Session) Capabilities() pluginapi.Capabilities {
	return pluginapi.Capabilities{ReadHistory: true, Realtime: s.appToken != ""}
}
func (s *Session) VerifyIdentity(ctx context.Context) (pluginapi.Identity, error) {
	doc, e := s.client.Call(ctx, "auth.test", nil)
	if e != nil {
		return pluginapi.Identity{}, e
	}
	if doc["team_id"] != s.team || !id(doc["user_id"], "user") {
		return pluginapi.Identity{}, pluginapi.ErrInvalid
	}
	return pluginapi.Identity{TeamID: s.team, UserID: str(doc["user_id"])}, nil
}
func (s *Session) Channels(ctx context.Context) ([]pluginapi.Channel, error) {
	out := map[string]pluginapi.Channel{}
	cursor := ""
	seen := map[string]bool{}
	for {
		p := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"200"}}
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		doc, e := s.client.Call(ctx, "users.conversations", p)
		if e != nil {
			return nil, e
		}
		a, ok := doc["channels"].([]any)
		if !ok {
			return nil, pluginapi.ErrInvalid
		}
		for _, v := range a {
			ch, ok := v.(map[string]any)
			if !ok || !id(ch["id"], "channel") {
				return nil, pluginapi.ErrInvalid
			}
			if ch["is_archived"] == true {
				continue
			}
			if team, has := ch["context_team_id"]; has && team != s.team {
				continue
			}
			cid := str(ch["id"])
			out[cid] = pluginapi.Channel{ID: cid, Name: str(ch["name"])}
		}
		cursor, e = nextCursor(doc)
		if e != nil {
			return nil, e
		}
		if cursor == "" {
			break
		}
		if seen[cursor] || len(seen) >= 1000 || len(out) > 10000 {
			return nil, pluginapi.ErrInvalid
		}
		seen[cursor] = true
	}
	channels := []pluginapi.Channel{}
	for _, ch := range out {
		channels = append(channels, ch)
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].ID < channels[j].ID })
	return channels, nil
}
func nextCursor(doc map[string]any) (string, error) {
	if more, has := doc["has_more"]; has {
		if _, ok := more.(bool); !ok {
			return "", pluginapi.ErrInvalid
		}
	}
	cursor := ""
	if m, has := doc["response_metadata"]; has && m != nil {
		meta, ok := m.(map[string]any)
		if !ok {
			return "", pluginapi.ErrInvalid
		}
		if c, has := meta["next_cursor"]; has {
			var valid bool
			cursor, valid = c.(string)
			if !valid {
				return "", pluginapi.ErrInvalid
			}
		}
	}
	if doc["has_more"] == true && cursor == "" {
		return "", pluginapi.ErrInvalid
	}
	return cursor, nil
}
func (s *Session) pages(ctx context.Context, method string, p url.Values) ([]pluginapi.Message, error) {
	params := url.Values{}
	for k, v := range p {
		params[k] = append([]string{}, v...)
	}
	seen, cursors := map[string]bool{}, map[string]bool{}
	out := []pluginapi.Message{}
	for {
		doc, e := s.client.Call(ctx, method, params)
		if e != nil {
			return nil, e
		}
		entries, ok := doc["messages"].([]any)
		if !ok {
			return nil, pluginapi.ErrInvalid
		}
		for i, v := range entries {
			m, ok := v.(map[string]any)
			if !ok || !id(m["ts"], "ts") {
				return nil, pluginapi.ErrInvalid
			}
			ts := str(m["ts"])
			if seen[ts] {
				if method == "conversations.replies" && i == 0 && ts == params.Get("ts") {
					continue
				}
				return nil, pluginapi.ErrInvalid
			}
			seen[ts] = true
			out = append(out, m)
		}
		cursor, e := nextCursor(doc)
		if e != nil {
			return nil, e
		}
		if cursor == "" {
			return out, nil
		}
		if cursors[cursor] || len(cursors) >= 100000 {
			return nil, pluginapi.ErrInvalid
		}
		cursors[cursor] = true
		params.Set("cursor", cursor)
	}
}
func (s *Session) History(ctx context.Context, channel, oldest string) ([]pluginapi.RootRef, error) {
	if !id(channel, "channel") || oldest != "" && !id(oldest, "ts") {
		return nil, pluginapi.ErrInvalid
	}
	p := url.Values{"channel": {channel}, "limit": {"100"}}
	if oldest != "" {
		p.Set("oldest", oldest)
	}
	messages, e := s.pages(ctx, "conversations.history", p)
	if e != nil {
		return nil, e
	}
	out := []pluginapi.RootRef{}
	for _, m := range messages {
		ts := str(m["ts"])
		if thread, has := m["thread_ts"]; has && thread != ts {
			continue
		}
		root, e := normalizeRoot(m)
		if e != nil {
			return nil, e
		}
		replies, _ := integer(m["reply_count"])
		out = append(out, pluginapi.RootRef{Channel: channel, RootTS: ts, Reactions: foldTones(root.Reactions), Replies: int(replies)})
	}
	return out, nil
}
func (s *Session) Root(ctx context.Context, channel, ts string) (pluginapi.Root, error) {
	if !id(channel, "channel") || !id(ts, "ts") {
		return pluginapi.Root{}, pluginapi.ErrInvalid
	}
	doc, e := s.client.Call(ctx, "reactions.get", url.Values{"channel": {channel}, "timestamp": {ts}, "full": {"true"}})
	if e != nil {
		var api apiError
		if errors.As(e, &api) && api.code == "message_not_found" {
			return pluginapi.Root{}, nil
		}
		return pluginapi.Root{}, e
	}
	m, ok := doc["message"].(map[string]any)
	if !ok || doc["type"] != "message" || doc["channel"] != channel || m["ts"] != ts {
		return pluginapi.Root{}, pluginapi.ErrInvalid
	}
	root, e := normalizeRoot(m)
	root.Reactions = foldTones(root.Reactions)
	return root, e
}

// Item re-reads the reactions on a reply and verifies it belongs to rootTS.
func (s *Session) Item(ctx context.Context, channel, rootTS, ts string) (pluginapi.Root, error) {
	if !id(channel, "channel") || !id(rootTS, "ts") || !id(ts, "ts") {
		return pluginapi.Root{}, pluginapi.ErrInvalid
	}
	doc, e := s.client.Call(ctx, "reactions.get", url.Values{"channel": {channel}, "timestamp": {ts}, "full": {"true"}})
	if e != nil {
		var api apiError
		if errors.As(e, &api) && api.code == "message_not_found" {
			return pluginapi.Root{}, nil
		}
		return pluginapi.Root{}, e
	}
	m, ok := doc["message"].(map[string]any)
	if !ok || doc["type"] != "message" || doc["channel"] != channel || m["ts"] != ts {
		return pluginapi.Root{}, pluginapi.ErrInvalid
	}
	if m["thread_ts"] != rootTS {
		return pluginapi.Root{}, nil
	}
	item, e := normalizeRoot(map[string]any{"ts": ts, "reactions": m["reactions"]})
	if e != nil {
		return pluginapi.Root{}, e
	}
	return pluginapi.Root{TS: ts, ThreadTS: rootTS, Reactions: foldTones(item.Reactions)}, nil
}

// threadRoot resolves the thread root of a reacted message with one bounded
// conversations.replies read, cached per message.
func (s *Session) threadRoot(ctx context.Context, channel, ts string) (string, error) {
	key := channel + "/" + ts
	if root, ok := s.roots.get(key); ok {
		return root, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	doc, e := s.client.Call(ctx, "conversations.replies", url.Values{"channel": {channel}, "ts": {ts}, "limit": {"1"}})
	if e != nil {
		return "", e
	}
	entries, _ := doc["messages"].([]any)
	if len(entries) == 0 {
		return "", pluginapi.ErrInvalid
	}
	first, _ := entries[0].(map[string]any)
	root := str(first["thread_ts"])
	if root == "" {
		root = str(first["ts"])
	}
	if !id(root, "ts") {
		return "", pluginapi.ErrInvalid
	}
	s.roots.put(key, root)
	return root, nil
}
func normalizeRoot(m map[string]any) (pluginapi.Root, error) {
	ts := str(m["ts"])
	thread := str(m["thread_ts"])
	if thread != "" && thread != ts {
		return pluginapi.Root{}, nil
	}
	root := pluginapi.Root{TS: ts, ThreadTS: thread, Raw: m}
	if raw, has := m["reactions"]; has {
		entries, ok := raw.([]any)
		if !ok {
			return root, pluginapi.ErrInvalid
		}
		for _, v := range entries {
			r, ok := v.(map[string]any)
			if !ok {
				return root, pluginapi.ErrInvalid
			}
			name, ok := r["name"].(string)
			if !ok {
				return root, pluginapi.ErrInvalid
			}
			users := []string{}
			if raw, has := r["users"]; has {
				a, ok := raw.([]any)
				if !ok {
					return root, pluginapi.ErrInvalid
				}
				for _, v := range a {
					u, ok := v.(string)
					if !ok {
						return root, pluginapi.ErrInvalid
					}
					users = append(users, u)
				}
			}
			count := int64(len(users))
			if raw, has := r["count"]; has {
				var valid bool
				count, valid = integer(raw)
				if !valid || count < 0 {
					return root, pluginapi.ErrInvalid
				}
			}
			if count < int64(len(users)) {
				return root, pluginapi.ErrInvalid
			}
			root.Reactions = append(root.Reactions, pluginapi.Reaction{Name: name, Users: users, Count: int(count)})
		}
	}
	return root, nil
}
func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func blob(b []byte) pluginapi.Blob {
	b = bytes.Clone(b)
	return pluginapi.Blob{Size: int64(len(b)), SHA256: sha(b), Data: b}
}
func (s *Session) Snapshot(ctx context.Context, channel string, root pluginapi.Root, prev *pluginapi.Snapshot) (pluginapi.Snapshot, error) {
	var empty pluginapi.Snapshot
	ts := root.TS
	if !id(channel, "channel") || !id(ts, "ts") {
		return empty, pluginapi.ErrInvalid
	}
	if prev != nil && (prev.Manifest.TeamID != s.team || prev.Manifest.Channel != channel || prev.Manifest.RootTS != ts) {
		return empty, pluginapi.ErrInvalid
	}
	messages, e := s.pages(ctx, "conversations.replies", url.Values{"channel": {channel}, "ts": {ts}, "limit": {"100"}})
	if e != nil {
		return empty, e
	}
	sort.Slice(messages, func(i, j int) bool { return numericTS(str(messages[i]["ts"]), str(messages[j]["ts"])) })
	if len(messages) == 0 || messages[0]["ts"] != ts {
		return empty, pluginapi.ErrInvalid
	}
	count := int64(0)
	if raw, has := root.Raw["reply_count"]; has {
		var ok bool
		count, ok = integer(raw)
		if !ok || count < 0 {
			return empty, pluginapi.ErrInvalid
		}
	}
	returned := int64(0)
	if raw, has := messages[0]["reply_count"]; has {
		var ok bool
		returned, ok = integer(raw)
		if !ok {
			return empty, pluginapi.ErrInvalid
		}
	}
	if count != returned || int64(len(messages)) != count+1 {
		return empty, pluginapi.ErrInvalid
	}
	// Preflight the full attachment inventory before fetching users or blobs.
	// This bounds work even when an API returns many individually valid files.
	var declared int64
	declaredFiles := map[string]int64{}
	for _, message := range messages {
		entries, _, err := Content(message)
		if err != nil {
			return empty, err
		}
		for _, entry := range entries {
			fid := str(entry["id"])
			size, ok := integer(entry["size"])
			if !id(fid, "file") || !ok || size < 0 || size > maxFile {
				return empty, pluginapi.ErrInvalid
			}
			if prior, exists := declaredFiles[fid]; exists {
				if prior != size {
					return empty, pluginapi.ErrInvalid
				}
				continue
			}
			declaredFiles[fid] = size
			declared += size
			if declared > maxSnapshot {
				return empty, pluginapi.ErrInvalid
			}
		}
	}
	users := map[string]pluginapi.Message{}
	files := map[string]pluginapi.Attachment{}
	blobs := map[string]pluginapi.Blob{}
	var total int64
	for i, m := range messages {
		if thread, has := m["thread_ts"]; has && thread != ts || i > 0 && !has {
			return empty, pluginapi.ErrInvalid
		}
		if !id(m["user"], "user") {
			return empty, pluginapi.ErrInvalid
		}
		if _, has := users[str(m["user"])]; !has {
			user, e := s.user(ctx, str(m["user"]))
			if e != nil {
				return empty, e
			}
			users[str(m["user"])] = user
		}
		entries, _, e := Content(m)
		if e != nil {
			return empty, e
		}
		for _, f := range entries {
			fid := str(f["id"])
			mode := str(f["mode"])
			if !id(fid, "file") || f["is_external"] == true || f["deleted"] == true || mode != "" && mode != "hosted" && mode != "snippet" || f["file_access"] == "check_file_info" || f["file_access"] == "access_denied" {
				return empty, pluginapi.ErrInvalid
			}
			rawURL := str(f["url_private_download"])
			if rawURL == "" {
				rawURL = str(f["url_private"])
			}
			if FileURL(rawURL) != nil {
				return empty, pluginapi.ErrInvalid
			}
			size, ok := integer(f["size"])
			if !ok || size < 0 || size > maxFile {
				return empty, pluginapi.ErrInvalid
			}
			if existing, has := files[fid]; has {
				if existing.Size != size || existing.URL != rawURL {
					return empty, pluginapi.ErrInvalid
				}
				continue
			}
			total += size
			if total > maxSnapshot {
				return empty, pluginapi.ErrInvalid
			}
			p := "files/" + fid + ".bin"
			var content []byte
			cached := false
			if prev != nil {
				for _, entry := range prev.Manifest.Files {
					if entry.ID == fid && entry.Size == size && entry.URL == rawURL {
						b, ok := prev.Blobs[p]
						o := prev.Manifest.Objects[p]
						if !ok || entry.Path != p || b.Size != size || b.SHA256 != entry.SHA256 || o.Size != size || o.SHA256 != entry.SHA256 {
							return empty, pluginapi.ErrInvalid
						}
						r, e := b.Open()
						if e != nil {
							return empty, pluginapi.ErrInvalid
						}
						content, e = io.ReadAll(io.LimitReader(r, size+1))
						r.Close()
						if e != nil || int64(len(content)) != size || sha(content) != entry.SHA256 {
							return empty, pluginapi.ErrInvalid
						}
						cached = true
						break
					}
				}
			}
			if !cached {
				content, e = s.client.Download(ctx, rawURL)
				if e != nil {
					return empty, e
				}
			}
			if int64(len(content)) != size {
				return empty, pluginapi.ErrInvalid
			}
			name := str(f["name"])
			if name == "" {
				name = fid
			}
			blobs[p] = blob(content)
			files[fid] = pluginapi.Attachment{ID: fid, Path: p, Name: name, Size: size, SHA256: sha(content), URL: rawURL}
		}
	}
	mentioned := map[string]bool{}
	for _, m := range messages {
		for _, uid := range Mentioned(m) {
			if users[uid] == nil {
				mentioned[uid] = true
			}
		}
	}
	ids := []string{}
	for uid := range mentioned {
		ids = append(ids, uid)
	}
	sort.Strings(ids)
	for i, uid := range ids {
		if i >= 200 {
			break
		}
		user, e := s.user(ctx, uid)
		if e == nil {
			users[uid] = user
		} else {
			var api apiError
			if !errors.As(e, &api) {
				return empty, e
			}
		}
	}
	doc, e := s.client.Call(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}})
	if e != nil {
		return empty, e
	}
	link := str(doc["permalink"])
	if Permalink(link, channel, true) != nil {
		return empty, pluginapi.ErrInvalid
	}
	entries := []pluginapi.Attachment{}
	for _, f := range files {
		entries = append(entries, f)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	v := pluginapi.Snapshot{Manifest: pluginapi.ArchiveManifest{Schema: 1, TeamID: s.team, Channel: channel, RootTS: ts, Permalink: link, MessageCount: len(messages), Files: entries}, Messages: messages, Users: users, Blobs: blobs}
	if e = (Hooks{}).Validate(ctx, v); e != nil {
		return empty, e
	}
	return v, nil
}
func numericTS(a, b string) bool {
	aa := strings.Split(a, ".")
	bb := strings.Split(b, ".")
	x := strings.TrimLeft(aa[0], "0")
	y := strings.TrimLeft(bb[0], "0")
	if len(x) != len(y) {
		return len(x) < len(y)
	}
	if x != y {
		return x < y
	}
	return aa[1] < bb[1]
}
func (s *Session) user(ctx context.Context, uid string) (pluginapi.Message, error) {
	doc, e := s.client.Call(ctx, "users.info", url.Values{"user": {uid}})
	if e != nil {
		return nil, e
	}
	m, ok := doc["user"].(map[string]any)
	if !ok || m["id"] != uid {
		return nil, pluginapi.ErrInvalid
	}
	if p, has := m["profile"]; has {
		if _, ok := p.(map[string]any); !ok {
			return nil, pluginapi.ErrInvalid
		}
	}
	return m, nil
}
func (s *Session) Reconfigure(context.Context, pluginapi.Workspace) (bool, error) { return false, nil }
func (s *Session) Stop(context.Context) error                                     { return nil }

// Slack's signed file redirect uses a distinct host. It is allowed only after
// the authenticated files.slack.com request, with credentials stripped.
func fileRedirectURL(raw string) error {
	u, e := cleanURL(raw)
	if e != nil || u.Hostname() != "files.slack.com" && u.Hostname() != "slack-files.com" {
		return pluginapi.ErrInvalid
	}
	return nil
}
