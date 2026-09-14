// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package keys

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The figures an exchange sizes a signing host from. Run them on your own box:
//
//	go test ./keys -run '^$' -bench . -benchmem
//
// The exchange guide (docs/EXCHANGE_INTEGRATION.md, "Signing, measured")
// quotes one run of these.

func benchManager(b *testing.B) (*Manager, *KeyPair) {
	b.Helper()
	m := NewManager(filepath.Join(b.TempDir(), "k.enc"), "benchmark-passphrase")
	kp, err := GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		b.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		b.Fatal(err)
	}
	return m, kp
}

// One ML-DSA-44 signature over a 32-byte digest, the per-input cost of signing
// a transaction.
func BenchmarkManagerSign(b *testing.B) {
	m, kp := benchManager(b)
	digest := sha256.Sum256([]byte("benchmark"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Sign(kp.Address, digest[:]); err != nil {
			b.Fatal(err)
		}
	}
}

// One ML-DSA-44 verification, the per-input cost of keys.Verify.
func BenchmarkVerify(b *testing.B) {
	m, kp := benchManager(b)
	digest := sha256.Sum256([]byte("benchmark"))
	sig, err := m.Sign(kp.Address, digest[:])
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, err := Verify(kp.PublicKey, digest[:], sig)
		if err != nil || !ok {
			b.Fatal(err, ok)
		}
	}
}

// One keystore write: Argon2id (t=3, m=64 MiB, p=4), AES-256-GCM and the
// durable write to disk. This is what adding a hot-wallet key costs.
func BenchmarkManagerSave(b *testing.B) {
	m, _ := benchManager(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.Save(); err != nil {
			b.Fatal(err)
		}
	}
}

// One key generation, the cost of a fresh hot-wallet key.
func BenchmarkGenerateKeyForNetwork(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := GenerateKeyForNetwork(types.Stagenet.HRP); err != nil {
			b.Fatal(err)
		}
	}
}
