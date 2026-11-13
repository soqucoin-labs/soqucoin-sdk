// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// DecodeWitnessV6 on strings the vectors shared with the node do not hold:
// bytes above 126, bytes at the edges of the ranges the byte and case checks
// test, and strings that fail two checks at once.

package address

import (
	"errors"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
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

// The byte check and the two letter ranges of the case check, at their edges.
//
// '!' (33) and '~' (126) are the lowest and the highest byte the byte check
// lets through. '@', '[', '`' and '{' are the neighbours of 'A', 'Z', 'a' and
// 'z' and are not letters. Written over the first byte of the prefix, each
// leaves a string that passes the byte and case checks and fails the checksum.
//
// 'a', 'z', 'A' and 'Z' are letters: a string whose only upper-case letter is
// one of them, or whose only lower-case letter is, is mixed case and refused.
func TestWitnessV6DecoderByteAndCaseEdges(t *testing.T) {
	letters := map[string]int{"a": 0, "z": 0}
	for _, v := range loadVectors(t).Valid {
		if v.Version != 6 {
			continue
		}
		lower, upper := v.Address, strings.ToUpper(v.Address)

		for _, c := range []struct {
			in string
			b  byte
		}{
			{lower, '!'}, {lower, '~'}, {lower, '@'}, {lower, '['},
			{upper, '`'}, {upper, '{'},
		} {
			written := []byte(c.in)
			written[0] = c.b
			program, err := DecodeWitnessV6(v.Prefix, string(written))
			if program != nil || !errors.Is(err, reasonClass["checksum"]) {
				t.Errorf("%q with %q as its first byte: DecodeWitnessV6 = %x, %v; want ErrInvalidChecksum",
					c.in, c.b, program, err)
			}
		}

		for letter := range letters {
			if !strings.Contains(lower, letter) {
				continue
			}
			letters[letter]++
			for _, written := range []string{
				strings.ReplaceAll(lower, letter, strings.ToUpper(letter)),
				strings.ReplaceAll(upper, strings.ToUpper(letter), letter),
			} {
				program, err := DecodeWitnessV6(v.Prefix, written)
				if program != nil || !errors.Is(err, reasonClass["mixed_case"]) {
					t.Errorf("%q: DecodeWitnessV6 = %x, %v; want ErrInvalidChar", written, program, err)
				}
			}
		}
	}
	for letter, n := range letters {
		if n == 0 {
			t.Errorf("no valid version 6 vector holds the letter %q", letter)
		}
	}
}

// checksummed writes a prefix, the separator, the groups of five bits and the
// bech32m checksum over them. The string passes the checksum whatever the
// prefix and the groups are.
func checksummed(hrp string, groups []int) string {
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, g := range groups {
		sb.WriteByte(charset[g])
	}
	for _, g := range createChecksum(hrp, groups) {
		sb.WriteByte(charset[g])
	}
	return sb.String()
}

// groupsOf is the data part of an address before its checksum: the witness
// version, then the program in groups of five bits, zero padded.
func groupsOf(t *testing.T, version int, program []byte) []int {
	t.Helper()
	in := make([]int, len(program))
	for i, b := range program {
		in[i] = int(b)
	}
	conv, err := convertBits(in, 8, 5, true)
	if err != nil {
		t.Fatalf("groups of %x: %v", program, err)
	}
	groups := []int{version}
	for _, g := range conv {
		groups = append(groups, int(g))
	}
	return groups
}

// orderCase is a string whose refusal depends on the order of the checks of
// DecodeWitnessV6. reason names the check that refuses it, in the words of the
// shared vectors.
type orderCase struct {
	name    string
	prefix  string
	address string
	reason  string
}

func orderCases(t *testing.T) []orderCase {
	t.Helper()
	program := progOf(0x5a)
	other := map[string]string{types.Mainnet.HRP: types.Stagenet.HRP, types.Stagenet.HRP: types.Mainnet.HRP}

	// For each prefix, a string the shared vectors refuse for its bech32
	// checksum. Any of them serves, and the first in the file is taken.
	bech32Vector := map[string]string{}
	for _, v := range loadVectors(t).Invalid {
		if _, taken := bech32Vector[v.Prefix]; !taken && v.Version == 6 && v.Reason == "checksum_bech32" {
			bech32Vector[v.Prefix] = v.Address
		}
	}

	var cases []orderCase
	for _, hrp := range []string{types.Mainnet.HRP, types.Stagenet.HRP} {
		valid, err := EncodeWitnessV6(hrp, program)
		if err != nil {
			t.Fatalf("EncodeWitnessV6(%q): %v", hrp, err)
		}
		sep := len(hrp)
		long := valid + strings.Repeat("q", MaxAddressLength+1-len(valid))
		bech32, held := bech32Vector[other[hrp]]
		if !held {
			t.Fatalf("the vectors hold no version 6 string with a bech32 checksum under %q", other[hrp])
		}

		version1, err := Encode(other[hrp], 1, program)
		if err != nil {
			t.Fatalf("Encode(%q, 1): %v", other[hrp], err)
		}
		padded := groupsOf(t, 1, program)
		padded[len(padded)-1] |= 1

		cases = append(cases,
			// A refused byte and mixed case come before the length limit.
			orderCase{"a space in a string of more than 90 characters", hrp, long + " ", "whitespace"},
			orderCase{"mixed case in a string of more than 90 characters", hrp, strings.ToUpper(long[:1]) + long[1:], "mixed_case"},

			// The length limit comes before the separator, and 90 characters are
			// within it.
			orderCase{"more than 90 characters and no separator", hrp, strings.Repeat("q", MaxAddressLength+1), "length_over_90"},
			orderCase{"more than 90 characters and nothing before the separator", hrp, "1" + strings.Repeat("q", MaxAddressLength), "length_over_90"},
			orderCase{"90 characters and no separator", hrp, strings.Repeat("q", MaxAddressLength), "separator_missing"},

			// No separator comes before the six characters of the checksum and
			// before the alphabet.
			orderCase{"no separator and five characters", hrp, "qqqqq", "separator_missing"},
			orderCase{"no separator and seven characters, one outside the alphabet", hrp, "bqqqqqq", "separator_missing"},

			// An empty prefix comes before the six characters of the checksum.
			orderCase{"nothing before the separator and five characters after it", hrp, "1qqqqq", "prefix_empty"},

			// The six characters of the checksum come before the alphabet.
			orderCase{"five characters after the separator, one outside the alphabet", hrp, hrp + "1bqqqq", "checksum_short"},

			// A bech32 checksum comes before the prefix. The vectors hold the
			// string under its own prefix only.
			orderCase{"a bech32 checksum under the other prefix", hrp, bech32, "checksum_bech32"},

			// The prefix comes before the witness version.
			orderCase{"a version 1 address of the other prefix", hrp, version1, "prefix"},
			orderCase{"no witness version under the other prefix", hrp, checksummed(other[hrp], nil), "prefix"},

			// The witness version comes before the padding.
			orderCase{"version 1 with a non-zero padding bit", hrp, checksummed(hrp, padded), "version"},

			// The separator is the last "1" of the string.
			orderCase{"a 1 among the last six characters", hrp, valid[:len(valid)-3] + "1" + valid[len(valid)-2:], "checksum_short"},
			orderCase{"a 1 in the data part before the last six characters", hrp, valid[:sep+10] + "1" + valid[sep+11:], "checksum"},
			orderCase{"a 1 inside the prefix, under the checksum of that prefix", hrp, checksummed(hrp[:1]+"1"+hrp[1:], groupsOf(t, 6, program)), "prefix"},
		)
	}
	return cases
}

// The order of the checks, where the shared vectors leave it open. Each case
// but one fails two checks, and DecodeWitnessV6 reports the one that comes
// first in the order it documents; the other case is a string of exactly 90
// characters, which the length limit lets through. The reason of a case is
// what classify() of the node's generator (gen_witness_address_vectors.py,
// beside the vectors in the node's tree) returns for the same string.
//
// With the vectors, these cases hold every pair of neighbouring checks that
// one string can fail with two different errors, and the rule that the
// separator is the last "1". Two neighbouring pairs stay open. The empty
// string has no byte to refuse, so no string fails the first two checks
// together. A refused byte and mixed case both return ErrInvalidChar, so the
// error does not tell which of the two was checked first.
func TestWitnessV6DecoderOrderOfChecks(t *testing.T) {
	for _, c := range orderCases(t) {
		want, known := reasonClass[c.reason]
		if !known {
			t.Fatalf("%s: reason %q has no error class", c.name, c.reason)
		}
		program, err := DecodeWitnessV6(c.prefix, c.address)
		if program != nil || !errors.Is(err, want) {
			t.Errorf("%s, %q under %q: DecodeWitnessV6 = %x, %v; want the error of %q, %q",
				c.name, c.address, c.prefix, program, err, c.reason, want)
		}
	}
}
