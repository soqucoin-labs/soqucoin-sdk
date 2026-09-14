package types

import (
	"errors"
	"math"
	"testing"
)

// ParseSOQ is exact for every value the node prints, including values past
// 2^53 shors where a float64 can no longer hold every shor.
func TestParseSOQExact(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"0.00000000", 0},
		{"0.00000001", 1},
		{"1.50000000", 150_000_000},
		{"1.5", 150_000_000},
		{"88", 8_800_000_000},
		{"12345.67891234", 1_234_567_891_234},
		{"90071992.54740992", 1 << 53},
		{"90071992.54740993", 1<<53 + 1},
		{"20000000000.00000000", 20_000_000_000 * ShorsPerSOQ},
		{"92233720368.54775807", math.MaxInt64},
	} {
		got, err := ParseSOQ(tc.in)
		if err != nil {
			t.Errorf("ParseSOQ(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSOQ(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Everything that is not the node's decimal form is refused with
// ErrAmountFormat: signs, exponents, a ninth fraction digit, a bare point,
// text, and values that overflow an int64 of shors.
func TestParseSOQRefusesOtherForms(t *testing.T) {
	for _, in := range []string{
		"", ".", "1.", ".5", "-1", "-1.0", "+1", "1e5", "1E-8", "0x10", "abc", "1.2.3", "1,000",
		"0.000000001", "1 ", " 1",
		"92233720369", "92233720368.54775808", "99999999999999999999",
	} {
		got, err := ParseSOQ(in)
		if !errors.Is(err, ErrAmountFormat) {
			t.Errorf("ParseSOQ(%q) = %d, %v; want ErrAmountFormat", in, got, err)
		}
	}
}
