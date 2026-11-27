package pluginapi

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxErrorDetail bounds an error cause shown to users or written to logs.
const MaxErrorDetail = 400

var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// Authorization headers and bearer credentials.
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)?bearer\s+[^\s"',;]+`), "Bearer [redacted]"},
	{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|cookie|set-cookie)\s*[:=]\s*[^\n"']*`), "$1: [redacted]"},
	// Platform API tokens: xoxb-/xoxp-/xoxe.xoxp-/xapp- style and similar prefixed secrets.
	{regexp.MustCompile(`\bxox[a-z]?[.-][A-Za-z0-9._-]+`), "[redacted-token]"},
	{regexp.MustCompile(`\bxapp-[A-Za-z0-9-]+`), "[redacted-token]"},
	{regexp.MustCompile(`\b(gh[pousr]|github_pat|glpat|sk|pk|rk)[_-][A-Za-z0-9_-]{16,}`), "[redacted-token]"},
	// URL user info and query strings may carry signed or secret parameters.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@"'<>]+@`), "$1"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s?#"'<>]*)[?#][^\s"'<>]*`), "$1?[redacted]"},
	// Long opaque runs (keys, signatures, session ids).
	{regexp.MustCompile(`[A-Za-z0-9+/_=-]{48,}`), "[redacted]"},
}

// Redact makes an error cause safe to display: it removes the given secret
// values and anything shaped like a credential, collapses whitespace and caps
// the length. It is idempotent.
func Redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)), " ")
	if utf8.RuneCountInString(s) > MaxErrorDetail {
		runes := []rune(s)
		s = string(runes[:MaxErrorDetail-1]) + "…"
	}
	return s
}
