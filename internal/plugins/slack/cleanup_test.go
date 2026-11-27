package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func cleanupFixture(t *testing.T, allow bool, token string, handler http.HandlerFunc) (pluginapi.CleanupDriver, pluginapi.ArchiveManifest) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Transport = cleanupTransport{srv.URL, client.Transport}
	w := pluginapi.Workspace{Platform: "slack", Channels: []string{"CFAKE"}, Settings: pluginapi.Settings(`{"team_id":"TFAKE"}`), Trigger: pluginapi.Trigger{Who: "owner", Owners: []string{"UOWNER"}, Reactions: []string{"save"}}, Cleanup: pluginapi.CleanupPolicy{AllowOthers: allow}}
	d, e := (Plugin{}).NewCleanupDriver(context.Background(), w, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}, HTTP: client})
	if e != nil {
		t.Fatal(e)
	}
	return d, pluginapi.ArchiveManifest{TeamID: "TFAKE", Channel: "CFAKE", RootTS: "1000000000.000001"}
}

type cleanupTransport struct {
	base string
	next http.RoundTripper
}

func (t cleanupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(t.base, "http://")
	return t.next.RoundTrip(r)
}
func TestCleanupIdentityAndOwnBot(t *testing.T) {
	for _, tc := range []string{"own", "other", "wrong-team", "admin"} {
		t.Run(tc, func(t *testing.T) {
			calls := 0
			d, m := cleanupFixture(t, tc == "admin", "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "auth.test") {
					team := "TFAKE"
					if tc == "wrong-team" {
						team = "TOTHER"
					}
					io.WriteString(w, `{"ok":true,"team_id":"`+team+`","user_id":"UBOT","bot_id":"BBOT"}`)
				} else {
					calls++
					io.WriteString(w, `{"ok":true}`)
				}
			})
			msg := pluginapi.Message{"ts": m.RootTS, "user": "UBOT", "text": "hi"}
			if d.Delete(context.Background(), m, msg) == nil {
				t.Fatal("delete without identity")
			}
			e := d.Verify(context.Background(), m)
			if tc == "wrong-team" || tc == "admin" {
				if e == nil {
					t.Fatal("identity accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if tc == "other" {
				msg["user"] = "UOTHER"
			}
			reason, e := d.Reason(context.Background(), msg)
			if tc == "other" {
				if reason == "" && e == nil {
					t.Fatal("other author accepted")
				}
				if d.Delete(context.Background(), m, msg) == nil {
					t.Fatal("other author delete accepted")
				}
			} else if reason != "" || e != nil {
				t.Fatal(reason, e)
			}
			if calls != 0 {
				t.Fatal("unexpected mutation")
			}
		})
	}
}
func TestCleanupOfflineSubtype(t *testing.T) {
	w := pluginapi.Workspace{}
	if (Plugin{}).CleanupReason(w, pluginapi.Message{"subtype": "channel_join", "user": "UOWNER"}) == "" {
		t.Fatal("unsupported subtype")
	}
}

func TestCleanupLiveComparison(t *testing.T) {
	for _, tc := range []string{"same", "edited", "growth", "missing", "unmark", "cursor", "duplicate", "incomplete", "cosmetic", "absent-pending", "absent-unexpected"} {
		t.Run(tc, func(t *testing.T) {
			root := pluginapi.Message{"ts": "1000000000.000001", "user": "UBOT", "text": "root", "reply_count": 1, "reactions": []any{map[string]any{"name": "save", "users": []any{"UOWNER"}, "count": 1}}}
			reply := pluginapi.Message{"ts": "1000000001.000001", "user": "UBOT", "text": "reply", "thread_ts": "1000000000.000001"}
			archived := []pluginapi.Message{root, reply}
			var calls int
			d, m := cleanupFixture(t, false, "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "auth.test") {
					io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UBOT"}`)
					return
				}
				calls++
				if strings.HasPrefix(tc, "absent") {
					io.WriteString(w, `{"ok":false,"error":"thread_not_found"}`)
					return
				}
				raw, _ := json.Marshal(archived)
				var live []pluginapi.Message
				json.Unmarshal(raw, &live)
				doc := map[string]any{"ok": true}
				switch tc {
				case "edited":
					live[1]["text"] = "changed"
				case "growth":
					live = append(live, pluginapi.Message{"ts": "1000000002.000001", "user": "UBOT", "text": "new", "thread_ts": mRoot()})
				case "missing":
					live = live[:1]
				case "unmark":
					delete(live[0], "reactions")
				case "cursor":
					doc["has_more"] = true
				case "duplicate":
					live = append(live, live[1])
				case "incomplete":
					live[0]["reply_count"] = 3
				case "cosmetic":
					live[0]["is_starred"] = true
				}
				doc["messages"] = live
				json.NewEncoder(w).Encode(doc)
			})
			if e := d.Verify(context.Background(), m); e != nil {
				t.Fatal(e)
			}
			done := []string{}
			if tc == "absent-pending" {
				done = []string{m.RootTS, "1000000001.000001"}
			}
			e := d.Check(context.Background(), m, archived, done)
			want := tc == "same" || tc == "cosmetic" || tc == "absent-pending"
			if (e == nil) != want {
				t.Fatalf("check=%v want success %v", e, want)
			}
			if calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}
func mRoot() string { return "1000000000.000001" }
func TestCleanupRetryGuardAndUnknown(t *testing.T) {
	for _, tc := range []string{"retry", "guard", "unknown", "refused", "limited"} {
		t.Run(tc, func(t *testing.T) {
			calls := 0
			d, m := cleanupFixture(t, false, "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "auth.test") {
					io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UBOT"}`)
					return
				}
				calls++
				if tc == "unknown" {
					io.WriteString(w, `{`)
					return
				}
				if tc == "refused" {
					w.WriteHeader(403)
					return
				}
				if calls == 1 || tc == "limited" {
					w.Header().Set("Retry-After", "0.125")
					w.WriteHeader(429)
					return
				}
				io.WriteString(w, `{"ok":true,"channel":"CFAKE","ts":"1000000000.000001"}`)
			})
			d.Verify(context.Background(), m)
			cd := d.(*cleanupDriver)
			cd.sleep = func(context.Context, time.Duration) error { return nil }
			guards := 0
			cd.SetGuard(func() error {
				guards++
				if tc == "guard" && guards == 2 {
					return errors.New("archive tampered")
				}
				return nil
			})
			e := d.Delete(context.Background(), m, pluginapi.Message{"ts": m.RootTS, "user": "UBOT"})
			want := 1
			if tc == "retry" {
				want = 2
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("accepted failure")
			}
			if tc == "limited" {
				want = 4
			}
			if calls != want {
				t.Fatal(calls, want)
			}
			if tc == "retry" && guards != 2 {
				t.Fatal("retry unguarded")
			}
		})
	}
}
func TestCleanupAdminPolicy(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(strconv.FormatBool(admin), func(t *testing.T) {
			d, m := cleanupFixture(t, true, "xoxp-synthetic", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "auth.test") {
					io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UADMIN"}`)
				} else {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": "UADMIN", "is_admin": admin}})
				}
			})
			e := d.Verify(context.Background(), m)
			if (e == nil) != admin {
				t.Fatal(e)
			}
			if admin {
				reason, e := d.Reason(context.Background(), pluginapi.Message{"user": "UOTHER"})
				if e != nil || reason != "" {
					t.Fatal(reason, e)
				}
			}
		})
	}
}

func TestCleanupExplicitOwnOnlyOverridesAdmin(t *testing.T) {
	d, m := cleanupFixture(t, true, "xoxp-synthetic", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth.test") {
			io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UADMIN"}`)
		} else {
			io.WriteString(w, `{"ok":true,"user":{"id":"UADMIN","is_owner":true}}`)
		}
	})
	cd := d.(*cleanupDriver)
	cd.ws.Cleanup.OwnOnly = true
	if e := d.Verify(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	reason, e := d.Reason(context.Background(), pluginapi.Message{"user": "UOTHER"})
	if reason == "" && e == nil {
		t.Fatal("own_only ignored")
	}
}

func TestCleanupLostIdentity(t *testing.T) {
	calls := 0
	d, m := cleanupFixture(t, false, "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth.test") {
			io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UBOT"}`)
		} else {
			calls++
			io.WriteString(w, `{"ok":true,"channel":"CFAKE","ts":"1000000000.000001"}`)
		}
	})
	d.Verify(context.Background(), m)
	cd := d.(*cleanupDriver)
	cd.SetGuard(func() error { cd.verified = false; return nil })
	if e := d.Delete(context.Background(), m, pluginapi.Message{"ts": m.RootTS, "user": "UBOT"}); e == nil || calls != 0 {
		t.Fatalf("lost identity still deleted: %v %d", e, calls)
	}
}
func TestCleanupOfflinePolicy(t *testing.T) {
	for _, tc := range []struct {
		subtype, author string
		allowed         bool
	}{{"", "UOWNER", true}, {"file_share", "UOTHER", true}, {"thread_broadcast", "UOWNER", true}, {"bot_message", "UBOT", true}, {"channel_join", "UOWNER", false}, {"unsupported", "UOTHER", false}, {"", "", false}} {
		m := pluginapi.Message{"user": tc.author}
		if tc.subtype != "" {
			m["subtype"] = tc.subtype
		}
		reason := (Plugin{}).CleanupReason(pluginapi.Workspace{}, m)
		if (reason == "") != tc.allowed {
			t.Fatal(tc, reason)
		}
	}
}
func TestCleanupPagination(t *testing.T) {
	for _, mode := range []string{"valid", "repeat-cursor", "missing-cursor"} {
		t.Run(mode, func(t *testing.T) {
			root := pluginapi.Message{"ts": "1000000000.000001", "user": "UBOT", "text": "root", "reply_count": 2, "reactions": []any{map[string]any{"name": "save", "users": []any{"UOWNER"}, "count": 1}}}
			a := pluginapi.Message{"ts": "1000000001.000001", "thread_ts": mRoot(), "user": "UBOT", "text": "a"}
			b := pluginapi.Message{"ts": "1000000002.000001", "thread_ts": mRoot(), "user": "UBOT", "text": "b"}
			d, m := cleanupFixture(t, false, "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "auth.test") {
					io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UBOT"}`)
					return
				}
				doc := map[string]any{"ok": true}
				if r.URL.Query().Get("cursor") == "" {
					doc["messages"] = []pluginapi.Message{root, a}
					doc["has_more"] = true
					if mode != "missing-cursor" {
						doc["response_metadata"] = map[string]any{"next_cursor": "next"}
					}
				} else {
					doc["messages"] = []pluginapi.Message{root, b}
					if mode == "repeat-cursor" {
						doc["has_more"] = true
						doc["response_metadata"] = map[string]any{"next_cursor": "next"}
					}
				}
				json.NewEncoder(w).Encode(doc)
			})
			d.Verify(context.Background(), m)
			e := d.Check(context.Background(), m, []pluginapi.Message{root, a, b}, nil)
			if (e == nil) != (mode == "valid") {
				t.Fatal(mode, e)
			}
		})
	}
}

func TestCleanupUserAgent(t *testing.T) {
	seen := 0
	d, m := cleanupFixture(t, false, "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "chatkeeper/") {
			t.Error("missing application User-Agent")
		}
		seen++
		if strings.HasSuffix(r.URL.Path, "auth.test") {
			io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UBOT"}`)
		} else {
			io.WriteString(w, `{"ok":true,"channel":"CFAKE","ts":"1000000000.000001"}`)
		}
	})
	if e := d.Verify(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	d.(*cleanupDriver).SetGuard(func() error { return nil })
	if e := d.Delete(context.Background(), m, pluginapi.Message{"ts": m.RootTS, "user": "UBOT"}); e != nil {
		t.Fatal(e)
	}
	if seen != 2 {
		t.Fatal(seen)
	}
}

func TestCleanupCredentialGate(t *testing.T) {
	for _, token := range []string{"", "invalid", "xoxb-", "xoxp-", "xoxb-with space", "xoxp-with\nline"} {
		_, e := (Plugin{}).NewCleanupDriver(context.Background(), pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFAKE"}`)}, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}})
		if e == nil {
			t.Fatal("malformed cleanup credential accepted")
		}
	}
}
func TestCleanupOfflineBoundary(t *testing.T) {
	w := pluginapi.Workspace{Settings: pluginapi.Settings(`{"team_id":"TFAKE"}`)}
	for _, m := range []pluginapi.ArchiveManifest{{TeamID: "TOTHER"}, {TeamID: "TFAKE", Platform: "other"}} {
		if (Plugin{}).CleanupBoundary(w, m) == "" {
			t.Fatal("foreign archive accepted")
		}
	}
}

func TestCleanupReadRateLimitIsReported(t *testing.T) {
	for _, method := range []string{"auth.test", "conversations.replies"} {
		t.Run(method, func(t *testing.T) {
			d, m := cleanupFixture(t, false, "xoxb-synthetic", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, method) {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
					return
				}
				io.WriteString(w, `{"ok":true,"team_id":"TFAKE","user_id":"UBOT"}`)
			})
			e := d.Verify(context.Background(), m)
			if method != "auth.test" {
				if e != nil {
					t.Fatal(e)
				}
				e = d.Check(context.Background(), m, nil, nil)
			}
			var ce *pluginapi.CleanupError
			if !errors.As(e, &ce) || ce.Status != "rate-limited" {
				t.Fatalf("rate limit hidden: %v", e)
			}
		})
	}
}
