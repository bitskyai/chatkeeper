package search

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"time"
)

var decimalTime = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE]([+-]?[0-9]+))?$`)

func parseTime(v string) (int64, error) {
	if len(v) > 64 || v == "" {
		return 0, ErrQuery
	}
	number := decimalTime.FindStringSubmatch(v)
	if number != nil && number[1] != "" {
		exponent, e := strconv.Atoi(number[1])
		if e != nil || exponent > 1000 || exponent < -1000 {
			return 0, ErrQuery
		}
	}
	var r *big.Rat
	ok := false
	if number != nil {
		r, ok = new(big.Rat).SetString(v)
	}
	if ok {
		r.Mul(r, big.NewRat(1000000, 1))
		n := new(big.Int).Quo(r.Num(), r.Denom())
		if r.Sign() < 0 || !n.IsInt64() {
			return 0, ErrQuery
		}
		return n.Int64(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, e := time.Parse(layout, v); e == nil {
			if t.Before(time.Unix(0, 0)) || t.After(time.UnixMicro(math.MaxInt64)) {
				return 0, ErrQuery
			}
			return t.UnixMicro(), nil
		}
	}
	return 0, ErrQuery
}
