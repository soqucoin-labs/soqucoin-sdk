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

// DeriveSeed is a published scheme: HMAC-SHA256(master, domain || hrp || "/"
// || index). These vectors pin it. An integrator who implements the scheme in
// their own key-management system must land on the same seeds and addresses;
// a change to the domain string, the HRP encoding or the index encoding fails
// here first.
func TestDeriveSeedVectors(t *testing.T) {
	var master [32]byte
	for i := range master {
		master[i] = byte(i)
	}
	cases := []struct {
		hrp     string
		index   uint32
		seed    string
		address string // FromSeed(hrp, seed)
	}{
		{"sq", 0, "d69ce1bf3460d6d29ac7fda99eb661d8d3e356f503517488230da95bfa2522da", "sq1py24wh5pnmv0r2sxkp6szq4t3ffwgjsmkk9wwkmdwm623mg5jw5dqpvfrfg"},
		{"sq", 1, "bf84c2ed07fe03340293480578bbd2be6b46aaeb642ffee9a86b2af2aef783ad", "sq1pxaq2fv8j6fayzlgyce967w7fuymqxytsk4fu2zdyx80er7julg8s4uyqdp"},
		{"sq", 0xFFFFFFFF, "cc9e53ef5f1589ff6b18f7949b8c49a6a9c4de60bb8b18edc1780cc3a1c47d30", "sq1pgrtjsngzgflau69sa9vmlr98u3xfznqx7thgczfeqfl9xj7m7q4s0l5utv"},
		{"ssq", 0, "3e8199d248b1937624e7957516bc76f48ea3da350a2cd502fe0e0e759f68ca48", "ssq1p03cjmljk8mmruetjg46h23qsqtlzvwldd66wwup2cg7m0a7w9pussyseaz"},
		{"ssq", 1, "192ac037c943c34c02e2a4a5e1b616fcb2008dd0a0812259d545d8995c47d5cf", "ssq1plu2t4n0p0fq7fek93ytap00ak4the6ddf2c6wsnwxtq3m3gz36qqs9y3pc"},
		{"ssq", 0xFFFFFFFF, "e945274e9f692fa3f89d7a7accebe8019d55499505e614f7b8843cb207355fd2", "ssq1p50czjtcfmwhmrcw0pn9suhzl85lyu35k2xxqqfqq9zsx7ld5pd2qmkf90h"},
	}
	for _, c := range cases {
		seed, err := DeriveSeed(master[:], c.hrp, c.index)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(seed[:]); got != c.seed {
			t.Errorf("%s/%d: seed %s, want %s", c.hrp, c.index, got, c.seed)
		}
		kp, err := FromSeed(c.hrp, seed)
		if err != nil {
			t.Fatalf("%s/%d: %v", c.hrp, c.index, err)
		}
		if kp.Address != c.address {
			t.Errorf("%s/%d: address %s, want %s", c.hrp, c.index, kp.Address, c.address)
		}
	}
}

// The attack the network binding closes: a production master that reaches a
// stagenet host. Under v1 the stagenet key at every index was the mainnet key
// with another prefix; under v2 the two derivations share nothing.
func TestDeriveSeedBindsTheNetwork(t *testing.T) {
	master := bytes.Repeat([]byte{0x5A}, 32)
	main, err := DeriveSeed(master, "sq", 7)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := DeriveSeed(master, "ssq", 7)
	if err != nil {
		t.Fatal(err)
	}
	if main == stage {
		t.Fatal("mainnet and stagenet derive the same seed for one master and index")
	}
	kpMain, err := FromSeed("sq", main)
	if err != nil {
		t.Fatal(err)
	}
	kpStage, err := FromSeed("ssq", stage)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(kpMain.PublicKey, kpStage.PublicKey) {
		t.Fatal("mainnet and stagenet derive the same key")
	}
	// A stagenet key re-encoded with the mainnet prefix is not a mainnet
	// deposit address this master ever produced.
	reencoded, err := AddressFor("sq", kpStage.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if reencoded == kpMain.Address {
		t.Fatal("the stagenet key re-encoded for mainnet is the mainnet deposit address")
	}
}

func TestDeriveSeedRefusesShortMasterUnknownHRPAndSeparatesInputs(t *testing.T) {
	if _, err := DeriveSeed(make([]byte, MinMasterSize-1), "sq", 0); !errors.Is(err, ErrShortMaster) {
		t.Fatalf("31-byte master accepted: %v", err)
	}
	if _, err := DeriveSeed(nil, "sq", 0); !errors.Is(err, ErrShortMaster) {
		t.Fatalf("nil master accepted: %v", err)
	}
	m1 := bytes.Repeat([]byte{0xA5}, 32)
	for _, hrp := range []string{"", "bc", "SQ", "sq/", "soq"} {
		if _, err := DeriveSeed(m1, hrp, 0); !errors.Is(err, ErrUnknownHRP) {
			t.Errorf("hrp %q accepted: %v", hrp, err)
		}
	}
	m2 := bytes.Repeat([]byte{0xA5}, 33)
	s10, _ := DeriveSeed(m1, "sq", 0)
	s11, _ := DeriveSeed(m1, "sq", 1)
	s20, _ := DeriveSeed(m2, "sq", 0)
	if s10 == s11 {
		t.Error("indices 0 and 1 derive the same seed")
	}
	if s10 == s20 {
		t.Error("two masters derive the same seed for index 0")
	}
	// The scheme as an integrator would write it in their own key-management
	// system, spelled out independently of DeriveSeed's implementation.
	mac := hmac.New(sha256.New, m1)
	mac.Write([]byte("soqucoin-sdk/keys/seed/v2/sq/"))
	mac.Write([]byte{0, 0, 0, 1})
	if !bytes.Equal(mac.Sum(nil), s11[:]) {
		t.Error("DeriveSeed(sq, 1) is not HMAC-SHA256(master, domain || \"sq/\" || 00000001)")
	}
}
