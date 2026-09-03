package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cloudflare/circl/sign/mldsa/mldsa44"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
)

// Node-derived vectors (2026-09-03 review). Public keys come from FIPS 204
// KeyGen_internal on the seed; the witness program and both addresses were
// produced by the node's own EncodeDilithiumAddress / DecodeDestination /
// GetScriptForDestination linked from the node build, not by this SDK. If the
// SDK ever disagrees with these, the SDK is wrong.
var nodeVectors = []struct {
	name    string
	seed    [32]byte
	program string
	sq, ssq string
}{
	{"zero seed", seedBytes(0x00),
		"eb4e7302842153b0fa19e8620739ad258af4929c26dd89079a7ec7d4282208e1",
		"sq1pad88xq5yy9fmp7seap3qwwddyk90fy5uymwcjpu60mrag2pzprssvu8yzx",
		"ssq1pad88xq5yy9fmp7seap3qwwddyk90fy5uymwcjpu60mrag2pzprssak92hv"},
	{"0x01 seed", seedBytes(0x01),
		"bc7f72c940225e3998067ebef1d7cd2cad938de8f70b34c515a81d9efacac204",
		"sq1ph3lh9j2qyf0rnxqx06l0r47d9jke8r0g7u9nf3g44qwea7k2cgzq4ywnrx",
		"ssq1ph3lh9j2qyf0rnxqx06l0r47d9jke8r0g7u9nf3g44qwea7k2cgzqywvakv"},
	{"sha256(soqucoin)", sha256.Sum256([]byte("soqucoin")),
		"ec671f444afa19d8ee919c21c76fd2fbda2d17dc173fe75e94c2fdbea5ef366b",
		"sq1pa3n373z2lgva3m53nssuwm7jl0dz697uzul7wh55ct7maf00xe4s2m80fs",
		"ssq1pa3n373z2lgva3m53nssuwm7jl0dz697uzul7wh55ct7maf00xe4sm39pu6"},
	{"sha256(nonkyc)", sha256.Sum256([]byte("nonkyc")),
		"93a1a51925ca42babd7c83dfccd6479cb7ea6a6d396f14960375c882c02221c8",
		"sq1pjws62xf9efpt40tus00ue4j8njm756nd89h3f9srwhyg9spzy8yqxwzy6t",
		"ssq1pjws62xf9efpt40tus00ue4j8njm756nd89h3f9srwhyg9spzy8yqhyq20p"},
}

func seedBytes(b byte) [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = b
	}
	return s
}

// A seed whose FIPS 204 public key begins with 0xFF, the node's invalid-key
// marker. Node verdict for this key: CPubKey::IsValid = false.
func invalidMarkerKey(t *testing.T) (pk, sk []byte) {
	t.Helper()
	seed := sha256.Sum256([]byte("soqucoin-ff-143"))
	p, s := mldsa44.NewKeyFromSeed(&seed)
	pk, sk = p.Bytes(), s.Bytes()
	if pk[0] != 0xFF {
		t.Fatalf("vector drift: expected pk[0]=0xFF for the marker seed, got %#x", pk[0])
	}
	return pk, sk
}

func TestAddressDerivationMatchesNodeVectors(t *testing.T) {
	for _, v := range nodeVectors {
		pk, _ := mldsa44.NewKeyFromSeed(&v.seed)
		pkBytes := pk.Bytes()
		if got := hex.EncodeToString(PubKeyHash(pkBytes)); got != v.program {
			t.Errorf("%s: witness program %s, node says %s", v.name, got, v.program)
		}
		for hrp, want := range map[string]string{"sq": v.sq, "ssq": v.ssq} {
			got, err := AddressFor(hrp, pkBytes)
			if err != nil {
				t.Fatalf("%s/%s: %v", v.name, hrp, err)
			}
			if got != want {
				t.Errorf("%s/%s: SDK address %s, node says %s", v.name, hrp, got, want)
			}
			spk, err := soqaddr.ScriptFor(got)
			if err != nil {
				t.Fatalf("%s/%s ScriptFor: %v", v.name, hrp, err)
			}
			if hex.EncodeToString(spk) != "5120"+v.program {
				t.Errorf("%s/%s: scriptPubKey %x, node says 5120%s", v.name, hrp, spk, v.program)
			}
		}
	}
}

// The node can never spend from a key whose public key starts with 0xFF. The
// generator must never hand one out. 3000 draws: without the regeneration loop
// the expected number of marker keys is ~11.7 and P(zero) is about 8e-6.
func TestGenerateKeyNeverReturnsInvalidMarkerKey(t *testing.T) {
	for i := 0; i < 3000; i++ {
		kp, err := GenerateKeyForNetwork("sq")
		if err != nil {
			t.Fatal(err)
		}
		if kp.PublicKey[0] == 0xFF {
			t.Fatalf("draw %d: generator returned a public key starting with 0xFF (%s); the node cannot spend from it", i, kp.Address)
		}
	}
}

func TestImportRejectsInvalidMarkerKey(t *testing.T) {
	pk, sk := invalidMarkerKey(t)
	pkHash := sha256.Sum256(pk)
	addr, err := soqaddr.Encode("sq", 1, pkHash[:])
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(t.TempDir(), "k.json"), "pw")
	err = m.ImportPrivateKey(sk, pk, addr)
	if !errors.Is(err, ErrInvalidPublicKey) {
		t.Fatalf("ImportPrivateKey accepted the 0xFF-marker key %s: err=%v", addr, err)
	}
	if _, err := AddressFor("sq", pk); !errors.Is(err, ErrInvalidPublicKey) {
		t.Errorf("AddressFor derived an address for the marker key: %v", err)
	}
	if _, err := Verify(pk, pkHash[:], make([]byte, SignatureSize)); !errors.Is(err, ErrInvalidPublicKey) {
		t.Errorf("Verify accepted the marker key: %v", err)
	}
}

func TestImportRejectsMismatchedRecords(t *testing.T) {
	a, err := GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(t.TempDir(), "k.json"), "pw")

	if err := m.ImportPrivateKey(a.PrivateKey, b.PublicKey, b.Address); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("private key of A with public key of B accepted: %v", err)
	}
	if err := m.ImportPrivateKey(a.PrivateKey, a.PublicKey, b.Address); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("key A with address of B accepted: %v", err)
	}
	ssqAddr, _ := AddressFor("ssq", a.PublicKey)
	if err := m.ImportPrivateKey(a.PrivateKey, a.PublicKey, ssqAddr); err != nil {
		t.Errorf("consistent record on another network refused: %v", err)
	}
	if err := m.ImportPrivateKey(a.PrivateKey, a.PublicKey, a.Address); err != nil {
		t.Errorf("consistent record refused: %v", err)
	}
}

// A keystore holding an inconsistent record must not load: the manager would
// otherwise serve a deposit address it cannot sign for.
func TestLoadRejectsInconsistentKeystore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.json")
	m := NewManager(path, "pw")
	a, _ := GenerateKeyForNetwork("sq")
	b, _ := GenerateKeyForNetwork("sq")
	m.keys = []KeyPair{{PrivateKey: a.PrivateKey, PublicKey: a.PublicKey, Address: b.Address}}
	m.loaded = true
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(path, "pw")
	if err := m2.Load(); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Load accepted a keystore whose address does not belong to its key: %v", err)
	}
}

// Verify must read the forms the SDK itself emits on the wire: 0x00||pk and
// signature||hashtype.
func TestVerifyAcceptsWitnessForms(t *testing.T) {
	kp, err := GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(t.TempDir(), "k.json"), "pw")
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("withdrawal"))
	sig, err := m.Sign(kp.Address, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	pk1313 := append([]byte{0x00}, kp.PublicKey...)
	sig2421 := append(append([]byte{}, sig...), 0x01)
	for _, c := range []struct {
		name string
		pk   []byte
		sig  []byte
	}{
		{"raw", kp.PublicKey, sig},
		{"witness pk 1313", pk1313, sig},
		{"witness sig 2421", kp.PublicKey, sig2421},
		{"both witness forms", pk1313, sig2421},
	} {
		ok, err := Verify(c.pk, digest[:], c.sig)
		if err != nil || !ok {
			t.Errorf("%s: ok=%v err=%v", c.name, ok, err)
		}
	}
}
