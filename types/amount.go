package types

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrAmountFormat is returned by ParseSOQ for text that is not a decimal SOQ
// figure in the form the node prints, or that does not fit in an int64 of
// shors.
var ErrAmountFormat = errors.New("types: not a decimal SOQ amount")

// ParseSOQ converts a decimal SOQ figure to shors without passing through a
// float. The node prints every amount as an integer part, a point and eight
// fraction digits (ValueFromAmount, src/rpc/server.cpp); ParseSOQ accepts that
// form, fewer fraction digits, and an integer with no point. It refuses a
// sign, an exponent, a ninth fraction digit, an empty integer part and any
// value above math.MaxInt64 shors.
//
// A float64 holds shors exactly only up to 2^53, about 90,071,992 SOQ; a
// value read through one can be off by one shor above that. ParseSOQ is exact
// for every value the node can print.
func ParseSOQ(s string) (int64, error) {
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
		if frac == "" || len(frac) > 8 {
			return 0, fmt.Errorf("%w: %q", ErrAmountFormat, s)
		}
	}
	if whole == "" {
		return 0, fmt.Errorf("%w: %q", ErrAmountFormat, s)
	}
	var soq int64
	for i := 0; i < len(whole); i++ {
		c := whole[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrAmountFormat, s)
		}
		d := int64(c - '0')
		if soq > (math.MaxInt64-d)/10 {
			return 0, fmt.Errorf("%w: %q exceeds MaxInt64 shors", ErrAmountFormat, s)
		}
		soq = soq*10 + d
	}
	if soq > math.MaxInt64/ShorsPerSOQ {
		return 0, fmt.Errorf("%w: %q exceeds MaxInt64 shors", ErrAmountFormat, s)
	}
	shors := soq * ShorsPerSOQ
	var fracShors int64
	for i := 0; i < 8; i++ {
		fracShors *= 10
		if i < len(frac) {
			c := frac[i]
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("%w: %q", ErrAmountFormat, s)
			}
			fracShors += int64(c - '0')
		}
	}
	if shors > math.MaxInt64-fracShors {
		return 0, fmt.Errorf("%w: %q exceeds MaxInt64 shors", ErrAmountFormat, s)
	}
	return shors + fracShors, nil
}
