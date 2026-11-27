package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Canonical reproduces Python json.dumps(sort_keys=True, ensure_ascii=True, indent=2).
func Canonical(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value any
	if e = d.Decode(&value); e != nil {
		return nil, e
	}
	var out strings.Builder
	if e = encode(&out, value, 0); e != nil {
		return nil, e
	}
	out.WriteByte('\n')
	return []byte(out.String()), nil
}
func quote(s string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 32 || r > 126 {
				if r > 0xffff {
					a, b := utf16.EncodeRune(r)
					fmt.Fprintf(&out, `\u%04x\u%04x`, a, b)
				} else {
					fmt.Fprintf(&out, `\u%04x`, r)
				}
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}
func encode(b *strings.Builder, v any, depth int) error {
	if depth > 100 {
		return errors.New("JSON nesting exceeds safety limit")
	}
	indent := strings.Repeat("  ", depth)
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(quote(x))
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			f, e := strconv.ParseFloat(s, 64)
			if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return errors.New("invalid JSON number")
			}
			s = strconv.FormatFloat(f, 'g', -1, 64)
			if f != 0 && math.Abs(f) >= 1e-4 && math.Abs(f) < 1e16 {
				s = strconv.FormatFloat(f, 'f', -1, 64)
			}
			if !strings.ContainsAny(s, ".eE") {
				s += ".0"
			}
		}
		b.WriteString(s)
	case []any:
		b.WriteByte('[')
		for i, v := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
			b.WriteString(indent + "  ")
			if e := encode(b, v, depth+1); e != nil {
				return e
			}
		}
		if len(x) > 0 {
			b.WriteByte('\n')
			b.WriteString(indent)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
			b.WriteString(indent + "  ")
			b.WriteString(quote(k))
			b.WriteString(": ")
			if e := encode(b, x[k], depth+1); e != nil {
				return e
			}
		}
		if len(keys) > 0 {
			b.WriteByte('\n')
			b.WriteString(indent)
		}
		b.WriteByte('}')
	default:
		return errors.New("invalid JSON value")
	}
	return nil
}
