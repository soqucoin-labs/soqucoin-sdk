// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package address

// The witness version 6 address form: bech32m, witness version 6, a 32-byte
// program, the prefix of the network. The program is the SHA-256 of a
// witnessScript, and the output it names is OP_6 <program>
// (tx.ScriptWitnessV6).
//
// The form is not a payment destination. Decode, Validate, ScriptFor and
// AddressToScriptHash refuse it as they refuse every version but 1, and the
// allowlist they share is unchanged, so nothing that pays a user-supplied
// address can reach a version 6 output. A caller that works with a script
// output asks for this form by name, through the two functions below.
//
// An address is a way to write a program down. It does not say which script
// the program commits to: a party that is shown a version 6 address derives
// the witnessScript itself and compares its SHA-256 with the decoded program.
// Nor does the prefix "sq" say which of mainnet and regtest the address was
// made for; the two share it (see NetworkOf).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// witnessV6 is the witness version of a script-hash output.
const witnessV6 = 6

// bech32Const is the checksum constant of bech32 (BIP-173). Addresses of
// witness version 1 and above are bech32m (BIP-350), so a string whose
// checksum verifies under this constant is refused.
const bech32Const = 1

// ErrInvalidPadding is returned for a data part that does not hold a whole
// number of bytes: non-zero padding bits, or a spare group.
var ErrInvalidPadding = errors.New("bech32m: invalid padding")

// isNetworkPrefix reports whether hrp is the prefix of one of this SDK's
// networks, written exactly as the network writes it. A network has a genesis
// hash; a prefix that has none belongs to no network.
func isNetworkPrefix(hrp string) bool {
	return len(types.GenesisHashesForHRP(hrp)) > 0
}

// EncodeWitnessV6 returns the witness version 6 address of a 32-byte program
// under a network's prefix, in lower case.
//
// It refuses a program of any other length with ErrInvalidLength: the node
// runs the version 6 spend path for OP_6 with a 32-byte program only, so an
// output with another program length can never be spent. It refuses a prefix
// that is not one of this SDK's networks' with ErrInvalidHRP.
func EncodeWitnessV6(hrp string, program []byte) (string, error) {
	if !isNetworkPrefix(hrp) {
		return "", fmt.Errorf("%w: %q is the prefix of no network", ErrInvalidHRP, hrp)
	}
	if len(program) != WitnessProgramSize {
		return "", fmt.Errorf("%w: witness program length %d, want %d",
			ErrInvalidLength, len(program), WitnessProgramSize)
	}
	return Encode(hrp, witnessV6, program)
}

// DecodeWitnessV6 returns the 32-byte program of a witness version 6 address
// written under the given network prefix. Like the node it accepts the string
// in lower case or entirely in upper case.
//
// The checks run in the order of the vectors shared with the node
// (testdata/witness_address_vectors.json), and each refusal wraps one of this
// package's errors:
//
//	the empty string                                   ErrInvalidLength
//	a byte outside the printable ASCII range           ErrInvalidChar
//	mixed case                                         ErrInvalidChar
//	more than MaxAddressLength characters              ErrInvalidLength
//	no separator, or nothing before it                 ErrInvalidHRP
//	fewer than six characters after the separator      ErrInvalidLength
//	a character outside the bech32 alphabet            ErrInvalidChar
//	a bech32 checksum, or any other that is not bech32m ErrInvalidChecksum
//	a prefix other than hrp                            ErrInvalidHRP
//	no witness version, or a version other than 6      ErrUnsupportedWitnessVersion
//	non-zero padding bits or a spare group             ErrInvalidPadding
//	a program that is not 32 bytes                     ErrInvalidLength
//
// A prefix argument that is not one of this SDK's networks' is refused with
// ErrInvalidHRP before the string is read. The program is nil on every
// refusal.
func DecodeWitnessV6(hrp, addr string) ([]byte, error) {
	if !isNetworkPrefix(hrp) {
		return nil, fmt.Errorf("%w: %q is the prefix of no network", ErrInvalidHRP, hrp)
	}
	if addr == "" {
		return nil, fmt.Errorf("%w: empty string", ErrInvalidLength)
	}
	lower, upper := false, false
	for i := 0; i < len(addr); i++ {
		switch c := addr[i]; {
		case c < 33 || c > 126:
			return nil, fmt.Errorf("%w: byte %#02x at offset %d", ErrInvalidChar, c, i)
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		}
	}
	if lower && upper {
		return nil, fmt.Errorf("%w: mixed case", ErrInvalidChar)
	}
	if len(addr) > MaxAddressLength {
		return nil, fmt.Errorf("%w: %d characters, limit %d", ErrInvalidLength, len(addr), MaxAddressLength)
	}
	addr = strings.ToLower(addr)

	pos := strings.LastIndexByte(addr, '1')
	switch {
	case pos < 0:
		return nil, fmt.Errorf("%w: no separator", ErrInvalidHRP)
	case pos == 0:
		return nil, fmt.Errorf("%w: nothing before the separator", ErrInvalidHRP)
	case pos+7 > len(addr):
		return nil, fmt.Errorf("%w: %d characters after the separator, the checksum alone is 6",
			ErrInvalidLength, len(addr)-pos-1)
	}

	data := make([]int, len(addr)-pos-1)
	for i := range data {
		c := addr[pos+1+i]
		if charsetRev[c] == -1 {
			return nil, fmt.Errorf("%w: %q is outside the bech32 alphabet", ErrInvalidChar, c)
		}
		data[i] = int(charsetRev[c])
	}

	gotHRP := addr[:pos]
	switch polymod(append(hrpExpand(gotHRP), data...)) {
	case bech32mConst:
	case bech32Const:
		return nil, fmt.Errorf("%w: a bech32 checksum where bech32m is required", ErrInvalidChecksum)
	default:
		return nil, ErrInvalidChecksum
	}
	if gotHRP != hrp {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrInvalidHRP, hrp, gotHRP)
	}

	data = data[:len(data)-6]
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: no witness version", ErrUnsupportedWitnessVersion)
	}
	if data[0] != witnessV6 {
		return nil, fmt.Errorf("%w: version %d where the version 6 form was asked for",
			ErrUnsupportedWitnessVersion, data[0])
	}

	// With groups of five bits in range, the conversion fails on the padding
	// and on nothing else.
	program, err := convertBits(data[1:], 5, 8, false)
	if err != nil {
		return nil, fmt.Errorf("%w: the data part is not a whole number of bytes", ErrInvalidPadding)
	}
	if len(program) != WitnessProgramSize {
		return nil, fmt.Errorf("%w: witness program length %d, want %d",
			ErrInvalidLength, len(program), WitnessProgramSize)
	}
	return program, nil
}
