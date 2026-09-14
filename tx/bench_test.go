// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Signing a maximal 80-input payout: sighash and ML-DSA-44 signature per
// input. The exchange guide quotes one run ("Signing, measured").
func BenchmarkSignAll80Inputs(b *testing.B) {
	m := keys.NewManager(filepath.Join(b.TempDir(), "k.enc"), "pw")
	kp, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		b.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		b.Fatal(err)
	}
	var inputs []types.UTXO
	for i := 0; i < 80; i++ {
		inputs = append(inputs, types.UTXO{TxID: displayTxID, Vout: uint32(i), Value: 1_000_000_000, Address: kp.Address})
	}
	change, _ := address.ScriptFor(kp.Address)
	tr, err := BuildSendTransaction(inputs, ScriptP2WPKH(hash32(0x02)), 70_000_000_000, change, types.RecommendedFeeRate)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tr.SignAll(m); err != nil {
			b.Fatal(err)
		}
	}
}
