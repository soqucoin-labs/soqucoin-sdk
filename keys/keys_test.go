package keys

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateKey(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error: %v", err)
	}

	if len(kp.PrivateKey) != PrivateKeySize {
		t.Errorf("private key size = %d, want %d", len(kp.PrivateKey), PrivateKeySize)
	}
	if len(kp.PublicKey) != PublicKeySize {
		t.Errorf("public key size = %d, want %d", len(kp.PublicKey), PublicKeySize)
	}
	if !strings.HasPrefix(kp.Address, "ssq1p") {
		t.Errorf("address = %q, want ssq1p... prefix", kp.Address)
	}
	t.Logf("Generated address: %s", kp.Address)
}

func TestSignAndVerify(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error: %v", err)
	}

	// Create a test sighash (SHA-256 of a "transaction")
	sighash := sha256.Sum256([]byte("test transaction data for BIP143 sighash"))

	// Set up manager with the generated key
	mgr := NewManager("/dev/null", "test-passwd")
	mgr.keys = []KeyPair{*kp}

	// Sign
	sig, err := mgr.Sign(kp.Address, sighash[:])
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	if len(sig) != SignatureSize {
		t.Errorf("signature size = %d, want %d", len(sig), SignatureSize)
	}

	// Verify
	valid, err := Verify(kp.PublicKey, sighash[:], sig)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if !valid {
		t.Error("Verify() = false, want true")
	}
	t.Log("Sign + Verify: PASS")
}

func TestSignWrongKey(t *testing.T) {
	// Generate two different keypairs
	kp1, _ := GenerateKey()
	kp2, _ := GenerateKey()

	sighash := sha256.Sum256([]byte("test message"))

	// Sign with key1
	mgr := NewManager("/dev/null", "test-passwd")
	mgr.keys = []KeyPair{*kp1}

	sig, err := mgr.Sign(kp1.Address, sighash[:])
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	// Verify with key2's pubkey — should fail
	valid, err := Verify(kp2.PublicKey, sighash[:], sig)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if valid {
		t.Error("Verify with wrong pubkey = true, want false")
	}
	t.Log("Wrong key rejection: PASS")
}

func TestSignModifiedDigest(t *testing.T) {
	kp, _ := GenerateKey()
	sighash := sha256.Sum256([]byte("original message"))

	mgr := NewManager("/dev/null", "test-passwd")
	mgr.keys = []KeyPair{*kp}

	sig, _ := mgr.Sign(kp.Address, sighash[:])

	// Modify one byte of the digest
	tampered := sha256.Sum256([]byte("tampered message"))

	valid, _ := Verify(kp.PublicKey, tampered[:], sig)
	if valid {
		t.Error("Verify with tampered digest = true, want false")
	}
	t.Log("Tampered digest rejection: PASS")
}

func TestKeystoreRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := filepath.Join(tmpDir, "test-keystore.enc")
	passwd := "test-passphrase-123!"

	// Generate and import a key
	kp, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error: %v", err)
	}

	// Create manager, import key, save
	mgr1 := NewManager(keyFile, passwd)
	if err := mgr1.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatalf("ImportPrivateKey() error: %v", err)
	}
	if err := mgr1.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Verify file exists
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("keystore file not created: %v", err)
	}
	t.Logf("Keystore file: %d bytes", info.Size())

	// Load with correct password
	mgr2 := NewManager(keyFile, passwd)
	if err := mgr2.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if mgr2.KeyCount() != 1 {
		t.Fatalf("loaded key count = %d, want 1", mgr2.KeyCount())
	}

	// Sign with loaded key and verify
	sighash := sha256.Sum256([]byte("round-trip test"))
	sig, err := mgr2.Sign(kp.Address, sighash[:])
	if err != nil {
		t.Fatalf("Sign after load error: %v", err)
	}
	valid, _ := Verify(kp.PublicKey, sighash[:], sig)
	if !valid {
		t.Error("Verify after keystore round-trip = false, want true")
	}
	t.Log("Keystore round-trip: PASS")
}

// TestImportedKeySurvivesCallerZeroing pins that the manager owns its copy of
// an imported key: a caller that zeroes its buffers after import, as the
// security guide advises for seeds, must not corrupt the keystore. Before the
// copy the record shared the caller's slices; a Save after zeroing wrote an
// all-zero private key and Load rejected the record, with the funds at that
// address unspendable.
func TestImportedKeySurvivesCallerZeroing(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "keystore.enc")
	passwd := "test-passphrase-123!"

	kp, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error: %v", err)
	}
	pubKey := append([]byte(nil), kp.PublicKey...)

	mgr := NewManager(keyFile, passwd)
	if err := mgr.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatalf("ImportPrivateKey() error: %v", err)
	}
	for i := range kp.PrivateKey {
		kp.PrivateKey[i] = 0
	}
	for i := range kp.PublicKey {
		kp.PublicKey[i] = 0
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("Save() after caller zeroing: %v", err)
	}

	loaded := NewManager(keyFile, passwd)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load() after caller zeroing: %v", err)
	}
	sighash := sha256.Sum256([]byte("caller zeroing"))
	sig, err := loaded.Sign(kp.Address, sighash[:])
	if err != nil {
		t.Fatalf("Sign after load: %v", err)
	}
	if valid, _ := Verify(pubKey, sighash[:], sig); !valid {
		t.Fatal("signature from the reloaded key does not verify")
	}
}

func TestKeystoreWrongPassword(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := filepath.Join(tmpDir, "test-keystore.enc")

	kp, _ := GenerateKey()

	// Save with correct password
	mgr1 := NewManager(keyFile, "correct-password")
	mgr1.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address)
	mgr1.Save()

	// Try to load with wrong password
	mgr2 := NewManager(keyFile, "wrong-password")
	err := mgr2.Load()
	if err == nil {
		t.Error("Load with wrong password should fail")
	} else {
		t.Logf("Correctly rejected wrong password: %v", err)
	}
}

func TestSignUnknownAddress(t *testing.T) {
	mgr := NewManager("/dev/null", "test")

	sighash := sha256.Sum256([]byte("test"))
	_, err := mgr.Sign("ssq1punknownaddress", sighash[:])
	if err == nil {
		t.Error("Sign with unknown address should fail")
	}
}

// Signing is hedged: two signatures of one digest differ and both verify.
// The txid does not depend on the witness, so this changes no transaction id.
func TestSigningIsHedged(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager("/dev/null", "test-passwd")
	mgr.keys = []KeyPair{*kp}
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i)
	}
	sig1, err := mgr.Sign(kp.Address, digest)
	if err != nil {
		t.Fatal(err)
	}
	sig2, err := mgr.Sign(kp.Address, digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(sig1) == string(sig2) {
		t.Fatal("two signatures of one digest are identical; signing is not hedged")
	}
	for i, sig := range [][]byte{sig1, sig2} {
		if ok, err := Verify(kp.PublicKey, digest, sig); err != nil || !ok {
			t.Fatalf("signature %d does not verify: %v %v", i, ok, err)
		}
	}
}
