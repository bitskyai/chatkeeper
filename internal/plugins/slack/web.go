package slack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"log/slog"
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

// apiError is a Slack Web API error response. Needed and provided are the
// scope lists Slack returns with missing_scope.
type apiError struct{ code, method, needed, provided string }

func (e apiError) Error() string {
	out := "Slack API "
	if e.method != "" {
		out += e.method + " "
	}
	out += "error: " + e.code
	if e.code == "missing_scope" {
		if e.needed != "" {
			out += " (needed: " + e.needed
			if e.provided != "" {
				out += "; token has: " + e.provided
			}
			out += ")"
		}
		out += ". " + scopeHint(e.needed)
	}
	return out
}

// scopeHint is the one-line fix for a missing bot scope.
func scopeHint(needed string) string {
	if needed == "" {
		needed = "the missing scope"
	}
	return "Fix: add " + needed + " under OAuth & Permissions > Bot Token Scopes, then reinstall the app to the workspace."
}

var safeScopes = regexp.MustCompile(`^[a-z0-9:._,-]{1,200}$`)

// apiErr wraps a transport-level failure with a short, credential-free detail.
func apiErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errAPI}, args...)...)
}

var readMethods = map[string]bool{"auth.test": true, "conversations.history": true, "conversations.replies": true, "reactions.get": true, "users.info": true, "users.list": true, "chat.getPermalink": true, "users.conversations": true, "conversations.list": true, "conversations.info": true, "emoji.list": true, "bots.info": true}

type Client struct {
	token string
	http  *http.Client
	log   *slog.Logger
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
		if file && redirects == 0 && FileURL(rawURL) != nil {
			return nil, invalid("file URL is not on files.slack.com")
		}
		if file && redirects > 0 && fileRedirectURL(rawURL) != nil {
			host := ""
			if u, e := url.Parse(rawURL); e == nil {
				host = u.Hostname()
			}
			return nil, invalid("file download redirected to %s (a sign-in page?); the bot token may lack files:read or the file is not shared with the bot", label(host))
		}
		req, e := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if e != nil {
			return nil, apiErr("bad request URL")
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
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, apiErr("request failed: %s", pluginapi.Redact(e.Error(), token))
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
			logger := c.log
			if logger == nil {
				logger = slog.Default()
			}
			apiMethod := "file.download"
			if !file {
				apiMethod = pathLast(req.URL.Path)
			}
			logger.Warn("source API rate limited", "method", apiMethod, "retry", retries, "retry_after", wait)
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
				return nil, apiErr("file download redirected too many times")
			}
			base, _ := url.Parse(rawURL)
			loc, e := url.Parse(res.Header.Get("Location"))
			if e != nil || res.Header.Get("Location") == "" {
				return nil, apiErr("file download redirect without a location")
			}
			rawURL = base.ResolveReference(loc).String()
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			what := "API"
			if file {
				what = "file download"
			}
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				return nil, apiErr("%s returned an unexpected redirect (HTTP %d)", what, res.StatusCode)
			}
			return nil, apiErr("%s returned HTTP %d", what, res.StatusCode)
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
		res.Body.Close()
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, apiErr("reading the response failed: %s", pluginapi.Redact(e.Error(), token))
		}
		if int64(len(b)) > limit {
			return nil, apiErr("response larger than %d bytes", limit)
		}
		if res.ContentLength >= 0 && res.ContentLength != int64(len(b)) {
			return nil, apiErr("response truncated (%d of %d bytes)", len(b), res.ContentLength)
		}
		if file && strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/html") {
			return b, errHTML
		}
		return b, nil
	}
}

// errHTML marks a file download that returned a web page instead of the file;
// Slack does this when the token cannot read the file.
var errHTML = errors.New("file download returned an HTML page instead of the file; the bot token may lack files:read or the file is not shared with the bot")

func parse(b []byte) (map[string]any, error) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&v) != nil || v == nil {
		return nil, apiErr("response is not a JSON object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, apiErr("response has trailing data")
	}
	if v["ok"] != true {
		code := str(v["error"])
		if !safeCode.MatchString(code) {
			code = "unknown_error"
		}
		e := apiError{code: code}
		if needed := str(v["needed"]); safeScopes.MatchString(needed) {
			e.needed = needed
		}
		if provided := str(v["provided"]); safeScopes.MatchString(provided) {
			e.provided = provided
		}
		return nil, e
	}
	return v, nil
}

var safeCode = patternsCode()

func (c *Client) Call(ctx context.Context, method string, params url.Values) (map[string]any, error) {
	return c.callRaw(ctx, method, params, nil)
}
func (c *Client) callRaw(ctx context.Context, method string, params url.Values, raw *[]byte) (result map[string]any, err error) {
	logger := c.log
	if logger == nil {
		logger = pluginapi.RedactingLogger(slog.Default(), pluginapi.Credentials{"token": c.token})
	}
	started := time.Now()
	defer func() {
		if err != nil {
			logger.Warn("source API failed", "method", method, "error", err, "duration", time.Since(started))
		} else {
			logger.Info("source API complete", "method", method, "duration", time.Since(started))
		}
	}()
	if !readMethods[method] {
		return nil, errAPI
	}
	b, e := c.get(ctx, "GET", "https://slack.com/api/"+method+"?"+params.Encode(), c.token, maxJSON, false)
	if e != nil {
		if errors.Is(e, errAPI) {
			return nil, fmt.Errorf("%s: %w", method, e)
		}
		return nil, e
	}
	if raw != nil {
		*raw = bytes.Clone(b)
	}
	doc, e := parse(b)
	var api apiError
	if errors.As(e, &api) {
		api.method = method
		return nil, api
	}
	if e != nil {
		return nil, fmt.Errorf("%s: %w", method, e)
	}
	return doc, nil
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
	return s.pagesRaw(ctx, method, p, nil)
}
func (s *Session) pagesRaw(ctx context.Context, method string, p url.Values, documents map[string][]byte) ([]pluginapi.Message, error) {
	params := url.Values{}
	for k, v := range p {
		params[k] = append([]string{}, v...)
	}
	seen, cursors := map[string]bool{}, map[string]bool{}
	out := []pluginapi.Message{}
	for {
		var raw []byte
		doc, e := s.client.callRaw(ctx, method, params, &raw)
		if documents != nil {
			documents["raw/replies-"+sha(raw)+".json"] = raw
		}
		if e != nil {
			return nil, e
		}
		entries, ok := doc["messages"].([]any)
		if !ok {
			if documents == nil {
				return nil, invalid("%s returned no message list", method)
			}
			entries = []any{map[string]any{"ts": params.Get("ts"), "chatkeeper_unrendered": "unexpected messages container retained in raw response"}}
		}
		reserved := map[string]bool{}
		for k := range seen {
			reserved[k] = true
		}
		for _, v := range entries {
			if m, ok := v.(map[string]any); ok && id(m["ts"], "ts") {
				reserved[str(m["ts"])] = true
			}
		}
		for i, v := range entries {
			m, ok := v.(map[string]any)
			if !ok || !id(m["ts"], "ts") {
				if method != "conversations.replies" {
					continue
				}
				copy := map[string]any{}
				for k, v := range m {
					copy[k] = v
				}
				m = copy
				if len(out) == 0 {
					m["ts"] = params.Get("ts")
				} else {
					m["ts"] = syntheticTimestamp(m, reserved)
				}
				m["chatkeeper_unrendered"] = "unexpected message/timestamp shape retained in raw response"
			}
			ts := str(m["ts"])
			if seen[ts] {
				if method == "conversations.replies" && i == 0 && ts == params.Get("ts") {
					continue
				}
				if method != "conversations.replies" {
					continue
				}
				m["ts"] = syntheticTimestamp(m, reserved)
				ts = str(m["ts"])
				m["chatkeeper_unrendered"] = "duplicate timestamp retained in raw response"
			}
			seen[ts] = true
			reserved[ts] = true
			out = append(out, m)
		}
		cursor, e := nextCursor(doc)
		if e != nil {
			if documents == nil {
				return nil, e
			}
			if len(out) > 0 {
				out[0]["chatkeeper_unrendered"] = "unexpected pagination metadata retained in raw response"
			}
			return out, nil
		}
		if cursor == "" {
			return out, nil
		}
		if cursors[cursor] || len(cursors) >= 100000 {
			if documents == nil {
				return nil, invalid("%s paging did not terminate", method)
			}
			if len(out) > 0 {
				out[0]["chatkeeper_unrendered"] = "pagination did not terminate; fetched responses retained"
			}
			return out, nil
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
			return root, nil
		}
		for _, v := range entries {
			r, ok := v.(map[string]any)
			if !ok {
				continue
			}
			name, ok := r["name"].(string)
			if !ok {
				continue
			}
			users := []string{}
			if raw, has := r["users"]; has {
				a, _ := raw.([]any)
				for _, v := range a {
					u, ok := v.(string)
					if !ok {
						continue
					}
					users = append(users, u)
				}
			}
			count := int64(len(users))
			if raw, has := r["count"]; has {
				var valid bool
				count, valid = integer(raw)
				if !valid || count < 0 {
					count = int64(len(users))
				}
			}
			if count < int64(len(users)) {
				count = int64(len(users))
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
		return empty, invalid("malformed channel or thread id")
	}
	if prev != nil && (prev.Manifest.TeamID != s.team || prev.Manifest.Channel != channel || prev.Manifest.RootTS != ts) {
		return empty, invalid("stored archive belongs to a different workspace or thread")
	}
	documents := map[string][]byte{}
	messages, e := s.pagesRaw(ctx, "conversations.replies", url.Values{"channel": {channel}, "ts": {ts}, "limit": {"100"}}, documents)
	if e != nil {
		return empty, e
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i]["ts"] == ts {
			return true
		}
		if messages[j]["ts"] == ts {
			return false
		}
		return numericTS(str(messages[i]["ts"]), str(messages[j]["ts"]))
	})
	if len(messages) == 0 || messages[0]["ts"] != ts {
		messages = append([]pluginapi.Message{{"ts": ts, "text": "[unrendered: source did not return the thread root]", "chatkeeper_unrendered": "source did not return the thread root; raw response retained"}}, messages...)
	}
	count := int64(0)
	if raw, has := root.Raw["reply_count"]; has {
		var ok bool
		count, ok = integer(raw)
		if !ok || count < 0 {
			count = int64(len(messages) - 1)
		}
	}
	returned := int64(0)
	if raw, has := messages[0]["reply_count"]; has {
		var ok bool
		returned, ok = integer(raw)
		if !ok {
			returned = int64(len(messages) - 1)
		}
	}
	incomplete := count != returned || int64(len(messages)) != count+1

	// Stored author records may be reused when a lookup fails, except those
	// that were themselves placeholders (recorded as user gaps).
	prevUsers := map[string]pluginapi.Message{}
	if prev != nil {
		for uid, u := range prev.Users {
			prevUsers[uid] = u
		}
		for _, g := range prev.Manifest.Gaps {
			if g.Kind == "user" {
				delete(prevUsers, g.ID)
			}
		}
	}
	users := map[string]pluginapi.Message{}
	files := map[string]pluginapi.Attachment{}
	blobs := map[string]pluginapi.Blob{}
	gaps := []pluginapi.Gap{}
	gapped := map[string]bool{}
	gap := func(kind, item, message, reason string) {
		if gapped[kind+"/"+item] {
			return
		}
		gapped[kind+"/"+item] = true
		gaps = append(gaps, pluginapi.Gap{Kind: kind, ID: item, Message: message, Reason: pluginapi.Redact(reason)})
	}
	var total int64
	for i, original := range messages {
		m := pluginapi.Message{}
		for k, v := range original {
			m[k] = v
		}
		messages[i] = m
		mts := str(m["ts"])
		note := func(reason string) { gap("unrendered", mts, mts, reason) }
		if i == 0 && incomplete {
			m["reply_count"] = len(messages) - 1
			note("reply count disagrees with returned messages; original retained in raw response")
		}
		if reason := renderingNote(m); reason != "" {
			note(reason)
		}
		if reason := str(m["chatkeeper_unrendered"]); reason != "" {
			note(reason)
			delete(m, "chatkeeper_unrendered")
		}
		if v, has := m["text"]; has {
			if _, ok := v.(string); !ok {
				delete(m, "text")
				note("non-string text retained in raw response")
			}
		}
		if v, has := m["user"]; has && !id(v, "user") {
			delete(m, "user")
			note("malformed author retained in raw response")
		}
		if i == 0 {
			if v, has := m["reply_count"]; has {
				if n, ok := integer(v); !ok || n < 0 {
					m["reply_count"] = len(messages) - 1
					note("malformed reply count retained in raw response")
				}
			}
		}
		if v, has := m["thread_ts"]; has && v == nil {
			delete(m, "thread_ts")
			note("null thread timestamp retained in raw response")
		}

		if thread, has := m["thread_ts"]; has && thread != ts || i > 0 && !has {
			m["thread_ts"] = ts
			note("unexpected thread reference retained in raw response")
		}
		// Posts by apps and integrations (bot_message) may carry no user; the
		// viewer names them from bot_profile/username instead.
		if uid, has := m["user"]; has {
			if !id(uid, "user") {
				return empty, invalid("message %s has a malformed user id", mts)
			}
			if _, done := users[str(uid)]; !done {
				user, e := s.user(ctx, str(uid))
				if e != nil {
					if fatalRead(ctx, e) {
						return empty, e
					}
					// Keep the thread: reuse the stored record or build a minimal
					// one from the message's own profile, and record the gap.
					if old := prevUsers[str(uid)]; old != nil && old["id"] == uid {
						user = old
					} else {
						user = placeholderUser(str(uid), m)
						gap("user", str(uid), mts, "author details unavailable: "+e.Error())
					}
				}
				users[str(uid)] = user
			}
		}
		entries, _, e := Content(m)
		if e != nil {
			gap("unrendered", mts, mts, e.Error())
		}
		for _, f := range entries {
			fid := str(f["id"])
			if !id(fid, "file") {
				gap("file", "", mts, "attachment without a valid file id")
				continue
			}
			if _, done := files[fid]; done || gapped["file/"+fid] {
				continue
			}
			if reason := fileSkipReason(f); reason != "" {
				gap("file", fid, mts, reason)
				continue
			}
			rawURL := str(f["url_private_download"])
			if rawURL == "" {
				rawURL = str(f["url_private"])
			}
			size, _ := integer(f["size"])
			if total+size > maxSnapshot {
				gap("file", fid, mts, fmt.Sprintf("thread files exceed the %d MiB archive limit", maxSnapshot>>20))
				continue
			}
			// Reserve the declared size before fetching so a thread never reads
			// more than the archive limit, even when downloads fail.
			total += size
			p := "files/" + fid + ".bin"
			content, cached := cachedFile(prev, fid, p, size, rawURL)
			if !cached {
				content, e = s.client.Download(ctx, rawURL)
				if errors.Is(e, errHTML) && int64(len(content)) == size && strings.Contains(str(f["mimetype"]), "html") {
					e = nil // an uploaded HTML file, not a sign-in page
				}
				if e != nil {
					if fatalRead(ctx, e) {
						return empty, e
					}
					gap("file", fid, mts, "download failed: "+e.Error())
					continue
				}
			}
			if int64(len(content)) != size {
				gap("file", fid, mts, fmt.Sprintf("download size %d does not match the declared %d bytes", len(content), size))
				continue
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
		// Mentions are display sugar: an unresolvable one renders as its id.
		user, e := s.user(ctx, uid)
		if e == nil {
			users[uid] = user
		} else if fatalRead(ctx, e) {
			return empty, e
		} else if old := prevUsers[uid]; old != nil && old["id"] == uid {
			users[uid] = old
		}
	}
	link := ""
	doc, e := s.client.Call(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}})
	if e == nil {
		link = str(doc["permalink"])
	} else if fatalRead(ctx, e) {
		return empty, e
	}
	if Permalink(link, channel, true) != nil && prev != nil && Permalink(prev.Manifest.Permalink, channel, true) == nil {
		link = prev.Manifest.Permalink
	}
	if Permalink(link, channel, true) != nil {
		if e != nil {
			return empty, e
		}
		return empty, invalid("chat.getPermalink returned an unexpected link")
	}
	entries := []pluginapi.Attachment{}
	for _, f := range files {
		entries = append(entries, f)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if len(gaps) == 0 {
		gaps = nil
	}
	v := pluginapi.Snapshot{RawDocuments: documents, Manifest: pluginapi.ArchiveManifest{Schema: 1, TeamID: s.team, Channel: channel, RootTS: ts, Permalink: link, MessageCount: len(messages), Files: entries, Gaps: gaps}, Messages: messages, Users: users, Blobs: blobs}
	if e = (Hooks{}).Validate(ctx, v); e != nil {
		return empty, e
	}
	return v, nil
}

// fatalRead reports errors that must abort the whole capture instead of being
// recorded as a per-item gap: cancellation and rate limiting are transient and
// retried by the next check, and nothing partial should be stored for them.
func fatalRead(ctx context.Context, e error) bool {
	var api apiError
	if errors.As(e, &api) {
		switch api.code {
		case "not_authed", "invalid_auth", "token_revoked", "account_inactive":
			return true
		}
	}
	return ctx.Err() != nil || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) || errors.Is(e, errRateLimited)
}

// fileSkipReason explains why a file entry cannot be downloaded, or "".
func fileSkipReason(f map[string]any) string {
	mode := str(f["mode"])
	switch {
	case f["deleted"] == true || mode == "tombstone":
		return "file was deleted"
	case mode == "hidden_by_limit":
		return "file is hidden by the workspace plan's history limit"
	case f["is_external"] == true || mode == "external":
		return "external file (stored outside Slack)"
	case mode != "" && mode != "hosted" && mode != "snippet":
		return "file type " + label(mode) + " cannot be downloaded"
	case f["file_access"] == "check_file_info" || f["file_access"] == "access_denied":
		return "file access restricted (" + str(f["file_access"]) + "); the bot cannot read it"
	}
	rawURL := str(f["url_private_download"])
	if rawURL == "" {
		rawURL = str(f["url_private"])
	}
	if rawURL == "" {
		return "file has no download URL"
	}
	if FileURL(rawURL) != nil {
		return "file URL is not on files.slack.com"
	}
	size, ok := integer(f["size"])
	if !ok || size < 0 {
		return "file has no valid size"
	}
	if size > maxFile {
		return fmt.Sprintf("file is larger than the %d MiB limit", maxFile>>20)
	}
	return ""
}

// cachedFile reuses a verified blob from the previous archive generation.
// A blob that fails verification is simply downloaded again.
func cachedFile(prev *pluginapi.Snapshot, fid, p string, size int64, rawURL string) ([]byte, bool) {
	if prev == nil {
		return nil, false
	}
	for _, entry := range prev.Manifest.Files {
		if entry.ID != fid || entry.Size != size || entry.URL != rawURL {
			continue
		}
		b, ok := prev.Blobs[p]
		o := prev.Manifest.Objects[p]
		if !ok || entry.Path != p || b.Size != size || b.SHA256 != entry.SHA256 || o.Size != size || o.SHA256 != entry.SHA256 {
			return nil, false
		}
		r, e := b.Open()
		if e != nil {
			return nil, false
		}
		content, e := io.ReadAll(io.LimitReader(r, size+1))
		r.Close()
		if e != nil || int64(len(content)) != size || sha(content) != entry.SHA256 {
			return nil, false
		}
		return content, true
	}
	return nil, false
}

// placeholderUser is a minimal author record built from the message itself
// when users.info cannot be read.
func placeholderUser(uid string, m pluginapi.Message) pluginapi.Message {
	user := pluginapi.Message{"id": uid}
	if p, ok := m["user_profile"].(map[string]any); ok {
		profile := map[string]any{}
		for _, k := range []string{"display_name", "real_name", "name"} {
			if v, ok := p[k].(string); ok {
				profile[k] = v
			}
		}
		user["profile"] = profile
		if v, ok := p["name"].(string); ok {
			user["name"] = v
		}
	}
	return user
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

// Synthetic keys are archive-local identities. The raw source value is retained,
// and the unrendered gap prevents destructive cleanup of uncertain items.
func syntheticTimestamp(m map[string]any, reserved map[string]bool) string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	n := uint64(0)
	for _, b := range sum[:7] {
		n = n<<8 | uint64(b)
	}
	for {
		ts := fmt.Sprintf("%d.999999", n)
		if !reserved[ts] {
			return ts
		}
		n++
	}
}
