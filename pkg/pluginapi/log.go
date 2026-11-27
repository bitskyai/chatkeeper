package pluginapi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

type redactingHandler struct {
	next    slog.Handler
	secrets []string
}

// RedactingLogger removes this workspace's resolved credential values from both
// messages and structured attributes, including nested groups and error values.
func RedactingLogger(base *slog.Logger, creds Credentials) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	values := []string{}
	for _, v := range creds {
		if v != "" {
			values = append(values, v)
		}
	}
	return slog.New(redactingHandler{next: base.Handler(), secrets: values})
}
func (h redactingHandler) text(s string) string {
	for _, secret := range h.secrets {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}
func (h redactingHandler) attr(a slog.Attr) slog.Attr {
	a.Key = h.text(a.Key)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			out[i] = h.attr(a)
		}
		a.Value = slog.GroupValue(out...)
	case slog.KindString:
		a.Value = slog.StringValue(h.text(v.String()))
	case slog.KindAny:
		a.Value = slog.StringValue(h.text(fmt.Sprint(v.Any())))
	default:
		a.Value = v
	}
	return a
}
func (h redactingHandler) Enabled(c context.Context, l slog.Level) bool { return h.next.Enabled(c, l) }
func (h redactingHandler) Handle(c context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, h.text(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool { clean.AddAttrs(h.attr(a)); return true })
	return h.next.Handle(c, clean)
}
func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.attr(a)
	}
	return redactingHandler{next: h.next.WithAttrs(out), secrets: h.secrets}
}
func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(h.text(name)), secrets: h.secrets}
}
