// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// DecodeWitnessV6 on strings the vectors shared with the node do not hold.

package address

import (
	"errors"
	"strings"
	"testing"
)

// A byte above 126 is refused by the byte check, before anything reads the
// bech32 table. The highest byte in the shared vectors is 'z' (0x7a), so they
// do not reach the upper bound: here DEL, the first byte outside ASCII and the
// last are written over the first byte of the prefix and over a byte of the
// data part of every valid version 6 vector.
func TestWitnessV6DecoderRefusesBytesAbove126(t *testing.T) {
	for _, v := range loadVectors(t).Valid {
		if v.Version != 6 {
			continue
		}
		sep := strings.LastIndexByte(v.Address, '1')
		for _, at := range []int{0, sep + 2} {
			for _, b := range []byte{0x7f, 0x80, 0xff} {
				written := []byte(v.Address)
				written[at] = b
				program, err := DecodeWitnessV6(v.Prefix, string(written))
				if program != nil || !errors.Is(err, ErrInvalidChar) {
					t.Errorf("%q with byte %#02x at offset %d: DecodeWitnessV6 = %x, %v; want ErrInvalidChar",
						v.Address, b, at, program, err)
				}
			}
		}
	}
}
