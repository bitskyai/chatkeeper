package slack

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestReferenceContentCases(t *testing.T) {
	b, e := os.ReadFile("testdata/content.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Name    string
		Message map[string]any
		Files   []map[string]any
		Texts   []string
		Invalid bool
	}
	if e = json.Unmarshal(b, &cases); e != nil {
		t.Fatal(e)
	}
	if len(cases) < 40 {
		t.Fatal("reference cases missing")
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.Name, " (call)", ""), func(t *testing.T) {
			files, texts, e := Content(tc.Message)
			if (e != nil) != tc.Invalid {
				t.Fatalf("invalid=%v: %v", tc.Invalid, e)
			}
			if !tc.Invalid {
				if len(files) != len(tc.Files) || !reflect.DeepEqual(texts, tc.Texts) {
					t.Fatalf("got %#v / %#v want %#v / %#v", files, texts, tc.Files, tc.Texts)
				}
			}
		})
	}
}
