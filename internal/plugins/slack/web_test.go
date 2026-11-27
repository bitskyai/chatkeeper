package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type route func(*http.Request) (*http.Response, error)

func (f route) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
}
func TestReadOnlyAndSafeErrors(t *testing.T) {
	calls := 0
	c := NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, `{"ok":false,"error":"secret TOKEN"}`), nil
	})})
	for _, method := range []string{"chat.delete", "chat.postMessage", "files.delete", "../evil"} {
		if _, e := c.Call(context.Background(), method, nil); e == nil {
			t.Fatal(method)
		}
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	_, e := c.Call(context.Background(), "auth.test", nil)
	if e == nil || strings.Contains(e.Error(), "TOKEN") {
		t.Fatal(e)
	}
}
func TestHTTPBoundsRedirectAndRetry(t *testing.T) {
	for _, tc := range []string{"redirect", "truncated", "oversize", "rate"} {
		t.Run(tc, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
				calls++
				switch tc {
				case "redirect":
					resp := response(302, "")
					resp.Header.Set("Location", "https://evil.invalid")
					return resp, nil
				case "truncated":
					resp := response(200, `{"ok":true}`)
					resp.ContentLength = 100
					return resp, nil
				case "oversize":
					return response(200, strings.Repeat("x", int(maxJSON)+1)), nil
				default:
					if calls == 1 {
						r := response(429, "")
						r.Header.Set("Retry-After", "0")
						return r, nil
					}
					return response(200, `{"ok":true}`), nil
				}
			})}
			c := NewClient("xoxb-synthetic", client)
			_, e := c.Call(context.Background(), "auth.test", nil)
			if tc == "rate" {
				if e != nil || calls != 2 {
					t.Fatal(calls, e)
				}
			} else if e == nil || calls != 1 {
				t.Fatal(calls, e)
			}
		})
	}
}
func TestFileRedirectValidated(t *testing.T) {
	calls := 0
	c := NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
		calls++
		res := response(302, "")
		res.Header.Set("Location", "https://evil.invalid/file")
		return res, nil
	})})
	if _, e := c.Download(context.Background(), "https://files.slack.com/file"); e == nil || calls != 1 {
		t.Fatal(calls, e)
	}
}
func TestPagination(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		bad                 bool
		length              int
	}{
		{"repeat root", `{"ok":true,"messages":[{"ts":"100.000001"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`, `{"ok":true,"messages":[{"ts":"100.000001"},{"ts":"101.000001"}]}`, false, 2},
		{"missing cursor", `{"ok":true,"messages":[],"has_more":true}`, ``, true, 0},
		{"repeated cursor", `{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"next"}}`, `{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"next"}}`, true, 0},
		{"duplicate reply", `{"ok":true,"messages":[{"ts":"100.000001"},{"ts":"101.000001"}],"response_metadata":{"next_cursor":"next"}}`, `{"ok":true,"messages":[{"ts":"101.000001"}]}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			client := NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
				n++
				if n == 1 {
					return response(200, tc.first), nil
				}
				if r.URL.Query().Get("cursor") != "next" {
					t.Error("cursor missing")
				}
				return response(200, tc.second), nil
			})})
			s := &Session{client: client}
			got, e := s.pages(context.Background(), "conversations.replies", url.Values{"ts": {"100.000001"}})
			if (e != nil) != tc.bad || !tc.bad && len(got) != tc.length {
				t.Fatal(len(got), e)
			}
		})
	}
}
func TestParseWorkspace(t *testing.T) {
	p := Plugin{}
	base := `{"team_id":"TFIXTURE","channels":["CFIXTURE"],"trigger":{"reactions":["white_check_mark"],"who":"any"}}`
	for _, token := range []string{"", "bad", "xoxb-", "xoxb-with space"} {
		if _, e := p.ParseWorkspace(context.Background(), json.RawMessage(base), pluginapi.Credentials{"bot_token": token}); e == nil {
			t.Fatal("accepted token")
		}
	}
	if _, e := p.ParseWorkspace(context.Background(), json.RawMessage(base), pluginapi.Credentials{"bot_token": "xoxb-synthetic"}); e != nil {
		t.Fatal(e)
	}
}
func TestSocketURL(t *testing.T) {
	for _, u := range []string{"ws://socket.slack.com/socket", "wss://slack.com.evil.invalid/socket", "wss://user@socket.slack.com/socket", "wss://socket.slack.com:444/socket", "wss://socket.slack.com/socket#x"} {
		if SocketURL(u) == nil {
			t.Fatal(u)
		}
	}
	if e := SocketURL("wss://socket.slack.com/socket?ticket=synthetic"); e != nil {
		t.Fatal(e)
	}
}

func TestIdentityDiscoveryAndHistoryBound(t *testing.T) {
	var methods []string
	c := NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		methods = append(methods, method)
		switch method {
		case "auth.test":
			return response(200, `{"ok":true,"team_id":"TOTHER","user_id":"UBOT"}`), nil
		case "users.conversations":
			return response(200, `{"ok":true,"channels":[{"id":"CFIXTURE"},{"id":"COLD","is_archived":true},{"id":"CFOREIGN","context_team_id":"TOTHER"}]}`), nil
		case "conversations.history":
			if r.URL.Query().Get("oldest") != "90.000001" {
				t.Error("missing oldest")
			}
			return response(200, `{"ok":true,"messages":[]}`), nil
		}
		return nil, fmt.Errorf("unexpected method")
	})})
	s := &Session{client: c, team: "TFIXTURE"}
	if _, e := s.VerifyIdentity(context.Background()); e == nil || len(methods) != 1 {
		t.Fatal("identity mismatch accepted")
	}
	channels, e := s.Channels(context.Background())
	if e != nil || len(channels) != 1 || channels[0].ID != "CFIXTURE" {
		t.Fatal(channels, e)
	}
	if _, e = s.History(context.Background(), "CFIXTURE", "90.000001"); e != nil {
		t.Fatal(e)
	}
}
func TestReactionRootBoundary(t *testing.T) {
	for name, raw := range map[string]string{"foreign": `{"ok":true,"type":"message","channel":"COTHER","message":{"ts":"100.000001"}}`, "reply": `{"ok":true,"type":"message","channel":"CFIXTURE","message":{"ts":"100.000001","thread_ts":"90.000001"}}`, "missing": `{"ok":false,"error":"message_not_found"}`} {
		t.Run(name, func(t *testing.T) {
			s := &Session{client: NewClient("xoxb-synthetic", &http.Client{Transport: route(func(*http.Request) (*http.Response, error) { return response(200, raw), nil })})}
			root, e := s.Root(context.Background(), "CFIXTURE", "100.000001")
			if name == "foreign" {
				if e == nil {
					t.Fatal("foreign root accepted")
				}
			} else if e != nil || root.TS != "" {
				t.Fatal(root, e)
			}
		})
	}
}
func TestSnapshotBudgetBeforeDownload(t *testing.T) {
	files := []any{}
	for _, fid := range []string{"F1", "F2", "F3", "F4", "F5", "F6"} {
		files = append(files, map[string]any{"id": fid, "size": maxFile, "url_private": "https://files.slack.com/" + fid})
	}
	root := pluginapi.Message{"ts": "100.000001", "user": "UOWNER", "reply_count": 0, "files": files}
	requests := []string{}
	s := &Session{team: "TFIXTURE", client: NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		if r.URL.Path == "/api/conversations.replies" {
			b, _ := json.Marshal(map[string]any{"ok": true, "messages": []any{root}})
			return response(200, string(b)), nil
		}
		return response(200, `{"ok":false,"error":"unexpected"}`), nil
	})})}
	if _, e := s.Snapshot(context.Background(), "CFIXTURE", pluginapi.Root{TS: "100.000001", Raw: root}, nil); e == nil {
		t.Fatal("budget accepted")
	}
	if len(requests) != 1 {
		t.Fatal("budget checked after additional fetches", requests)
	}
}

func TestSlackFileRedirectTargets(t *testing.T) {
	for _, target := range []string{
		"https://files.slack.com/files-pri/TFIXTURE-FFILE/download/fixture.bin",
		"https://slack-files.com/TFIXTURE-FFILE-synthetic/download/fixture.bin?signature=synthetic",
		"https://files.slack.com.evil.invalid/file",
		"https://slack-files.com.evil.invalid/file",
		"http://files.slack.com/file",
	} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			client := NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					res := response(302, "")
					res.Header.Set("Location", target)
					return res, nil
				}
				if r.URL.Hostname() == "slack-files.com" && r.Header.Get("Authorization") != "" {
					t.Fatal("credential forwarded to signed file host")
				}
				if r.URL.Hostname() == "files.slack.com" && r.Header.Get("Authorization") != "Bearer xoxb-synthetic" {
					t.Fatal("missing Slack authorization")
				}
				return response(200, "synthetic file"), nil
			})})
			got, e := client.Download(context.Background(), "https://files.slack.com/files-pri/TFIXTURE-FFILE/fixture.bin")
			allowed := strings.HasPrefix(target, "https://files.slack.com/") || strings.HasPrefix(target, "https://slack-files.com/")
			if allowed && (e != nil || string(got) != "synthetic file" || calls != 2) {
				t.Fatal(string(got), e, calls)
			}
			if !allowed && (e == nil || calls != 1) {
				t.Fatal("unsafe redirect followed", e, calls)
			}
		})
	}
}
