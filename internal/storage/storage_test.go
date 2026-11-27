package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"os"
	"path/filepath"
	"testing"
)

var ctx = context.Background()
var key = pluginapi.Key{Channel: "CFIXTURE", RootTS: "100.000001"}

func fixture(t *testing.T) pluginapi.Snapshot {
	t.Helper()
	s, e := Open("testdata/golden")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	v, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	return *v
}
func TestCanonical(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"z":[],"a":"café 😀 <>&"}`, "{\n  \"a\": \"caf\\u00e9 \\ud83d\\ude00 <>&\",\n  \"z\": []\n}\n"},
		{`{"a":{},"n":1.0,"small":1e-7}`, "{\n  \"a\": {},\n  \"n\": 1.0,\n  \"small\": 1e-07\n}\n"},
	} {
		var v any
		d := json.NewDecoder(bytes.NewBufferString(tc.raw))
		d.UseNumber()
		if e := d.Decode(&v); e != nil {
			t.Fatal(e)
		}
		b, e := Canonical(v)
		if e != nil || string(b) != tc.want {
			t.Fatalf("%s %v", b, e)
		}
	}
}
func TestGoldenRoundTrip(t *testing.T) {
	v := fixture(t)
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	changed, e := s.Save(ctx, key, v)
	if e != nil || !changed {
		t.Fatal(changed, e)
	}
	changed, e = s.Save(ctx, key, v)
	if e != nil || changed {
		t.Fatal(changed, e)
	}
	for name, want := range v.Documents {
		got, e := os.ReadFile(filepath.Join(s.path, "archive", key.Channel, key.RootTS, name))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s mismatch: %v", name, e)
		}
	}
	got, e := s.Read(ctx, key)
	if e != nil || got.Manifest.Objects["files/FFILE.bin"].Size != 5 {
		t.Fatal(e)
	}
}
func TestTamperAndInventory(t *testing.T) {
	for _, bad := range []string{"hash", "extra", "symlink", "escape", "missing"} {
		t.Run(bad, func(t *testing.T) {
			v := fixture(t)
			s, _ := Open(t.TempDir())
			defer s.Close()
			if _, e := s.Save(ctx, key, v); e != nil {
				t.Fatal(e)
			}
			base := filepath.Join(s.path, "archive", key.Channel, key.RootTS)
			switch bad {
			case "hash":
				os.WriteFile(filepath.Join(base, "users.json"), []byte("{}"), 0600)
			case "extra":
				os.WriteFile(filepath.Join(base, "extra.txt"), []byte("private"), 0600)
			case "symlink":
				os.Remove(filepath.Join(base, "users.json"))
				if e := os.Symlink("messages.json", filepath.Join(base, "users.json")); e != nil {
					t.Skip(e)
				}
			case "missing":
				os.Remove(filepath.Join(base, "files/FFILE.bin"))
			case "escape":
				v.Manifest.Objects["../../escape"] = pluginapi.Object{}
				if _, e := s.Save(ctx, key, v); e == nil {
					t.Fatal("escape accepted")
				}
				return
			}
			if _, e := s.Read(ctx, key); e == nil {
				t.Fatal("corrupt archive accepted")
			}
		})
	}
}
func TestRecovery(t *testing.T) {
	v := fixture(t)
	s, _ := Open(t.TempDir())
	defer s.Close()
	if _, e := s.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	base := filepath.Join(s.path, "archive", key.Channel)
	if e := os.Rename(filepath.Join(base, key.RootTS), filepath.Join(base, "."+key.RootTS+"-previous")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Read(ctx, key); e != nil {
		t.Fatal(e)
	}
}
func TestEphemeralNoop(t *testing.T) {
	v := fixture(t)
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.Save(ctx, key, v)
	v.Documents = nil
	b, _ := json.Marshal(v.Messages)
	json.Unmarshal(b, &v.Messages)
	v.Messages[0]["blocks"].([]any)[0].(map[string]any)["block_id"] = "changed"
	changed, e := s.Save(ctx, key, v)
	if e != nil || changed {
		t.Fatal(changed, e)
	}
}
func TestMalformedNonAuthorMetadata(t *testing.T) {
	v := fixture(t)
	v.Documents = nil
	v.RawUsers = map[string]any{"UBAD": "not a mapping", "UPROF": map[string]any{"id": "UPROF", "profile": "bad", "name": "fixture-handle"}, "UNUM": map[string]any{"id": "UNUM", "profile": map[string]any{"display_name": 7}, "real_name": "Fixture fallback"}}
	s, _ := Open(t.TempDir())
	defer s.Close()
	if _, e := s.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	got, e := s.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	if got.RawUsers["UBAD"] != "not a mapping" {
		t.Fatal("non-author raw data lost")
	}
	if changed, e := s.Save(ctx, key, *got); e != nil || changed {
		t.Fatal(changed, e)
	}
}
func TestReactionMembershipNoop(t *testing.T) {
	v := fixture(t)
	v.Documents = nil
	v.Messages[0]["reactions"] = []any{map[string]any{"name": "white_check_mark", "count": 2, "users": []any{"UOWNER", "UOTHER"}}}
	s, _ := Open(t.TempDir())
	defer s.Close()
	if _, e := s.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	v.Messages[0]["reactions"].([]any)[0].(map[string]any)["users"] = []any{"UOTHER", "UOWNER"}
	if changed, e := s.Save(ctx, key, v); e != nil || changed {
		t.Fatal(changed, e)
	}
}
func TestKeysDoesNotReadObjects(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.Save(ctx, key, fixture(t))
	os.WriteFile(filepath.Join(s.path, "archive", key.Channel, key.RootTS, "files/FFILE.bin"), []byte("wrong"), 0600)
	keys, e := s.Keys(ctx)
	if e != nil || len(keys) != 1 || len(s.Failures()) != 0 {
		t.Fatal(keys, e, s.Failures())
	}
	if _, e = s.Read(ctx, key); e == nil {
		t.Fatal("Read did not verify after discovery")
	}
}

func TestRawResponsesSurviveRefresh(t *testing.T) {
	v := fixture(t)
	v.Documents = nil
	first := []byte("{\"ok\":true, \"source\":\"first\"}\n")
	second := []byte("{\"ok\":true, \"source\":\"second\"}\n")
	v.RawDocuments = map[string][]byte{"raw/first.json": first}
	st, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if _, e = st.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	v.RawDocuments = map[string][]byte{"raw/second.json": second}
	v.Messages[0]["text"] = "edited content"
	if _, e = st.Save(ctx, key, v); e != nil {
		t.Fatal(e)
	}
	saved, e := st.Read(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(saved.Documents["raw/first.json"], first) || !bytes.Equal(saved.Documents["raw/second.json"], second) {
		t.Fatal("lost verbatim response during refresh")
	}
}
