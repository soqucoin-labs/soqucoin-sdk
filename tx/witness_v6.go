// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"errors"
	"fmt"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
)

// ErrWitnessProgramSize is returned for a witness program that is not 32 bytes.
var ErrWitnessProgramSize = errors.New("tx: witness program must be 32 bytes")

// ScriptWitnessV6 returns the witness version 6 output script of a 32-byte
// program: OP_6, the push of 32 bytes, the program. The program is the SHA-256
// of the witnessScript that spends the output; the node recognises this
// 34-byte form and compares the hash when the output is spent
// (src/script/interpreter.cpp, VerifyScript).
//
// A program of any other length is refused with ErrWitnessProgramSize: the
// node has no spend path for it, so the output could never be spent.
//
// The function is the same in every chain state, and the node is not: it
// enforces a version 6 script from the activation height of the deployment.
// In a block below that height consensus does not check the script the
// program commits to. A caller that funds an output of this form first
// confirms with its node that witness version 6 is enforced for the next
// block.
//
// No function of this SDK that takes an address returns this script. The
// payment address path (address.ScriptFor) stays witness version 1 only.
func ScriptWitnessV6(program []byte) ([]byte, error) {
	if len(program) != soqaddr.WitnessProgramSize {
		return nil, fmt.Errorf("%w: got %d", ErrWitnessProgramSize, len(program))
	}
	script := make([]byte, 2+soqaddr.WitnessProgramSize)
	script[0] = 0x56 // OP_6 (witness version 6)
	script[1] = 0x20 // Push 32 bytes
	copy(script[2:], program)
	return script, nil
}
