// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package address

import "errors"

// ErrInvalidPadding is returned for a data part that does not hold a whole
// number of bytes: non-zero padding bits, or a spare group.
var ErrInvalidPadding = errors.New("bech32m: invalid padding")

var errWitnessV6NotBuilt = errors.New("address: the witness version 6 form is declared and has no body yet")

// EncodeWitnessV6 returns the witness version 6 address of a 32-byte program.
func EncodeWitnessV6(hrp string, program []byte) (string, error) {
	return "", errWitnessV6NotBuilt
}

// DecodeWitnessV6 returns the 32-byte program of a witness version 6 address.
func DecodeWitnessV6(hrp, addr string) ([]byte, error) {
	return nil, errWitnessV6NotBuilt
}
