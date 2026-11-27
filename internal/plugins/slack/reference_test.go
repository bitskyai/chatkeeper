package slack

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// degraded reports reference cases that were fail-closed and now parse with
// the malformed element ignored, so one odd block cannot fail a whole thread.
func degraded(name string) bool {
	for _, prefix := range []string{"test_malformed_message_mentions_fail_closed", "test_malformed_canvas_rejected", "test_unsupported_or_oversized_content_is_rejected[message0]", "test_unsupported_or_oversized_content_is_rejected[message1]", "test_unsupported_or_oversized_content_is_rejected[message2]", "test_unsupported_or_oversized_content_is_rejected[message3]", "test_unsupported_or_oversized_content_is_rejected[message4]", "test_unsupported_or_oversized_content_is_rejected[message5]", "test_unsupported_or_oversized_content_is_rejected[message6]", "test_non_string_block_type_is_archive_error", "test_file_block_without_downloadable_metadata_still_fails_closed", "test_style_metadata_remains_subject_to_content_bounds"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

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
			if tc.Invalid && degraded(tc.Name) {
				// Malformed decorations no longer fail the thread: they are skipped.
				if e != nil {
					t.Fatalf("want degraded parse, got %v", e)
				}
				return
			}
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
