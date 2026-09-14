package keys

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The attack: a signer started with a mistyped keystore path. Before this,
// Load returned an empty manager and the process went on to hand out deposit
// addresses it could never spend from.
func TestLoadRefusesMissingKeystore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.enc")
	m := NewManager(path, "pw")
	err := m.Load()
	if !errors.Is(err, ErrKeystoreMissing) {
		t.Fatalf("Load on a missing file: %v, want ErrKeystoreMissing", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the path: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("Load created the file")
	}
	if m.KeyCount() != 0 {
		t.Fatal("manager holds keys after a refused Load")
	}
}

// LoadOrCreate is the first run: it writes an empty encrypted keystore, so
// the path exists from then on and every later Load succeeds.
func TestLoadOrCreateWritesAnEmptyKeystoreOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "keys.enc")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, "pw")
	if err := m.LoadOrCreate(); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("keystore not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("keystore mode %o, want 600", perm)
	}
	// A plain Load on the created file works, with the right passphrase only.
	if err := NewManager(path, "pw").Load(); err != nil {
		t.Fatalf("Load after LoadOrCreate: %v", err)
	}
	if err := NewManager(path, "other").Load(); err == nil {
		t.Fatal("Load with the wrong passphrase accepted the empty keystore")
	}
	// A second LoadOrCreate on an existing file loads it and does not rewrite it.
	kp, err := GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(path, "pw")
	if err := m2.LoadOrCreate(); err != nil {
		t.Fatalf("LoadOrCreate on an existing keystore: %v", err)
	}
	if m2.KeyCount() != 1 {
		t.Fatalf("LoadOrCreate on an existing keystore loaded %d keys, want 1", m2.KeyCount())
	}
}

// Load on a manager already holding keys would replace them silently; a
// sweep that imported a derived key and then reloaded the hot wallet would
// lose the derived key without a word.
func TestLoadRefusesToDiscardHeldKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	m := NewManager(path, "pw")
	if err := m.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	kp, err := GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(); !errors.Is(err, ErrKeysHeld) {
		t.Fatalf("Load with a key held: %v, want ErrKeysHeld", err)
	}
	if err := m.LoadOrCreate(); !errors.Is(err, ErrKeysHeld) {
		t.Fatalf("LoadOrCreate with a key held: %v, want ErrKeysHeld", err)
	}
	if !m.HasKey(kp.Address) {
		t.Fatal("the held key is gone")
	}
}

// Key material leaves the manager only through ExportPrivateKey, and only as
// a copy: a caller that zeroes or corrupts what it received has not touched
// the manager's key.
func TestExportPrivateKeyAndPublicKeyForReturnCopies(t *testing.T) {
	kp, err := GenerateKeyForNetwork("sq")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager("", "pw")
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	exported, err := m.ExportPrivateKey(kp.Address)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exported, kp.PrivateKey) {
		t.Fatal("exported key differs from the imported one")
	}
	for i := range exported {
		exported[i] = 0xFF
	}
	pub, err := m.PublicKeyFor(kp.Address)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pub {
		pub[i] = 0xFF
	}
	digest := sha256.Sum256([]byte("after the caller corrupted its copies"))
	sig, err := m.Sign(kp.Address, digest[:])
	if err != nil {
		t.Fatalf("Sign after the caller corrupted its copies: %v", err)
	}
	pubAgain, err := m.PublicKeyFor(kp.Address)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pubAgain, kp.PublicKey) {
		t.Fatal("PublicKeyFor handed out the manager's own slice; the record is corrupted")
	}
	if ok, err := Verify(pubAgain, digest[:], sig); err != nil || !ok {
		t.Fatalf("signature does not verify: ok=%v err=%v", ok, err)
	}
	if _, err := m.ExportPrivateKey("sq1punknown"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("export of an unknown address: %v, want ErrNoKey", err)
	}
	if m.HasKey("sq1punknown") {
		t.Fatal("HasKey reports a key the manager does not hold")
	}
}

// A KeyPair that reaches a log line through any fmt verb must not carry the
// private key with it.
func TestKeyPairPrintingRedactsThePrivateKey(t *testing.T) {
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	// Any 8-byte run of the private key is enough to recognise a leak in hex
	// or in Go's byte-slice rendering.
	hexRun := fmt.Sprintf("%x", kp.PrivateKey[100:108])
	decRun := fmt.Sprintf("%d %d %d %d", kp.PrivateKey[100], kp.PrivateKey[101], kp.PrivateKey[102], kp.PrivateKey[103])
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []interface{}{*kp, kp} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, hexRun) || strings.Contains(out, decRun) {
				t.Errorf("%s prints private key material: %.120s", verb, out)
			}
			if !strings.Contains(out, kp.Address) {
				t.Errorf("%s does not name the address: %.120s", verb, out)
			}
		}
	}
}
