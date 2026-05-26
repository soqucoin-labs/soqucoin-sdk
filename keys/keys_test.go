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
	mgr.loaded = true

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
	mgr.loaded = true

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
	mgr.loaded = true

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
	mgr1.loaded = true
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

func TestKeystoreWrongPassword(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := filepath.Join(tmpDir, "test-keystore.enc")

	kp, _ := GenerateKey()

	// Save with correct password
	mgr1 := NewManager(keyFile, "correct-password")
	mgr1.loaded = true
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
	mgr.loaded = true

	sighash := sha256.Sum256([]byte("test"))
	_, err := mgr.Sign("ssq1punknownaddress", sighash[:])
	if err == nil {
		t.Error("Sign with unknown address should fail")
	}
}
