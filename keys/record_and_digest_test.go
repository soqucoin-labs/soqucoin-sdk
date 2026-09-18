package keys

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

// The packed ML-DSA-44 private key is rho[0:32] K[32:64] tr[64:128] s1[128:512]
// s2[512:896] t0[896:2560]. The public key is recomputed from rho, s1 and s2,
// so a record whose tr bytes are altered derives its own public key and
// address and signs signatures that key refuses, every one of them, because
// the verifier hashes its own tr into the message. The record check refuses
// it at import and at load, the two ways a record enters a manager. (A change
// to t0 shifts the hints and fails only a fraction of signatures, growing with
// the size of the change; the self-test catches it at that rate, which is the
// rate a withdrawal would fail, and a case on it would be a coin toss.)
func TestARecordThatCannotSignForItsKeyIsRefused(t *testing.T) {
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		at   int
	}{{"first tr byte", 64}, {"last tr byte", 127}} {
		t.Run(tc.name+" altered", func(t *testing.T) {
			bad := bytes.Clone(kp.PrivateKey)
			bad[tc.at] ^= 0x01
			m := NewManager(filepath.Join(t.TempDir(), "keys.enc"), "pw")
			if err := m.ImportPrivateKey(bad, kp.PublicKey, kp.Address); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("ImportPrivateKey: %v, want ErrKeyMismatch", err)
			}
			if m.KeyCount() != 0 {
				t.Fatal("manager holds the record after a refused import")
			}

			// The same record written around the import check, as a build
			// without this check wrote it.
			path := filepath.Join(t.TempDir(), "keys.enc")
			w := NewManager(path, "pw")
			w.keys = []KeyPair{{PrivateKey: bad, PublicKey: kp.PublicKey, Address: kp.Address}}
			if err := w.Save(); err != nil {
				t.Fatal(err)
			}
			r := NewManager(path, "pw")
			if err := r.Load(); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("Load: %v, want ErrKeyMismatch", err)
			}
			if r.KeyCount() != 0 {
				t.Fatal("manager holds the record after a refused Load")
			}
		})
	}

	t.Run("the intact record signs for its key", func(t *testing.T) {
		m := NewManager(filepath.Join(t.TempDir(), "keys.enc"), "pw")
		if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		digest := bytes.Repeat([]byte{7}, 32)
		sig, err := m.Sign(kp.Address, digest)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := Verify(kp.PublicKey, digest, sig); !ok || err != nil {
			t.Fatalf("Verify: %v %v", ok, err)
		}
	})

	// K seeds the signer's randomness and takes no part in a signature's
	// validity, so a record with K altered signs signatures its key verifies,
	// and is accepted. The check is whether the record can spend, not whether
	// every byte is the one the generator wrote.
	t.Run("K altered is accepted and still signs for its key", func(t *testing.T) {
		alt := bytes.Clone(kp.PrivateKey)
		alt[40] ^= 0x01
		m := NewManager(filepath.Join(t.TempDir(), "keys.enc"), "pw")
		if err := m.ImportPrivateKey(alt, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		digest := bytes.Repeat([]byte{9}, 32)
		sig, err := m.Sign(kp.Address, digest)
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := Verify(kp.PublicKey, digest, sig); !ok {
			t.Fatal("a record with K altered does not sign for its key")
		}
	})
}

// A digest is 32 bytes. Sign and Verify refuse any other width rather than
// sign or verify over the wrong thing.
func TestSignAndVerifyTakeA32ByteDigest(t *testing.T) {
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(t.TempDir(), "keys.enc"), "pw")
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	digest := bytes.Repeat([]byte{3}, 32)
	sig, err := m.Sign(kp.Address, digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 31, 33, 64} {
		if _, err := m.Sign(kp.Address, make([]byte, n)); !errors.Is(err, ErrDigestSize) {
			t.Errorf("Sign with a %d-byte digest: %v, want ErrDigestSize", n, err)
		}
		if ok, err := Verify(kp.PublicKey, make([]byte, n), sig); ok || !errors.Is(err, ErrDigestSize) {
			t.Errorf("Verify with a %d-byte digest: %v %v, want false and ErrDigestSize", n, ok, err)
		}
	}
	if ok, err := Verify(kp.PublicKey, digest, sig); !ok || err != nil {
		t.Fatalf("Verify: %v %v", ok, err)
	}
}
