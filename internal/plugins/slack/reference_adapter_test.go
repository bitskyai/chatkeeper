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

// gapped lists reference traces that used to fail the whole thread and now
// archive it with the offending file recorded as a gap.
var gapped = map[string]bool{
	"test_corrupt_cached_attachment_never_reused (call)":         true,
	"test_deleted_file_never_satisfied_from_cached_bytes (call)": true,
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
			prev := payloadSnapshot(t, tc.Previous)
			got, e := s.Snapshot(context.Background(), tc.Channel, pluginapi.Root{TS: str(tc.Root["ts"]), Raw: tc.Root}, prev)
			if tc.Invalid && gapped[tc.Name] {
				// Formerly the whole thread failed; now the thread is kept and the
				// unusable file is recorded as missing, never served from cache.
				if e != nil || len(got.Manifest.Gaps) == 0 || got.Manifest.Gaps[0].Kind != "file" {
					t.Fatalf("want archive with a file gap, got %v %#v", e, got.Manifest.Gaps)
				}
				for _, g := range got.Manifest.Gaps {
					if _, kept := got.Blobs["files/"+g.ID+".bin"]; kept {
						t.Fatal("missing file was stored")
					}
				}
				return
			}
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
				// The historical oracle predates verbatim transport documents. Extend its
				// manifest expectation with the independently checksummed raw responses.
				for name, want := range tc.Expected {
					if name == "manifest.json" {
						var expected pluginapi.ArchiveManifest
						if e := json.Unmarshal(want, &expected); e != nil {
							t.Fatal(e)
						}
						if strings.HasPrefix(tc.Name, "test_snapshot_keeps_raw_preview_and_unknown_content_without_external_fetch") {
							expected.Gaps = []pluginapi.Gap{{Kind: "unrendered", ID: "101.000001", Message: "101.000001", Reason: "unknown or bounded content rendered as best-effort text; original retained in raw response"}}
						}
						for path, raw := range got.Documents {
							if strings.HasPrefix(path, "raw/") {
								expected.Objects[path] = pluginapi.Object{Size: int64(len(raw)), SHA256: sha(raw)}
							}
						}
						want, e = storage.Canonical(expected)
						if e != nil {
							t.Fatal(e)
						}
					}
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
