// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import "errors"

// ErrWitnessProgramSize is returned for a witness program that is not 32 bytes.
var ErrWitnessProgramSize = errors.New("tx: witness program must be 32 bytes")

// ScriptWitnessV6 returns the witness version 6 output script of a 32-byte
// program.
func ScriptWitnessV6(program []byte) ([]byte, error) {
	return nil, errors.New("tx: the witness version 6 output is declared and has no body yet")
}
