package pluginapi

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedact(t *testing.T) {
	for _, tc := range []struct{ in, gone, keep string }{
		{"auth failed for xoxb-1111-2222-abcdefABCDEF", "xoxb-1111", "auth failed for"},
		{"rotated xoxe.xoxp-1-abcdef123456 token", "xoxp-1-abcdef", "rotated"},
		// Built at runtime so secret scanners do not flag the synthetic value.
		{"socket " + "xapp" + "-1-A111-222-abcdef rejected", "A111-222", "socket"},
		{`Get "https://files.example.invalid/f?token=s3cr3t&x=1": EOF`, "s3cr3t", "https://files.example.invalid/f?[redacted]"},
		{"sent Authorization: Bearer abc.def.ghi upstream", "abc.def.ghi", "Bearer [redacted]"},
		{"header cookie: d=sessionvalue; d-s=1", "sessionvalue", "cookie: [redacted]"},
		{"https://user:pw@example.invalid/path failed", "user:pw", "https://example.invalid/path"},
		{"key " + strings.Repeat("A1b2", 16) + " leaked", "A1b2A1b2", "key [redacted] leaked"},
		{"error: missing_scope (needed: files:read)", "\x00", "missing_scope (needed: files:read)"},
	} {
		got := Redact(tc.in)
		if strings.Contains(got, tc.gone) || !strings.Contains(got, tc.keep) {
			t.Errorf("Redact(%q) = %q", tc.in, got)
		}
		if Redact(got) != got {
			t.Errorf("not idempotent: %q", got)
		}
	}
	if got := Redact("value plainsecret here", "plainsecret"); strings.Contains(got, "plainsecret") {
		t.Fatal(got)
	}
	long := Redact(strings.Repeat("word ", 500))
	if utf8.RuneCountInString(long) > MaxErrorDetail || !strings.HasSuffix(long, "…") {
		t.Fatal(len(long))
	}
	if got := Redact("line1\nline2\x00\tend"); got != "line1 line2 end" {
		t.Fatal(got)
	}
}
