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
	// %d and %x are the verbs a Stringer alone does not cover: on a struct
	// they walk the fields and print the private key byte by byte.
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q", "%08d"} {
		wrapped := struct{ K KeyPair }{*kp}
		for _, v := range []interface{}{*kp, kp, wrapped, []KeyPair{*kp}} {
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

// The attack, and the reason this exists: a signer pointed at a populated
// keystore that it never opened, saving. Before this, the file's keys were
// replaced by the manager's, Save returned nil, and nothing anywhere said the
// keys were gone.
func TestSaveRefusesAKeystoreItHasNotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	writer := NewManager(path, "pw")
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	if err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		manager func(t *testing.T) *Manager
	}{
		{"holding nothing", func(t *testing.T) *Manager {
			return NewManager(path, "pw")
		}},
		{"holding an imported key", func(t *testing.T) *Manager {
			m := NewManager(path, "pw")
			other, err := GenerateKeyForNetwork("ssq")
			if err != nil {
				t.Fatal(err)
			}
			if err := m.ImportPrivateKey(other.PrivateKey, other.PublicKey, other.Address); err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"after a load that failed on the passphrase", func(t *testing.T) *Manager {
			m := NewManager(path, "not the passphrase")
			if err := m.Load(); err == nil {
				t.Fatal("Load with a wrong passphrase succeeded")
			}
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.manager(t)
			err := m.Save()
			if !errors.Is(err, ErrKeystoreUnread) {
				t.Fatalf("Save: %v, want ErrKeystoreUnread", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the error does not name the path: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the refused Save changed the keystore")
			}
			reader := NewManager(path, "pw")
			if err := reader.Load(); err != nil {
				t.Fatalf("Load after the refused Save: %v", err)
			}
			if !reader.HasKey(kp.Address) {
				t.Error("the original key is gone")
			}
		})
	}
}

// The three legitimate ways to write a keystore stay silent. Each of these
// would be a regression an integrator hits on their first run.
func TestSaveAllowsTheWaysAKeystoreIsMeantToBeWritten(t *testing.T) {
	t.Run("a first run on a path that does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		m := NewManager(path, "pw")
		kp, err := GenerateKeyForNetwork("ssq")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		if err := m.Save(); err != nil {
			t.Fatalf("Save on a new path: %v", err)
		}
		if err := m.Save(); err != nil {
			t.Fatalf("second Save by the manager that created it: %v", err)
		}
	})

	// The precondition LoadOrCreate's refusal path depends on: it leaves the
	// manager holding an empty key set, and an empty key set does not trip
	// ErrKeysHeld, so the manager can still read the file afterwards.
	t.Run("a manager holding an empty key set can still load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		m := NewManager(path, "pw")
		if err := m.LoadOrCreate(); err != nil {
			t.Fatal(err)
		}
		if m.KeyCount() != 0 {
			t.Fatalf("manager holds %d keys after creating an empty keystore", m.KeyCount())
		}
		if err := m.Load(); err != nil {
			t.Fatalf("Load on a manager holding an empty key set: %v", err)
		}
	})

	t.Run("LoadOrCreate then save", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		m := NewManager(path, "pw")
		if err := m.LoadOrCreate(); err != nil {
			t.Fatal(err)
		}
		if err := m.Save(); err != nil {
			t.Fatalf("Save after LoadOrCreate created the file: %v", err)
		}
	})

	t.Run("load then save", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		first := NewManager(path, "pw")
		if err := first.LoadOrCreate(); err != nil {
			t.Fatal(err)
		}
		second := NewManager(path, "pw")
		if err := second.Load(); err != nil {
			t.Fatal(err)
		}
		kp, err := GenerateKeyForNetwork("ssq")
		if err != nil {
			t.Fatal(err)
		}
		if err := second.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		if err := second.Save(); err != nil {
			t.Fatalf("Save after Load: %v", err)
		}
		third := NewManager(path, "pw")
		if err := third.Load(); err != nil {
			t.Fatal(err)
		}
		if !third.HasKey(kp.Address) {
			t.Error("the key added after the load is not in the file")
		}
	})

	t.Run("LoadOrCreate on a populated keystore", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.enc")
		first := NewManager(path, "pw")
		kp, err := GenerateKeyForNetwork("ssq")
		if err != nil {
			t.Fatal(err)
		}
		if err := first.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		if err := first.Save(); err != nil {
			t.Fatal(err)
		}
		second := NewManager(path, "pw")
		if err := second.LoadOrCreate(); err != nil {
			t.Fatal(err)
		}
		if err := second.Save(); err != nil {
			t.Fatalf("Save after LoadOrCreate opened an existing file: %v", err)
		}
		if !second.HasKey(kp.Address) {
			t.Error("LoadOrCreate did not bring the existing key back")
		}
	})
}

// The decision LoadOrCreate's creating write would reach if the keystore
// appeared between the moment it looked and the moment it wrote: refuse, and
// let the next run load what appeared.
//
// This asks checkMayOverwrite directly. The window it describes cannot be
// opened from a test without injecting a fault into the file system, so what
// is checked here is the decision, and that the creating write routes through
// it is established by there being exactly one call site, at the top of
// saveLocked, which the create branch returns into. Do not read this test as
// covering that branch.
func TestCheckMayOverwriteRefusesAKeystoreThatAppeared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	m := NewManager(path, "pw")

	// What the create path does, with the file present as it would be had
	// another process won the race: the guard is the last thing between the
	// two, so it is asked directly.
	other := NewManager(path, "pw")
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	if err := m.checkMayOverwrite(); !errors.Is(err, ErrKeystoreUnread) {
		t.Fatalf("checkMayOverwrite: %v, want ErrKeystoreUnread", err)
	}
	if err := NewManager(path, "pw").LoadOrCreate(); err != nil {
		t.Fatalf("the next run does not load what appeared: %v", err)
	}
}

// A path that cannot be examined is not a path this can call empty, so it is
// refused rather than written.
func TestSaveRefusesAPathItCannotExamine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unreadable")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sub", "keys.enc")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		// Root traverses a directory whose mode forbids it and stats through
		// it, so there is no unexaminable path to construct here.
		t.Skip("running as root: permissions do not apply")
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skip("cannot drop directory permissions here")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := NewManager(path, "pw").Save()
	if err == nil {
		t.Fatal("Save into a directory that cannot be examined succeeded")
	}
	if errors.Is(err, ErrKeystoreUnread) {
		t.Fatalf("a stat failure is reported as an overwrite refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "stat keystore") {
		t.Fatalf("Save failed for some other reason than the stat: %v", err)
	}
}
