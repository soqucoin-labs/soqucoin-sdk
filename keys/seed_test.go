package keys

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
)

// FromSeed must reproduce the node-derived vectors exactly: same seed, same
// witness program, same address on both networks. The vectors come from the
// node's own encoder (see node_vectors_test.go); this test ties the public
// derivation entry point to them, not just the address encoder.
func TestFromSeedMatchesNodeVectors(t *testing.T) {
	for _, v := range nodeVectors {
		for hrp, want := range map[string]string{"sq": v.sq, "ssq": v.ssq} {
			kp, err := FromSeed(hrp, v.seed)
			if err != nil {
				t.Fatalf("%s/%s: %v", v.name, hrp, err)
			}
			if kp.Address != want {
				t.Errorf("%s/%s: FromSeed address %s, node says %s", v.name, hrp, kp.Address, want)
			}
			if got := hex.EncodeToString(PubKeyHash(kp.PublicKey)); got != v.program {
				t.Errorf("%s/%s: witness program %s, node says %s", v.name, hrp, got, v.program)
			}
			if len(kp.PrivateKey) != PrivateKeySize || len(kp.PublicKey) != PublicKeySize {
				t.Errorf("%s/%s: key sizes %d/%d", v.name, hrp, len(kp.PrivateKey), len(kp.PublicKey))
			}
		}
	}
}

// The attack: a seed whose key the node can never spend from. FromSeed must
// refuse it rather than hand out a deposit address that swallows funds.
func TestFromSeedRefusesInvalidMarkerSeed(t *testing.T) {
	seed := sha256.Sum256([]byte("soqucoin-ff-143"))
	invalidMarkerKey(t) // asserts the vector still produces a 0xFF key
	kp, err := FromSeed("sq", seed)
	if !errors.Is(err, ErrInvalidPublicKey) {
		t.Fatalf("FromSeed accepted the 0xFF-marker seed: kp=%v err=%v", kp, err)
	}
	if kp != nil {
		t.Fatal("FromSeed returned a KeyPair alongside the error")
	}
}

func TestFromSeedIsDeterministicAndSeedsDiffer(t *testing.T) {
	seed := sha256.Sum256([]byte("determinism"))
	a, err := FromSeed("sq", seed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromSeed("sq", seed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.PrivateKey, b.PrivateKey) || !bytes.Equal(a.PublicKey, b.PublicKey) || a.Address != b.Address {
		t.Fatal("two derivations from one seed differ")
	}
	seed[31] ^= 0x01
	c, err := FromSeed("sq", seed)
	if err != nil {
		t.Fatal(err)
	}
	if c.Address == a.Address {
		t.Fatal("a one-bit seed change produced the same address")
	}
	// The random generator and the seeded one produce the same key format:
	// a seeded key signs and verifies, and imports into a Manager, like any other.
	m := NewManager(filepath.Join(t.TempDir(), "k.json"), "pw")
	if err := m.ImportPrivateKey(a.PrivateKey, a.PublicKey, a.Address); err != nil {
		t.Fatalf("seeded key refused by ImportPrivateKey: %v", err)
	}
	digest := sha256.Sum256([]byte("sweep"))
	sig, err := m.Sign(a.Address, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(a.PublicKey, digest[:], sig); err != nil || !ok {
		t.Fatalf("signature from a seeded key does not verify: ok=%v err=%v", ok, err)
	}
}

// DeriveSeed is a published scheme: HMAC-SHA256(master, domain || index).
// These vectors pin it. An integrator who implements the scheme in their own
// key-management system must land on the same seeds and addresses; a change
// to the domain string or the index encoding fails here first.
func TestDeriveSeedVectors(t *testing.T) {
	var master [32]byte
	for i := range master {
		master[i] = byte(i)
	}
	cases := []struct {
		index   uint32
		seed    string
		address string // FromSeed("sq", seed)
	}{
		{0, "fbb22041c07fe40cc7a1678a3afb411b6005640d625f46f0f286e4e9b50b662d", "sq1pz5ynntauhsf42tr0qe49ne48m7k6ujv4dac5xkeua0q782gyw0ns96tyy7"},
		{1, "5ccbff2bef90678dba6301e419453279ea13081220d7ababf1f4f7222afe8f03", "sq1pq9x6dhgqwh88aaen23dzqe58u9d2f7kfhnhhn6dmsejnnepvdtqseu6qwp"},
		{0xFFFFFFFF, "1f5edd8755f63111a41a2675e8340fa1298d5517b158a6c4073790d8210377c3", "sq1pww8ccgjk7wtgcngvf5xxz0z2m72pcdxqnyzy45s3urp3t4nmzkuswc953d"},
	}
	for _, c := range cases {
		seed, err := DeriveSeed(master[:], c.index)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(seed[:]); got != c.seed {
			t.Errorf("index %d: seed %s, want %s", c.index, got, c.seed)
		}
		kp, err := FromSeed("sq", seed)
		if err != nil {
			t.Fatalf("index %d: %v", c.index, err)
		}
		if kp.Address != c.address {
			t.Errorf("index %d: address %s, want %s", c.index, kp.Address, c.address)
		}
	}
}

func TestDeriveSeedRefusesShortMasterAndSeparatesInputs(t *testing.T) {
	if _, err := DeriveSeed(make([]byte, MinMasterSize-1), 0); !errors.Is(err, ErrShortMaster) {
		t.Fatalf("31-byte master accepted: %v", err)
	}
	if _, err := DeriveSeed(nil, 0); !errors.Is(err, ErrShortMaster) {
		t.Fatalf("nil master accepted: %v", err)
	}
	m1 := bytes.Repeat([]byte{0xA5}, 32)
	m2 := bytes.Repeat([]byte{0xA5}, 33)
	s10, _ := DeriveSeed(m1, 0)
	s11, _ := DeriveSeed(m1, 1)
	s20, _ := DeriveSeed(m2, 0)
	if s10 == s11 {
		t.Error("indices 0 and 1 derive the same seed")
	}
	if s10 == s20 {
		t.Error("two masters derive the same seed for index 0")
	}
	// The scheme as an integrator would write it in their own key-management
	// system, spelled out independently of DeriveSeed's implementation.
	mac := hmac.New(sha256.New, m1)
	mac.Write([]byte("soqucoin-sdk/keys/seed/v1"))
	mac.Write([]byte{0, 0, 0, 1})
	if !bytes.Equal(mac.Sum(nil), s11[:]) {
		t.Error("DeriveSeed(index 1) is not HMAC-SHA256(master, domain || 00000001)")
	}
}
