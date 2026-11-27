package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/http"
	"os"
	"strings"
	"testing"
)

func payloadSnapshot(t *testing.T, docs map[string][]byte) *pluginapi.Snapshot {
	t.Helper()
	if docs == nil {
		return nil
	}
	v := &pluginapi.Snapshot{Documents: map[string][]byte{}, Blobs: map[string]pluginapi.Blob{}}
	for name, b := range docs {
		if strings.HasPrefix(name, "files/") {
			v.Blobs[name] = storage.BytesBlob(b)
		} else {
			v.Documents[name] = b
		}
	}
	if json.Unmarshal(docs["manifest.json"], &v.Manifest) != nil || json.Unmarshal(docs["messages.json"], &v.Messages) != nil || json.Unmarshal(docs["users.json"], &v.Users) != nil {
		t.Fatal("invalid reference")
	}
	return v
}
func TestReferenceAdapterCases(t *testing.T) {
	b, e := os.ReadFile("testdata/adapter.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Name, Channel, Team string
		Root                pluginapi.Message
		Previous, Expected  map[string][]byte
		Invalid             bool
		Trace               []struct {
			Method, URL string
			Params      map[string]any
			Response    map[string]any
			Bytes       []byte
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e = d.Decode(&cases); e != nil {
		t.Fatal(e)
	}
	if len(cases) < 20 {
		t.Fatal("missing reference traces")
	}
	for _, tc := range cases {
		t.Run(strings.TrimSuffix(tc.Name, " (call)"), func(t *testing.T) {
			n := 0
			c := NewClient("xoxb-synthetic", &http.Client{Transport: route(func(r *http.Request) (*http.Response, error) {
				if n >= len(tc.Trace) {
					return nil, fmt.Errorf("unexpected request")
				}
				trace := tc.Trace[n]
				n++
				if trace.Method == "download" {
					if r.URL.String() != trace.URL {
						t.Errorf("download URL mismatch")
					}
					return response(200, string(trace.Bytes)), nil
				}
				if r.URL.Path != "/api/"+trace.Method {
					t.Errorf("request %s want %s", r.URL.Path, trace.Method)
				}
				for k, v := range trace.Params {
					if r.URL.Query().Get(k) != fmt.Sprint(v) {
						t.Errorf("parameter %s mismatch", k)
					}
				}
				b, _ := json.Marshal(trace.Response)
				return response(200, string(b)), nil
			})})
			s := &Session{client: c, team: tc.Team}
			got, e := s.Snapshot(context.Background(), tc.Channel, pluginapi.Root{TS: str(tc.Root["ts"]), Raw: tc.Root}, payloadSnapshot(t, tc.Previous))
			if (e != nil) != tc.Invalid {
				t.Fatalf("invalid=%v err=%v", tc.Invalid, e)
			}
			if !tc.Invalid {
				if n != len(tc.Trace) {
					t.Fatalf("unused requests %d/%d", n, len(tc.Trace))
				}
				got, e = storage.Prepare(got)
				if e != nil {
					t.Fatal(e)
				}
				for name, want := range tc.Expected {
					if strings.HasPrefix(name, "files/") {
						blob := got.Blobs[name]
						if blob.Size != int64(len(want)) || blob.SHA256 != sha(want) {
							t.Fatal("blob mismatch")
						}
					} else if !bytes.Equal(got.Documents[name], want) {
						t.Fatalf("%s differs\ngot %s\nwant %s", name, got.Documents[name], want)
					}
				}
			}
		})
	}
}
