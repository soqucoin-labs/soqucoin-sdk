package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The version 1 fixture in testdata was written by the v0.3.6.1 keystore
// writer (keys/ is unchanged between v0.3.6.1 and the commit this branch
// starts from, so the writer is that release's). Its two keys come from
// FromSeed over the SHA-256 of the labels below, so a test can derive what
// the file must contain instead of trusting a string copied out of it.
const (
	v1FixturePath       = "testdata/keystore-v1.json"
	v1FixturePassphrase = "fixture passphrase, not a secret"
)

var v1FixtureLabels = []string{
	"soqucoin-sdk keystore v1 fixture 0",
	"soqucoin-sdk keystore v1 fixture 1",
}

// The addresses as v0.3.6 produced them, recorded here so that a change to
// the address encoder is caught as well as a change to the keystore reader.
var v1FixtureAddresses = []string{
	"ssq1pywcukla40dpa25hr5p57plnl0kvq7ttc7udekfn73va36m95mp7qyhkk96",
	"ssq1p93xv4affdcmuwddzpc6gnf38tpq5mc29hul5cjl3fczspm9qwn0s4eyqqy",
}

func testExternalKey(t *testing.T, label string) []byte {
	t.Helper()
	k := sha256.Sum256([]byte(label))
	return k[:]
}

// newManagerWithKey is NewManagerWithKey with the error checked, for the
// tests whose subject is something else.
func newManagerWithKey(t *testing.T, path string, key []byte) *Manager {
	t.Helper()
	m, err := NewManagerWithKey(path, key)
	if err != nil {
		t.Fatalf("NewManagerWithKey: %v", err)
	}
	return m
}

// saveOneKey writes a keystore holding one freshly generated key and returns
// the address and the public key.
func saveOneKey(t *testing.T, m *Manager) (string, []byte) {
	t.Helper()
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatalf("GenerateKeyForNetwork: %v", err)
	}
	pub := bytes.Clone(kp.PublicKey)
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatalf("ImportPrivateKey: %v", err)
	}
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return kp.Address, pub
}

func readKeystore(t *testing.T, path string) Keystore {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	var ks Keystore
	if err := json.Unmarshal(data, &ks); err != nil {
		t.Fatalf("parse keystore: %v", err)
	}
	return ks
}

// A key held in a key-management system opens the keystore it wrote, and the
// key it holds still signs.
func TestExternalKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	key := testExternalKey(t, "vault key")

	addr, pub := saveOneKey(t, newManagerWithKey(t, path, key))

	ks := readKeystore(t, path)
	if ks.Version != keystoreVersion {
		t.Errorf("version = %d, want %d", ks.Version, keystoreVersion)
	}
	if ks.KDF != KDFHKDFSHA256 {
		t.Errorf("kdf = %q, want %q", ks.KDF, KDFHKDFSHA256)
	}
	if ks.KDFParams != nil {
		t.Errorf("kdfparams = %+v, want none for an external key", ks.KDFParams)
	}

	loaded := newManagerWithKey(t, path, key)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	digest := sha256.Sum256([]byte("external key round trip"))
	sig, err := loaded.Sign(addr, digest[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if ok, err := Verify(pub, digest[:], sig); err != nil || !ok {
		t.Fatalf("signature from the reloaded key does not verify: %v %v", ok, err)
	}
}

// The attack: an operator's key-management system hands the signer the wrong
// key, or an attacker with a key of their own points the signer at a keystore
// they wrote. The load must fail and leave nothing behind: a manager that
// came back half-populated would serve deposit addresses it cannot sign for.
func TestWrongExternalKeyFailsClosedWithNoPartialState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	addr, _ := saveOneKey(t, newManagerWithKey(t, path, testExternalKey(t, "right key")))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	m := newManagerWithKey(t, path, testExternalKey(t, "wrong key"))
	if err := m.Load(); err == nil {
		t.Fatal("Load with the wrong external key succeeded")
	}
	if n := m.KeyCount(); n != 0 {
		t.Errorf("manager holds %d keys after a refused Load, want 0", n)
	}
	if got := m.GetAddresses(); len(got) != 0 {
		t.Errorf("manager reports addresses after a refused Load: %v", got)
	}
	if m.HasKey(addr) {
		t.Error("manager reports a key for the address after a refused Load")
	}
	digest := sha256.Sum256([]byte("after a refused load"))
	if _, err := m.Sign(addr, digest[:]); !errors.Is(err, ErrNoKey) {
		t.Errorf("Sign after a refused Load: %v, want ErrNoKey", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused Load changed the keystore file")
	}
}

// A failure leaves the manager able to try again: nothing about the refused
// attempt is latched. The file is repaired between the two calls, which is
// the shape of a load that failed on a half-written file restored from
// backup, and the same manager loads it.
func TestRefusedLoadLeavesTheManagerReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	key := testExternalKey(t, "vault key")
	addr, _ := saveOneKey(t, newManagerWithKey(t, path, key))
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, good[:len(good)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	m := newManagerWithKey(t, path, key)
	if err := m.Load(); err == nil {
		t.Fatal("Load of a truncated keystore succeeded")
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(); err != nil {
		t.Fatalf("Load after the file was repaired: %v", err)
	}
	if !m.HasKey(addr) {
		t.Error("the repaired Load did not bring the key back")
	}
}

// A key of the wrong length is refused at construction, not at the first
// Load: a caller that ignores the error would otherwise hold a manager that
// fails much later, on a path where the reason is no longer visible.
func TestExternalKeySizeRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		key := make([]byte, n)
		m, err := NewManagerWithKey(path, key)
		if !errors.Is(err, ErrExternalKeySize) {
			t.Errorf("NewManagerWithKey with a %d-byte key: %v, want ErrExternalKeySize", n, err)
		}
		if m != nil {
			t.Errorf("NewManagerWithKey with a %d-byte key returned a manager", n)
		}
	}
	if _, err := NewManagerWithKey(path, nil); !errors.Is(err, ErrExternalKeySize) {
		t.Errorf("NewManagerWithKey with a nil key: %v, want ErrExternalKeySize", err)
	}
}

// The manager keeps its own copy of the external key, so a caller that zeroes
// the buffer it unsealed from Vault does not silently change the encryption
// key of every later Save.
func TestExternalKeySurvivesCallerZeroing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	key := testExternalKey(t, "vault key")
	caller := bytes.Clone(key)

	m := newManagerWithKey(t, path, caller)
	for i := range caller {
		caller[i] = 0
	}
	addr, _ := saveOneKey(t, m)

	loaded := newManagerWithKey(t, path, key)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load with the original key: %v", err)
	}
	if !loaded.HasKey(addr) {
		t.Error("the reloaded manager does not hold the key")
	}
}

// A keystore says which key source wrote it, so opening it with the other one
// names the mistake instead of reporting a decryption failure that sends an
// operator looking for a wrong passphrase.
func TestKeySourceMismatchIsNamed(t *testing.T) {
	dir := t.TempDir()
	key := testExternalKey(t, "vault key")

	passFile := filepath.Join(dir, "passphrase.enc")
	saveOneKey(t, NewManager(passFile, "pw"))
	if err := newManagerWithKey(t, passFile, key).Load(); !errors.Is(err, ErrKDFMismatch) {
		t.Errorf("external-key manager on a passphrase keystore: %v, want ErrKDFMismatch", err)
	}

	extFile := filepath.Join(dir, "external.enc")
	saveOneKey(t, newManagerWithKey(t, extFile, key))
	if err := NewManager(extFile, "pw").Load(); !errors.Is(err, ErrKDFMismatch) {
		t.Errorf("passphrase manager on an external-key keystore: %v, want ErrKDFMismatch", err)
	}
}

// Version 1 is passphrase-only, so a manager holding an external key is told
// that rather than left to fail on the decryption.
func TestV1KeystoreRefusedByAnExternalKeyManager(t *testing.T) {
	path := copyFixture(t, v1FixturePath)
	if err := newManagerWithKey(t, path, testExternalKey(t, "vault key")).Load(); !errors.Is(err, ErrKDFMismatch) {
		t.Errorf("external-key manager on a version 1 keystore: %v, want ErrKDFMismatch", err)
	}
}

// Parameters outside the range this build accepts are refused by name, and
// before Argon2id is asked to run with them.
//
// The two ends are there for different reasons. The ceiling is a guard against
// a hostile file: a header claiming 64 GiB would otherwise have the process
// allocate it. The floor is not a guard against an edit at all, because the
// parameters feed the derivation and an edited file opens for nobody; it
// catches a file that was legitimately written weak, by an older writer or
// another implementation, which would open without a word while its at-rest
// protection was worth less than its holder believed.
func TestKDFParamsOutsideTheRangeAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(p map[string]any)
		wants error
	}{
		{"no passes", func(p map[string]any) { p["t"] = 1 }, ErrKDFParams},
		{"too many passes", func(p map[string]any) { p["t"] = maxTime + 1 }, ErrKDFParams},
		{"memory below the floor", func(p map[string]any) { p["m"] = 8 * 1024 }, ErrKDFParams},
		{"memory above the ceiling", func(p map[string]any) { p["m"] = maxMemoryKiB + 1 }, ErrKDFParams},
		{"no lanes", func(p map[string]any) { p["p"] = 0 }, ErrKDFParams},
		{"too many lanes", func(p map[string]any) { p["p"] = maxThreads + 1 }, ErrKDFParams},
		{"a 128-bit key", func(p map[string]any) { p["keylen"] = 16 }, ErrKDFParams},
		{"no parameters at all", func(p map[string]any) { clear(p) }, ErrKDFParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.enc")
			saveOneKey(t, NewManager(path, "pw"))
			editHeader(t, path, func(h map[string]any) {
				tc.edit(h["kdfparams"].(map[string]any))
			})
			m := NewManager(path, "pw")
			err := m.Load()
			if !errors.Is(err, tc.wants) {
				t.Fatalf("Load: %v, want %v", err, tc.wants)
			}
			if m.KeyCount() != 0 {
				t.Error("manager holds keys after a refused Load")
			}
		})
	}
}

// Every field of the header is refused when it is edited, and each case
// declares which guard does the refusing, because they are not the same
// strength. Editing the salt or the parameters changes the derived key;
// editing the nonce or the ciphertext changes what the AEAD is given; the
// version and an unknown KDF are read by the header check; naming the other
// key source is caught by the comparison in deriveKey; and the public key list
// is compared against the decrypted records, which is the guard that also
// covers version 1.
//
// The declaration is checked rather than trusted. refusalMechanism re-runs
// load's questions in load's order and returns the guard that fires first, and
// the declared field list is compared against the edit that was actually made,
// so a case can neither claim a mechanism it does not exercise nor claim a
// field it leaves alone.
//
// Most fields are covered twice over, and on a version 2 file the binding
// usually fires first. That is worth keeping and worth stating plainly: the
// binding makes "the header is authenticated" one property of the format
// rather than a conclusion drawn from five separate guards all staying in
// place. TestSwappedPublicKeyListIsRefused is where the list check is the
// guard on its own, on version 1, which binds nothing.
const (
	byParse       = "the parser"
	byHeaderCheck = "the header check"
	byKeySource   = "the key source comparison"
	byDerivedKey  = "a different derived key"
	byMessage     = "a different AEAD input"
	byBinding     = "the bound header"
	byListCheck   = "the public key list check"
)

func TestEveryHeaderFieldIsTamperEvident(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range []struct {
		fields []string
		name   string
		by     string
		edit   func(h map[string]any)
		wants  error
	}{
		{[]string{"version"}, "an unknown version", byHeaderCheck,
			func(h map[string]any) { h["version"] = 3 }, ErrKeystoreVersion},
		{[]string{"version", "kdfparams"}, "claimed to be version 1", byBinding, func(h map[string]any) {
			h["version"] = 1
			delete(h, "kdfparams")
		}, nil},
		{[]string{"kdf", "kdfparams"}, "another key source", byKeySource, func(h map[string]any) {
			h["kdf"] = KDFHKDFSHA256
			delete(h, "kdfparams")
		}, ErrKDFMismatch},
		{[]string{"kdf"}, "an unknown kdf", byHeaderCheck,
			func(h map[string]any) { h["kdf"] = "pbkdf2" }, ErrKeystoreHeader},
		{[]string{"kdfparams"}, "parameters changed within the accepted range", byDerivedKey,
			func(h map[string]any) { h["kdfparams"].(map[string]any)["t"] = maxTime }, nil},
		{[]string{"salt"}, "another salt", byDerivedKey,
			func(h map[string]any) { h["salt"] = flipFirstByte(t, h["salt"]) }, nil},
		{[]string{"salt"}, "a short salt", byHeaderCheck,
			func(h map[string]any) { h["salt"] = "" }, ErrKeystoreHeader},
		{[]string{"nonce"}, "another nonce", byMessage,
			func(h map[string]any) { h["nonce"] = flipFirstByte(t, h["nonce"]) }, nil},
		{[]string{"nonce"}, "a short nonce", byHeaderCheck,
			func(h map[string]any) { h["nonce"] = "" }, ErrKeystoreHeader},
		{[]string{"ciphertext"}, "an edited ciphertext", byMessage,
			func(h map[string]any) { h["ciphertext"] = flipFirstByte(t, h["ciphertext"]) }, nil},
		{[]string{"pubkeys"}, "the operator's copy of the address swapped", byBinding,
			func(h map[string]any) {
				h["pubkeys"].([]any)[0].(map[string]any)["address"] = v1FixtureAddresses[0]
			}, nil},
	} {
		for _, f := range tc.fields {
			covered[f] = true
		}
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.enc")
			m := NewManager(path, "pw")
			saveOneKey(t, m)
			before := readKeystore(t, path)

			editHeader(t, path, tc.edit)
			if got := changedFields(t, path, before); !reflect.DeepEqual(got, sorted(tc.fields)) {
				t.Fatalf("the edit changed %v, the case declares %v", got, sorted(tc.fields))
			}
			if got := refusalMechanism(t, path, before, m); got != tc.by {
				t.Errorf("refused by %s, the case declares %s", got, tc.by)
			}

			loader := NewManager(path, "pw")
			err := loader.Load()
			if err == nil {
				t.Fatalf("Load accepted an edited keystore (expected refusal by %s)", tc.by)
			}
			if tc.wants != nil && !errors.Is(err, tc.wants) {
				t.Fatalf("Load: %v, want %v", err, tc.wants)
			}
			if loader.KeyCount() != 0 {
				t.Error("manager holds keys after a refused Load")
			}
		})
	}

	for i := range reflect.TypeFor[Keystore]().NumField() {
		tag := jsonName(reflect.TypeFor[Keystore]().Field(i))
		if !covered[tag] {
			t.Errorf("header field %q has no tamper case in this test", tag)
		}
	}
}

// refusalMechanism works out which guard refuses the file at path, by running
// the same questions load runs, in the order load runs them, against the file
// before and after the edit. It is derived rather than declared: a case in the
// table above cannot claim a mechanism that is not the one doing the work, and
// a case nothing refuses is reported as such instead of passing because some
// other guard happened to catch it.
func refusalMechanism(t *testing.T, path string, before Keystore, m *Manager) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	after, err := strictDecode(data)
	if err != nil {
		return byParse
	}
	aad, err := aadFor(after)
	if err != nil {
		t.Fatal(err)
	}
	wasAAD, err := aadFor(before)
	if err != nil {
		t.Fatal(err)
	}

	normalise := func(ks Keystore) *Keystore {
		if ks.Version == keystoreVersionV1 {
			if err := upgradeV1(&ks); err != nil {
				return nil
			}
		}
		return &ks
	}
	edited := normalise(after)
	if edited == nil {
		return byHeaderCheck
	}
	if err := checkHeader(edited); err != nil {
		return byHeaderCheck
	}

	editedKey, err := m.deriveKey(edited)
	if errors.Is(err, ErrKDFMismatch) {
		return byKeySource
	}
	if err != nil {
		return byHeaderCheck
	}
	originalKey, err := m.deriveKey(normalise(before))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(editedKey, originalKey) {
		return byDerivedKey
	}
	if !bytes.Equal(after.Nonce, before.Nonce) || !bytes.Equal(after.Ciphertext, before.Ciphertext) {
		return byMessage
	}
	if !bytes.Equal(aad, wasAAD) {
		return byBinding
	}
	return byListCheck
}

// changedFields is the sorted set of top-level header fields whose value
// differs from before, so a case's declared field list is checked against the
// edit it actually performed.
func changedFields(t *testing.T, path string, before Keystore) []string {
	t.Helper()
	asMap := func(v any) map[string]any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	was := asMap(before)
	names := map[string]bool{}
	for k := range was {
		names[k] = true
	}
	for k := range after {
		names[k] = true
	}
	var changed []string
	for k := range names {
		a, _ := json.Marshal(was[k])
		b, _ := json.Marshal(after[k])
		if !bytes.Equal(a, b) {
			changed = append(changed, k)
		}
	}
	return sorted(changed)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}

// Reformatting the file changes no value, so it must still open: the bound
// data is the header's values in a canonical encoding, not the bytes on disk,
// and an operator whose tooling rewrites the JSON has not destroyed the
// keystore.
func TestReformattingTheFileKeepsItReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	addr, _ := saveOneKey(t, NewManager(path, "pw"))
	editHeader(t, path, func(h map[string]any) {})

	m := NewManager(path, "pw")
	if err := m.Load(); err != nil {
		t.Fatalf("Load of a reformatted keystore: %v", err)
	}
	if !m.HasKey(addr) {
		t.Error("the reformatted keystore did not yield the key")
	}
}

// The ciphertext value is the one thing not repeated in the bound data: the
// AEAD already covers it as the message, and repeating it would double the
// file. Which fields are bound, and in what encoding, is pinned by
// TestHeaderAADIsFrozenForVersion2 against exact bytes; this is the one
// property of headerAAD that a golden vector states less clearly than an
// assertion.
func TestTheCiphertextIsNotBoundAsWellAsSealed(t *testing.T) {
	ciphertext := bytes.Repeat([]byte{3}, 64)
	aad, err := headerAAD(Keystore{
		Version:    keystoreVersion,
		KDF:        KDFArgon2id,
		KDFParams:  &KDFParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32},
		Salt:       bytes.Repeat([]byte{1}, saltSize),
		Nonce:      bytes.Repeat([]byte{2}, nonceSize),
		Ciphertext: ciphertext,
		PubKeys:    []KeyPair{{PublicKey: bytes.Repeat([]byte{4}, PublicKeySize), Address: v1FixtureAddresses[0]}},
	})
	if err != nil {
		t.Fatalf("headerAAD: %v", err)
	}
	ct, err := json.Marshal(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(aad, ct) {
		t.Error("the ciphertext is in the bound data as well as in the message")
	}
}

// Whatever this package writes it must be able to read back: the header check
// runs on the way out as well as on the way in, so a shape that Save could
// produce and Load would refuse cannot reach disk.
func TestSaveWritesAHeaderItWouldAccept(t *testing.T) {
	dir := t.TempDir()
	for name, m := range map[string]*Manager{
		"passphrase":   NewManager(filepath.Join(dir, "pass.enc"), "pw"),
		"external key": newManagerWithKey(t, filepath.Join(dir, "ext.enc"), testExternalKey(t, "vault key")),
	} {
		t.Run(name, func(t *testing.T) {
			addr, _ := saveOneKey(t, m)
			ks := readKeystore(t, m.keyFile)
			if err := checkHeader(&ks); err != nil {
				t.Fatalf("Save wrote a header the header check refuses: %v", err)
			}
			// The header check is run by Save on the same values, so the
			// assertion above cannot fire on its own. Reading the file back
			// through a fresh manager is what makes the claim in the name
			// true: it exercises the parser, the unknown-field refusal, the
			// derivation and the AEAD against bytes that went to disk.
			reader := readerFor(t, name, m.keyFile)
			if err := reader.Load(); err != nil {
				t.Fatalf("Save wrote a keystore Load refuses: %v", err)
			}
			if !reader.HasKey(addr) {
				t.Error("the reloaded keystore does not hold the key that was saved")
			}
		})
	}
}

// With an external key that never changes, the salt and the nonce are what
// stand between two saves and a repeated AES key and nonce pair. Both are
// drawn fresh on every Save.
func TestEverySaveDrawsAFreshSaltAndNonce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	m := newManagerWithKey(t, path, testExternalKey(t, "vault key"))
	saveOneKey(t, m)
	first := readKeystore(t, path)
	if err := m.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	second := readKeystore(t, path)
	if bytes.Equal(first.Salt, second.Salt) {
		t.Error("two saves used the same salt")
	}
	if bytes.Equal(first.Nonce, second.Nonce) {
		t.Error("two saves used the same nonce")
	}
}

// The external key is not used as the AES key. It goes through HKDF with the
// file's salt, so the AES key differs on every Save even though the key in
// the key-management system never changes, and the nonce is not left as the
// only thing that must never repeat. Derivation is checked directly because
// nothing outside this package can see the key it produces.
func TestExternalKeyDerivationIsSaltBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	m := newManagerWithKey(t, path, testExternalKey(t, "vault key"))
	other := newManagerWithKey(t, path, testExternalKey(t, "another vault key"))

	header := func(salt byte) *Keystore {
		return &Keystore{KDF: KDFHKDFSHA256, Salt: bytes.Repeat([]byte{salt}, saltSize)}
	}
	first, err := m.deriveKey(header(1))
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	second, err := m.deriveKey(header(2))
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	sameSalt, err := other.deriveKey(header(1))
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	if len(first) != encKeySize {
		t.Fatalf("derived key is %d bytes, want %d", len(first), encKeySize)
	}
	if bytes.Equal(first, second) {
		t.Error("two salts derive the same key; the salt is not in the derivation")
	}
	if bytes.Equal(first, sameSalt) {
		t.Error("two external keys derive the same key")
	}
	if bytes.Contains(first, testExternalKey(t, "vault key")) {
		t.Error("the external key is the AES key")
	}
}

// A version 1 keystore written by v0.3.6.1 opens under the new code and
// yields the same keys and the same addresses. The expected keys are derived
// here from the seeds the fixture was built from, so this compares the
// keystore reader against FIPS 204 key generation rather than against a
// string copied out of the file.
func TestV1KeystoreYieldsTheSameKeysAndAddresses(t *testing.T) {
	on := readKeystore(t, v1FixturePath)
	if on.Version != keystoreVersionV1 {
		t.Fatalf("fixture version = %d, want %d", on.Version, keystoreVersionV1)
	}
	if on.KDFParams != nil {
		t.Fatal("fixture carries KDF parameters; version 1 did not")
	}

	m := NewManager(v1FixturePath, v1FixturePassphrase)
	if err := m.Load(); err != nil {
		t.Fatalf("Load of the version 1 fixture: %v", err)
	}
	if got := m.GetAddresses(); !reflect.DeepEqual(got, v1FixtureAddresses) {
		t.Fatalf("addresses = %v, want %v", got, v1FixtureAddresses)
	}
	for i, label := range v1FixtureLabels {
		want, err := FromSeed("ssq", sha256.Sum256([]byte(label)))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if want.Address != v1FixtureAddresses[i] {
			t.Fatalf("seed %d derives %s, want %s", i, want.Address, v1FixtureAddresses[i])
		}
		got, err := m.ExportPrivateKey(want.Address)
		if err != nil {
			t.Fatalf("ExportPrivateKey %s: %v", want.Address, err)
		}
		if !bytes.Equal(got, want.PrivateKey) {
			t.Errorf("key %d from the version 1 keystore is not the key the seed derives", i)
		}
		pub, err := m.PublicKeyFor(want.Address)
		if err != nil {
			t.Fatalf("PublicKeyFor %s: %v", want.Address, err)
		}
		if !bytes.Equal(pub, want.PublicKey) {
			t.Errorf("public key %d from the version 1 keystore is not the one the seed derives", i)
		}
	}
}

// Reading a version 1 file does not rewrite it; the next Save does, in
// version 2, under the same passphrase, and the keys come back unchanged.
func TestV1KeystoreIsRewrittenAsV2OnTheNextSave(t *testing.T) {
	path := copyFixture(t, v1FixturePath)

	m := NewManager(path, v1FixturePassphrase)
	if err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v := readKeystore(t, path).Version; v != keystoreVersionV1 {
		t.Fatalf("Load rewrote the file: version = %d, want %d", v, keystoreVersionV1)
	}
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ks := readKeystore(t, path)
	if ks.Version != keystoreVersion {
		t.Errorf("version after Save = %d, want %d", ks.Version, keystoreVersion)
	}
	if ks.KDF != KDFArgon2id {
		t.Errorf("kdf after Save = %q, want %q", ks.KDF, KDFArgon2id)
	}
	if ks.KDFParams == nil || *ks.KDFParams != defaultParams {
		t.Errorf("parameters after Save = %+v, want %+v", ks.KDFParams, defaultParams)
	}

	reloaded := NewManager(path, v1FixturePassphrase)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load of the rewritten keystore: %v", err)
	}
	if got := reloaded.GetAddresses(); !reflect.DeepEqual(got, v1FixtureAddresses) {
		t.Errorf("addresses after the rewrite = %v, want %v", got, v1FixtureAddresses)
	}
}

// A version 1 file still refuses the wrong passphrase, and the parameters it
// is read with are the ones it was written with: a reader that used today's
// defaults would be right only for as long as the defaults do not move, so
// this fails the moment v1Params is edited to track them.
func TestV1KeystoreRefusesTheWrongPassphrase(t *testing.T) {
	if err := NewManager(v1FixturePath, "not the passphrase").Load(); err == nil {
		t.Fatal("Load of the version 1 fixture with a wrong passphrase succeeded")
	}
	if (v1Params != KDFParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32}) {
		t.Errorf("v1Params = %+v; version 1 files were written with t=3 m=64MiB p=4 keylen=32", v1Params)
	}
}

// A version 1 header is two fields, and both are refused when they are not
// what version 1 wrote: the reader supplies that version's parameters from
// its own constants, so a file that claims others, or claims another KDF, is
// not a version 1 file and is not read as one.
func TestMalformedV1HeaderIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(h map[string]any)
	}{
		{"parameters version 1 never carried", func(h map[string]any) {
			h["kdfparams"] = map[string]any{"t": 3, "m": 64 * 1024, "p": 4, "keylen": 32}
		}},
		{"a kdf version 1 never used", func(h map[string]any) { h["kdf"] = KDFHKDFSHA256 }},
		{"no kdf at all", func(h map[string]any) { delete(h, "kdf") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := copyFixture(t, v1FixturePath)
			editHeader(t, path, tc.edit)
			m := NewManager(path, v1FixturePassphrase)
			if err := m.Load(); !errors.Is(err, ErrKeystoreHeader) {
				t.Fatalf("Load: %v, want ErrKeystoreHeader", err)
			}
			if m.KeyCount() != 0 {
				t.Error("manager holds keys after a refused Load")
			}
		})
	}
}

// copyFixture puts a writable copy of a committed fixture in a temporary
// directory, so a test that saves over it does not edit the committed file.
func copyFixture(t *testing.T, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(fixture))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return path
}

// editHeader rewrites the keystore file through a generic JSON map, so a test
// can change one field the way an attacker with write access would, and so
// that the rewrite itself reformats the file.
func editHeader(t *testing.T, path string, edit func(h map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	var h map[string]any
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatalf("parse keystore: %v", err)
	}
	edit(h)
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("serialize keystore: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write keystore: %v", err)
	}
}

// flipFirstByte returns a base64 JSON value with its first decoded byte
// changed: the smallest edit to a byte-slice header field.
func flipFirstByte(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field is %T, want a base64 string", v)
	}
	var b []byte
	if err := json.Unmarshal([]byte(`"`+s+`"`), &b); err != nil {
		t.Fatalf("decode field: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("field is empty")
	}
	b[0] ^= 0xFF
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(string(out), `"`)
}

// jsonName is the name a struct field is written under in the keystore file.
func jsonName(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}

// The bound data is a function of the Keystore struct's definition, so the
// version 2 field set is frozen: adding a field changes the canonical
// encoding and every version 2 keystore already written stops opening under
// the new build. This test fails on any change to the struct, so that
// decision is taken at the edit rather than discovered by an integrator whose
// keys have become unreadable. A new header field means a new format version.
func TestKeystoreFieldSetIsFrozen(t *testing.T) {
	want := []struct{ name, tag, typ string }{
		{"Version", "version", "int"},
		{"KDF", "kdf", "string"},
		{"KDFParams", "kdfparams,omitempty", "*keys.KDFParams"},
		{"Salt", "salt", "[]uint8"},
		{"Nonce", "nonce", "[]uint8"},
		{"Ciphertext", "ciphertext", "[]uint8"},
		{"PubKeys", "pubkeys", "[]keys.KeyPair"},
	}
	typ := reflect.TypeFor[Keystore]()
	if typ.NumField() != len(want) {
		t.Fatalf("Keystore has %d fields, want %d; see the comment on headerAAD", typ.NumField(), len(want))
	}
	for i, w := range want {
		f := typ.Field(i)
		if f.Name != w.name || f.Tag.Get("json") != w.tag || f.Type.String() != w.typ {
			t.Errorf("field %d is %s %s `json:%q`, want %s %s `json:%q`",
				i, f.Name, f.Type, f.Tag.Get("json"), w.name, w.typ, w.tag)
		}
	}
}

// The exact bytes the AEAD binds for a known version 2 header. A change here
// is a change to the format, whatever produced it: a renamed tag, a reordered
// field, a different encoding of a byte slice. The entry in PubKeys is what
// extends that to KeyPair, whose tags the outer field set does not describe;
// the vector also shows the private key staying out of the bound data, where
// KeyPair's `json:"-"` puts it.
//
// Three tests hold the format together and each catches something the others
// do not: this one the encoding, TestKeystoreFieldSetIsFrozen the outer field
// set, and TestV2KeystoreFromAnEarlierBuildStillOpens a real file written by
// another build.
func TestHeaderAADIsFrozenForVersion2(t *testing.T) {
	ks := Keystore{
		Version:    keystoreVersion,
		KDF:        KDFArgon2id,
		KDFParams:  &KDFParams{Time: 3, Memory: 65536, Threads: 4, KeyLen: 32},
		Salt:       bytes.Repeat([]byte{0xA1}, saltSize),
		Nonce:      bytes.Repeat([]byte{0xB2}, nonceSize),
		Ciphertext: bytes.Repeat([]byte{0xC3}, 8),
		// One entry, so the vector pins KeyPair's own tags and field order as
		// well as the outer ones. A public key of four bytes keeps the vector
		// readable; headerAAD encodes, it does not validate.
		PubKeys: []KeyPair{{
			PrivateKey: bytes.Repeat([]byte{0xD4}, 4),
			PublicKey:  bytes.Repeat([]byte{0xE5}, 4),
			Address:    "ssq1pgoldenvector",
			Index:      7,
		}},
	}
	const want = `{"version":2,"kdf":"argon2id","kdfparams":{"t":3,"m":65536,"p":4,"keylen":32},` +
		`"salt":"oaGhoaGhoaGhoaGhoaGhoaGhoaGhoaGhoaGhoaGhoaE=",` +
		`"nonce":"srKysrKysrKysrKy","ciphertext":null,` +
		`"pubkeys":[{"pubkey":"5eXl5Q==","address":"ssq1pgoldenvector","index":7}]}`
	got, err := headerAAD(ks)
	if err != nil {
		t.Fatalf("headerAAD: %v", err)
	}
	if string(got) != want {
		t.Errorf("version 2 bound data changed.\n got: %s\nwant: %s", got, want)
	}
}

// Content this build does not know is refused rather than dropped, whether it
// sits inside the object or after it. Either way it would be in the file and
// outside everything the header's tamper-evidence covers. The second case is
// its own test and not a variation on the first: a Decoder reads one value and
// ignores whatever follows, so refusing unknown fields does not refuse this.
func TestContentThisBuildDoesNotKnowIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, path string)
	}{
		{"a field inside the object", func(t *testing.T, path string) {
			editHeader(t, path, func(h map[string]any) { h["note"] = "added after the fact" })
		}},
		{"a field inside a public key entry", func(t *testing.T, path string) {
			editHeader(t, path, func(h map[string]any) {
				h["pubkeys"].([]any)[0].(map[string]any)["label"] = "hot wallet"
			})
		}},
		{"another object after it", func(t *testing.T, path string) {
			appendToFile(t, path, []byte(`{"note":"appended"}`))
		}},
		{"bytes after it", func(t *testing.T, path string) {
			appendToFile(t, path, []byte{0x00, 0xFF})
		}},
		{"a member named twice", func(t *testing.T, path string) {
			duplicateMember(t, path, `"pubkeys":`, `"pubkeys": [], `)
		}},
		{"a member of a public key entry named twice", func(t *testing.T, path string) {
			duplicateMember(t, path, `"address":`, `"address": "ssq1pnotthisone", `)
		}},
		{"a KDF parameter named twice", func(t *testing.T, path string) {
			duplicateMember(t, path, `"m":`, `"m": 8, `)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.enc")
			saveOneKey(t, NewManager(path, "pw"))
			tc.break_(t, path)

			m := NewManager(path, "pw")
			if err := m.Load(); err == nil {
				t.Fatal("Load accepted a keystore carrying content this build does not know")
			}
			if m.KeyCount() != 0 {
				t.Error("manager holds keys after a refused Load")
			}
		})
	}
}

// A trailing newline is not content: a file an editor has touched still opens.
func TestTrailingWhitespaceIsNotContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	addr, _ := saveOneKey(t, NewManager(path, "pw"))
	appendToFile(t, path, []byte("\n\n  \t"))

	m := NewManager(path, "pw")
	if err := m.Load(); err != nil {
		t.Fatalf("Load of a keystore with a trailing newline: %v", err)
	}
	if !m.HasKey(addr) {
		t.Error("the keystore did not yield its key")
	}
}

func appendToFile(t *testing.T, path string, extra []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, extra...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The attack this closes for version 1, which bound nothing: the unencrypted
// list is what an operator reads to find the address to send to, so swapping
// an address in it sends a withdrawal to whoever edited the file. Version 2
// refuses the edit at the AEAD; version 1 has only this check, so the test
// runs against both.
func TestSwappedPublicKeyListIsRefused(t *testing.T) {
	other, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path func(t *testing.T) (string, *Manager)
		edit func(h map[string]any)
	}{
		{"version 1, the address swapped", v1Keystore,
			func(h map[string]any) { h["pubkeys"].([]any)[0].(map[string]any)["address"] = other.Address }},
		{"version 1, an entry dropped", v1Keystore,
			func(h map[string]any) { h["pubkeys"] = h["pubkeys"].([]any)[:1] }},
		{"version 1, the index rewritten", v1Keystore,
			func(h map[string]any) { h["pubkeys"].([]any)[1].(map[string]any)["index"] = 7 }},
		{"version 2, the address swapped", v2Keystore,
			func(h map[string]any) { h["pubkeys"].([]any)[0].(map[string]any)["address"] = other.Address }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, m := tc.path(t)
			editHeader(t, path, tc.edit)
			if err := m.Load(); err == nil {
				t.Fatal("Load accepted a keystore whose public key list does not match its keys")
			}
			if m.KeyCount() != 0 {
				t.Error("manager holds keys after a refused Load")
			}
		})
	}
}

// v1Keystore is a writable copy of the committed version 1 fixture and a
// manager for it; v2Keystore is a freshly written version 2 keystore and a
// manager for it. Both return a manager that has not loaded yet.
func v1Keystore(t *testing.T) (string, *Manager) {
	t.Helper()
	path := copyFixture(t, v1FixturePath)
	return path, NewManager(path, v1FixturePassphrase)
}

func v2Keystore(t *testing.T) (string, *Manager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.enc")
	writer := NewManager(path, "pw")
	for _, label := range []string{"list check 0", "list check 1"} {
		seed := sha256.Sum256([]byte(label))
		kp, err := FromSeed("ssq", seed)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Save(); err != nil {
		t.Fatal(err)
	}
	return path, NewManager(path, "pw")
}

// The committed version 2 keystore, written by this release. Every other
// version 2 test saves and loads inside one binary, so the bound data is
// self-consistent by construction and a change to the format would go
// unnoticed. This one is a file from outside the running build, which is the
// only shape that catches it. If this test fails, a keystore an integrator
// already holds has stopped opening.
const (
	v2FixturePath       = "testdata/keystore-v2.json"
	v2FixturePassphrase = "fixture passphrase, not a secret"
)

var v2FixtureLabels = []string{
	"soqucoin-sdk keystore v2 fixture 0",
	"soqucoin-sdk keystore v2 fixture 1",
}

var v2FixtureAddresses = []string{
	"ssq1pdyjusanmt7wmr20enheql9afnwymxsmj02e36044mwgwk0msc6nsf3ndns",
	"ssq1pqc55t04la8nr0k6xf6zwxylr4eapeycuwls67p0rjxy55jss56ls0gtp52",
}

func TestV2KeystoreFromAnEarlierBuildStillOpens(t *testing.T) {
	on := readKeystore(t, v2FixturePath)
	if on.Version != keystoreVersion {
		t.Fatalf("fixture version = %d, want %d", on.Version, keystoreVersion)
	}
	if on.KDF != KDFArgon2id || on.KDFParams == nil || *on.KDFParams != defaultParams {
		t.Fatalf("fixture kdf = %q params = %+v", on.KDF, on.KDFParams)
	}

	m := NewManager(v2FixturePath, v2FixturePassphrase)
	if err := m.Load(); err != nil {
		t.Fatalf("Load of the version 2 fixture: %v", err)
	}
	if got := m.GetAddresses(); !reflect.DeepEqual(got, v2FixtureAddresses) {
		t.Fatalf("addresses = %v, want %v", got, v2FixtureAddresses)
	}
	for i, label := range v2FixtureLabels {
		want, err := FromSeed("ssq", sha256.Sum256([]byte(label)))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if want.Address != v2FixtureAddresses[i] {
			t.Fatalf("seed %d derives %s, want %s", i, want.Address, v2FixtureAddresses[i])
		}
		got, err := m.ExportPrivateKey(want.Address)
		if err != nil {
			t.Fatalf("ExportPrivateKey %s: %v", want.Address, err)
		}
		if !bytes.Equal(got, want.PrivateKey) {
			t.Errorf("key %d from the version 2 fixture is not the key the seed derives", i)
		}
	}
}

// readerFor is a second manager for a keystore written by one of the two key
// sources in TestSaveWritesAHeaderItWouldAccept.
func readerFor(t *testing.T, source, path string) *Manager {
	t.Helper()
	if source == "external key" {
		return newManagerWithKey(t, path, testExternalKey(t, "vault key"))
	}
	return NewManager(path, "pw")
}

// The binding is in force in the file Save writes, checked against the
// ciphertext itself rather than inferred from a refusal that another guard
// might also produce: the sealed message opens under the header's additional
// data, and under nothing else.
func TestTheHeaderIsBoundIntoTheCiphertext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	m := NewManager(path, "pw")
	saveOneKey(t, m)
	ks := readKeystore(t, path)

	open := func(aad []byte) error {
		key, err := m.deriveKey(&ks)
		if err != nil {
			t.Fatal(err)
		}
		gcm, err := openAEAD(key)
		if err != nil {
			t.Fatal(err)
		}
		_, err = gcm.Open(nil, ks.Nonce, ks.Ciphertext, aad)
		return err
	}

	aad, err := headerAAD(ks)
	if err != nil {
		t.Fatal(err)
	}
	if err := open(aad); err != nil {
		t.Fatalf("the ciphertext does not open under the header it was written with: %v", err)
	}
	if err := open(nil); err == nil {
		t.Error("the ciphertext opens with no additional data; the header is not bound")
	}

	altered := ks
	altered.PubKeys = append([]KeyPair(nil), ks.PubKeys...)
	altered.PubKeys[0].Address = v1FixtureAddresses[0]
	other, err := headerAAD(altered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other, aad) {
		t.Fatal("changing the public key list did not change the bound data")
	}
	if err := open(other); err == nil {
		t.Error("the ciphertext opens under an altered header; the header is not bound")
	}
}

// plaintextKey exists only to hold private keys, and it is handed between two
// files, so the guarantee KeyPair makes has to hold for it as well: no fmt
// verb prints key material. Without it a struct dump of what came out of the
// ciphertext writes 2560 bytes of private key to a log.
func TestPlaintextKeyPrintingRedactsThePrivateKey(t *testing.T) {
	kp, err := GenerateKeyForNetwork("ssq")
	if err != nil {
		t.Fatal(err)
	}
	k := plaintextKey(*kp)
	secret := hex.EncodeToString(k.PrivateKey[:16])

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
		for _, subject := range []any{k, &k, []plaintextKey{k}, plaintextKeys{Keys: []plaintextKey{k}}} {
			out := fmt.Sprintf(verb, subject)
			if strings.Contains(out, secret) {
				t.Errorf("%s of %T printed private key bytes", verb, subject)
			}
			if !strings.Contains(out, k.Address) {
				t.Errorf("%s of %T did not print the address: %s", verb, subject, out)
			}
		}
	}
}

// duplicateMember puts a second copy of a member in front of the real one, the
// way an attacker with write access would: Go's decoder keeps the last, so the
// prepended copy is the one a reader taking the first member would see, and
// the one headerAAD would not authenticate.
func duplicateMember(t *testing.T, path, member, prefix string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte(member))
	if i < 0 {
		t.Fatalf("the keystore has no member %s", member)
	}
	out := append(append(append([]byte(nil), data[:i]...), prefix...), data[i:]...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	// The edit has to be a file the standard parser still accepts, or the test
	// would be pinning a syntax error rather than the duplicate rule.
	var any map[string]any
	if err := json.Unmarshal(out, &any); err != nil {
		t.Fatalf("the edited file is not valid JSON, so this case tests nothing: %v", err)
	}
}

// The committed version 2 keystore written under an external key. The
// passphrase vector beside it does not cover this path: the AES key comes from
// HKDF over the key with the file's salt, so the derivation, the info string
// and the salt's part in it are pinned by nothing else that arrives from
// outside the running build. If this fails, a file an integrator unsealed from
// Vault yesterday does not open today.
const v2ExternalFixturePath = "testdata/keystore-v2-external.json"

var v2ExternalFixtureLabels = []string{
	"soqucoin-sdk keystore v2 external fixture 0",
	"soqucoin-sdk keystore v2 external fixture 1",
}

var v2ExternalFixtureAddresses = []string{
	"ssq1pz5k8h02qgef4u3yv3ur72lkdxjsfa2t99s78egjzmgnxqnwsun9qq46av3",
	"ssq1pje9q3xlvnt68pp7a3egrwrn6uhjmv3pz86ug2t4fpzr446up29vq7xpaq0",
}

func TestExternalKeyV2FromAnEarlierBuildStillOpens(t *testing.T) {
	on := readKeystore(t, v2ExternalFixturePath)
	if on.Version != keystoreVersion || on.KDF != KDFHKDFSHA256 || on.KDFParams != nil {
		t.Fatalf("fixture is version %d kdf %q params %+v", on.Version, on.KDF, on.KDFParams)
	}

	key := sha256.Sum256([]byte("soqucoin-sdk keystore v2 external fixture key"))
	m := newManagerWithKey(t, v2ExternalFixturePath, key[:])
	if err := m.Load(); err != nil {
		t.Fatalf("Load of the external-key fixture: %v", err)
	}
	if got := m.GetAddresses(); !reflect.DeepEqual(got, v2ExternalFixtureAddresses) {
		t.Fatalf("addresses = %v, want %v", got, v2ExternalFixtureAddresses)
	}
	for i, label := range v2ExternalFixtureLabels {
		want, err := FromSeed("ssq", sha256.Sum256([]byte(label)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.ExportPrivateKey(want.Address)
		if err != nil {
			t.Fatalf("ExportPrivateKey %s: %v", want.Address, err)
		}
		if !bytes.Equal(got, want.PrivateKey) {
			t.Errorf("key %d from the external-key fixture is not the key the seed derives", i)
		}
	}

	// Another external key does not open it, so this is a test about the key
	// and the derivation rather than about the file parsing.
	other := sha256.Sum256([]byte("another vault key"))
	if err := newManagerWithKey(t, v2ExternalFixturePath, other[:]).Load(); err == nil {
		t.Error("the external-key fixture opened under a different key")
	}
}
