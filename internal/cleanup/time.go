package cleanup

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Parse decimal timestamps exactly: truncating fractional seconds can open the
// minimum-age gate early. Unsupported formats fail closed.
func messageTime(ts string) (time.Time, error) {
	whole, fraction, has := strings.Cut(ts, ".")
	invalid := errors.New("invalid message age")
	if whole == "" || len(whole) > 12 || has && (fraction == "" || len(fraction) > 9) {
		return time.Time{}, invalid
	}
	for _, part := range []string{whole, fraction} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return time.Time{}, invalid
			}
		}
	}
	seconds, e := strconv.ParseInt(whole, 10, 64)
	if e != nil || seconds <= 0 {
		return time.Time{}, invalid
	}
	ns := int64(0)
	if has {
		ns, e = strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
		if e != nil {
			return time.Time{}, invalid
		}
	}
	return time.Unix(seconds, ns), nil
}
