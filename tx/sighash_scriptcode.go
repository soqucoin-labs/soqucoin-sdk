// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import "errors"

// ComputeSigHashWithScriptCode is declared here ahead of its body so that the
// tests of the digest compile and fail on their assertions.
func (tx *Transaction) ComputeSigHashWithScriptCode(inputIndex int, scriptCode []byte, hashType uint32) ([]byte, error) {
	return nil, errors.New("tx: ComputeSigHashWithScriptCode is not implemented")
}
