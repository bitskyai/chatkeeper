// Package slacktest is a synthetic HTTP Web API and Socket Mode test server.
package slacktest

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type request struct{ Method, Token string }
type Server struct {
	server         *httptest.Server
	mu             sync.Mutex
	writeMu        sync.Mutex
	marked         bool
	conn           *websocket.Conn
	counts         map[string]int
	requests       []request
	acks, sequence int
	ctx            context.Context
	cancel         context.CancelFunc
	Overrides      map[string]func(http.ResponseWriter, *http.Request)
}

func New(t testing.TB) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{counts: map[string]int{}, ctx: ctx, cancel: cancel, Overrides: map[string]func(http.ResponseWriter, *http.Request){}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		cancel()
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c != nil {
			_ = c.CloseNow()
		}
		s.server.Close()
	})
	return s
}
func (s *Server) Mark(v bool)             { s.mu.Lock(); s.marked = v; s.mu.Unlock() }
func (s *Server) Calls(method string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.counts[method] }
func (s *Server) Acknowledgements() int   { s.mu.Lock(); defer s.mu.Unlock(); return s.acks }
func (s *Server) Connected() bool         { s.mu.Lock(); defer s.mu.Unlock(); return s.conn != nil }

type transport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "slack.com" && r.URL.Hostname() != "files.slack.com" && r.URL.Hostname() != "socket.slack.com" {
		return nil, fmt.Errorf("unexpected test host")
	}
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	copy.URL = &u
	copy.Host = t.target.Host
	return t.base.RoundTrip(copy)
}
func (s *Server) Client() *http.Client {
	u, _ := url.Parse(s.server.URL)
	return &http.Client{Transport: transport{target: u, base: http.DefaultTransport}, Timeout: 2 * time.Second}
}
func (s *Server) Emit(team string, event map[string]any) {
	s.mu.Lock()
	c := s.conn
	s.sequence++
	seq := s.sequence
	s.mu.Unlock()
	if c == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"envelope_id": fmt.Sprintf("synthetic-%d", seq), "type": "events_api", "payload": map[string]any{"team_id": team, "event": event}})
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, b)
}
func (s *Server) Disconnect() {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		_ = c.CloseNow()
	}
}
func (s *Server) AssertCredentials(t testing.TB) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		want := "Bearer xoxb-synthetic"
		if r.Method == "apps.connections.open" {
			want = "Bearer xapp-synthetic"
		}
		if r.Token != want {
			t.Errorf("wrong credential for %s", r.Method)
		}
	}
}
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		c, e := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if e != nil {
			return
		}
		s.mu.Lock()
		s.conn = c
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			if s.conn == c {
				s.conn = nil
			}
			s.mu.Unlock()
			_ = c.CloseNow()
		}()
		for {
			_, b, e := c.Read(s.ctx)
			if e != nil {
				return
			}
			var ack map[string]any
			if json.Unmarshal(b, &ack) == nil && ack["envelope_id"] != nil {
				s.mu.Lock()
				s.acks++
				s.mu.Unlock()
			}
		}
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.URL.Path == "/file" {
		method = "download"
	}
	s.mu.Lock()
	s.counts[method]++
	s.requests = append(s.requests, request{method, r.Header.Get("Authorization")})
	marked := s.marked
	s.mu.Unlock()
	if override := s.Overrides[method]; override != nil {
		override(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-OAuth-Scopes", "channels:history,channels:read,groups:history,groups:read,reactions:read,users:read,files:read")
	root := map[string]any{"ts": "100.000001", "user": "UOWNER", "text": "Synthetic root <@UMENTION>", "reply_count": 1, "files": []any{map[string]any{"id": "FFILE", "name": "fixture.bin", "size": 5, "url_private": "https://files.slack.com/file"}}}
	if marked {
		root["reactions"] = []any{map[string]any{"name": "white_check_mark", "count": 1, "users": []string{"UOWNER"}}}
	}
	reply := map[string]any{"ts": "101.000001", "thread_ts": "100.000001", "user": "UAUTHOR", "text": "Synthetic reply"}
	doc := map[string]any{"ok": true}
	switch method {
	case "auth.test":
		doc["team_id"] = "TFIXTURE"
		doc["user_id"] = "UBOT"
	case "users.conversations", "conversations.list":
		doc["channels"] = []any{map[string]any{"id": "CFIXTURE", "name": "fixture", "context_team_id": "TFIXTURE"}}
	case "conversations.history":
		doc["messages"] = []any{root}
	case "conversations.replies":
		doc["messages"] = []any{root, reply}
	case "reactions.get":
		doc["type"] = "message"
		doc["channel"] = r.URL.Query().Get("channel")
		doc["message"] = root
	case "users.info":
		uid := r.URL.Query().Get("user")
		name := map[string]string{"UOWNER": "Fixture owner", "UAUTHOR": "Fixture author", "UMENTION": "Fixture mention"}[uid]
		if name == "" {
			doc["ok"] = false
			doc["error"] = "user_not_found"
		} else {
			doc["user"] = map[string]any{"id": uid, "profile": map[string]any{"display_name": name}}
		}
	case "users.list":
		doc["members"] = []any{map[string]any{"id": "UOWNER", "profile": map[string]any{"display_name": "Fixture owner"}}}
	case "chat.getPermalink":
		doc["permalink"] = "https://fixture.slack.com/archives/CFIXTURE/p100000001"
	case "apps.connections.open":
		if r.Method != "POST" {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		doc["url"] = "wss://socket.slack.com/socket"
	case "download":
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("\x00test"))
		return
	default:
		doc["ok"] = false
		doc["error"] = "unknown_method"
	}
	_ = json.NewEncoder(w).Encode(doc)
}
