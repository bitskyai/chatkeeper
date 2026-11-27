package cleanup

import (
	"testing"
	"time"
)

func TestMessageTimeKeepsAgeWindowPrecision(t *testing.T) {
	stamp, e := messageTime("1000000000.999999")
	if e != nil || !stamp.Equal(time.Unix(1000000000, 999999000)) {
		t.Fatal("age truncated fractional seconds", stamp, e)
	}
	for _, s := range []string{"NaN", "+Inf", "100.bad", "-1.000001", "1.1234567890", "1e9"} {
		if _, e := messageTime(s); e == nil {
			t.Fatal("invalid age accepted", s)
		}
	}
}
