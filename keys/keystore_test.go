package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	path := copyFixture(t)
	if err := newManagerWithKey(t, path, testExternalKey(t, "vault key")).Load(); !errors.Is(err, ErrKDFMismatch) {
		t.Errorf("external-key manager on a version 1 keystore: %v, want ErrKDFMismatch", err)
	}
}

// The attack: whoever can write the keystore file edits the parameters the
// reader now takes from it. Below the floor is the downgrade, which would
// make an offline guess at the passphrase cheap; above the ceiling is a
// memory bomb against the process that opens the file. Both are refused by
// name, before Argon2id is asked to run with them.
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

// Every field of the header is bound into the AEAD, so an edit to any of them
// is refused. The list below is the enumeration of the header's fields: the
// last case checks it against the struct so a field added later cannot be
// left off it.
func TestEveryHeaderFieldIsTamperEvident(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range []struct {
		field string
		name  string
		edit  func(h map[string]any)
		wants error
	}{
		{"version", "an unknown version", func(h map[string]any) { h["version"] = 3 }, ErrKeystoreVersion},
		{"version", "claimed to be version 1", func(h map[string]any) {
			h["version"] = 1
			delete(h, "kdfparams")
		}, nil},
		{"kdf", "another key source", func(h map[string]any) {
			h["kdf"] = KDFHKDFSHA256
			delete(h, "kdfparams")
		}, ErrKDFMismatch},
		{"kdf", "an unknown kdf", func(h map[string]any) { h["kdf"] = "pbkdf2" }, ErrKeystoreHeader},
		{"kdfparams", "parameters changed within the accepted range", func(h map[string]any) {
			h["kdfparams"].(map[string]any)["t"] = maxTime
		}, nil},
		{"salt", "another salt", func(h map[string]any) { h["salt"] = flipFirstByte(t, h["salt"]) }, nil},
		{"salt", "a short salt", func(h map[string]any) { h["salt"] = "" }, ErrKeystoreHeader},
		{"nonce", "another nonce", func(h map[string]any) { h["nonce"] = flipFirstByte(t, h["nonce"]) }, nil},
		{"nonce", "a short nonce", func(h map[string]any) { h["nonce"] = "" }, ErrKeystoreHeader},
		{"ciphertext", "an edited ciphertext", func(h map[string]any) { h["ciphertext"] = flipFirstByte(t, h["ciphertext"]) }, nil},
		{"pubkeys", "the operator's copy of the address swapped", func(h map[string]any) {
			h["pubkeys"].([]any)[0].(map[string]any)["address"] = v1FixtureAddresses[0]
		}, nil},
	} {
		covered[tc.field] = true
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.enc")
			saveOneKey(t, NewManager(path, "pw"))
			editHeader(t, path, tc.edit)
			m := NewManager(path, "pw")
			err := m.Load()
			if err == nil {
				t.Fatal("Load accepted an edited keystore")
			}
			if tc.wants != nil && !errors.Is(err, tc.wants) {
				t.Fatalf("Load: %v, want %v", err, tc.wants)
			}
			if m.KeyCount() != 0 {
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

// headerAAD is derived from the Keystore value rather than a hand-written
// list, so that a field added to the header later is bound without anyone
// remembering to bind it. This is the check that it stays that way.
func TestHeaderAADBindsEveryHeaderField(t *testing.T) {
	ks := Keystore{
		Version:    keystoreVersion,
		KDF:        KDFArgon2id,
		KDFParams:  &KDFParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32},
		Salt:       bytes.Repeat([]byte{1}, saltSize),
		Nonce:      bytes.Repeat([]byte{2}, nonceSize),
		Ciphertext: bytes.Repeat([]byte{3}, 64),
		PubKeys:    []KeyPair{{PublicKey: bytes.Repeat([]byte{4}, PublicKeySize), Address: v1FixtureAddresses[0]}},
	}
	aad, err := headerAAD(ks)
	if err != nil {
		t.Fatalf("headerAAD: %v", err)
	}
	typ := reflect.TypeFor[Keystore]()
	for i := range typ.NumField() {
		tag := jsonName(typ.Field(i))
		if !strings.Contains(string(aad), `"`+tag+`"`) {
			t.Errorf("header field %q is not in the bound data", tag)
		}
	}
	// The ciphertext is the one field that is not bound: the AEAD covers it
	// as the message, and repeating it here would double the file's size.
	ct, err := json.Marshal(ks.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(aad, ct) {
		t.Error("the ciphertext is in the bound data as well as in the message")
	}
	for _, p := range []string{`"t"`, `"m"`, `"p"`, `"keylen"`} {
		if !strings.Contains(string(aad), p) {
			t.Errorf("KDF parameter %s is not in the bound data", p)
		}
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
			saveOneKey(t, m)
			ks := readKeystore(t, m.keyFile)
			if err := checkHeader(&ks); err != nil {
				t.Fatalf("Save wrote a header Load refuses: %v", err)
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
	path := copyFixture(t)

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
			path := copyFixture(t)
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

// copyFixture puts a writable copy of the version 1 fixture in a temporary
// directory, so a test that saves over it does not edit the committed file.
func copyFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(v1FixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "keystore-v1.json")
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
