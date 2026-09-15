// Package keys provides ML-DSA-44 (FIPS 204) key generation, deterministic
// derivation from a seed, signing, verification, and encrypted storage for
// Soqucoin.
//
// Two ways to hold keys: FromSeed with DeriveSeed derives one key per index
// from a master secret kept in your own key-management system, which is the
// per-user deposit-address path; Manager is the hot-wallet store, randomly
// generated keys in a file encrypted with AES-256-GCM, and also signs for a
// derived key imported in memory at sweep time.
//
// The keystore's encryption key comes from one of two places: a passphrase,
// stretched by Argon2id (NewManager), or a 32-byte key held in a
// key-management system (NewManagerWithKey). The file records which, along
// with the KDF parameters it was written with (keystore.go).
package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
)

// DilithiumKeySize constants matching Soqucoin's FIPS 204 ML-DSA-44 parameters.
const (
	PrivateKeySize = 2560 // ML-DSA-44 private key bytes (FIPS 204)
	PublicKeySize  = 1312 // ML-DSA-44 public key bytes
	SignatureSize  = 2420 // ML-DSA-44 signature bytes

	// sigHashAll is the only hashtype this SDK signs (tx.SigHashAll; this
	// package cannot import tx).
	sigHashAll = 0x01
)

// KeyPair holds a Dilithium keypair.
type KeyPair struct {
	PrivateKey []byte `json:"-"` // Never serialized in plain
	PublicKey  []byte `json:"pubkey"`
	Address    string `json:"address"` // Bech32m address for the network the key was derived for
	// Index is the record's position in a Manager's keystore, assigned by
	// ImportPrivateKey. It is not a derivation index: a KeyPair carries no
	// record of the seed or DeriveSeed index that produced it.
	Index uint32 `json:"index"`
}

// String prints the address and the hash of the public key, never the
// private key.
func (k KeyPair) String() string {
	return fmt.Sprintf("keys.KeyPair{Address: %s, PubKeyHash: %s, Index: %d}", k.Address, PubKeyHashHex(k.PublicKey), k.Index)
}

// Format makes every fmt verb print String for a KeyPair, a pointer to one,
// and one held in an exported field, a slice or a map: a Stringer alone
// covers the string verbs, and %d or %x on the struct would still walk the
// fields and print all 2560 private-key bytes. Two shapes fmt prints raw
// with no method dispatch and nothing here can change: %p applied to a
// non-pointer, and a KeyPair behind an unexported struct field. Do not hold
// a KeyPair where a struct dump can reach it that way.
func (k KeyPair) Format(f fmt.State, verb rune) { io.WriteString(f, k.String()) }

// plaintextKeys is the decrypted inner structure.
type plaintextKeys struct {
	Keys []struct {
		PrivateKey []byte `json:"sk"`
		PublicKey  []byte `json:"pk"`
		Address    string `json:"addr"`
		Index      uint32 `json:"index"`
	} `json:"keys"`
}

// Manager manages Dilithium keypairs with encrypted storage.
//
// Exactly one of passwd and extKey is the key source: NewManager sets passwd
// and leaves extKey nil, NewManagerWithKey the reverse. deriveKey is the only
// reader of either.
type Manager struct {
	mu      sync.RWMutex
	keys    []KeyPair
	keyFile string
	passwd  []byte
	extKey  []byte
}

var (
	// ErrInvalidPublicKey marks an ML-DSA-44 public key whose first byte is
	// 0xFF. The node's CPubKey uses that byte as its invalid-key sentinel
	// (src/pubkey.h GetLen), CheckSig fails on it, and the node's own key
	// generator regenerates when it appears (src/key.cpp). Roughly 1 key in 256
	// from a plain generator is such a key: its address still receives (the
	// witness program is SHA-256 of the raw key) but nothing can ever spend.
	ErrInvalidPublicKey = errors.New("keys: public key first byte 0xFF is the node's invalid-key marker; its address is unspendable")

	// ErrKeyMismatch marks a key record whose private key, public key and
	// address do not belong together. A manager that holds such a record would
	// hand out a deposit address it cannot sign for.
	ErrKeyMismatch = errors.New("keys: private key, public key and address do not match")

	// ErrHashType marks a witness-form signature (2421 bytes) whose trailing
	// hashtype byte is not SIGHASH_ALL, the only type this SDK signs.
	ErrHashType = errors.New("keys: signature hashtype is not SIGHASH_ALL (0x01)")

	// ErrKeystoreMissing is returned by Load when the keystore file does not
	// exist. A signer that starts with no keys because its path was mistyped
	// generates deposit addresses it can never spend from, so a missing file
	// is an error; LoadOrCreate is the first-run call that creates one.
	ErrKeystoreMissing = errors.New("keys: keystore file does not exist")

	// ErrKeysHeld is returned by Load and LoadOrCreate on a manager that
	// already holds keys, imported or loaded: reading the file would replace
	// them without a word.
	ErrKeysHeld = errors.New("keys: manager already holds keys; Load would discard them")

	// ErrNoKey marks an address this manager holds no key for.
	ErrNoKey = errors.New("keys: no key for address")
)

// maxKeygenAttempts bounds the regeneration loop in GenerateKeyForNetwork. The
// probability of exhausting it with a sound RNG is 256^-32.
const maxKeygenAttempts = 32

// checkPublicKey enforces size and the node's invalid-key sentinel.
func checkPublicKey(pubKey []byte) error {
	if len(pubKey) != PublicKeySize {
		return fmt.Errorf("invalid public key size: got %d, want %d", len(pubKey), PublicKeySize)
	}
	if pubKey[0] == 0xFF {
		return ErrInvalidPublicKey
	}
	return nil
}

// publicKeyOf derives the public key from a packed private key.
func publicKeyOf(privKey []byte) ([]byte, error) {
	if len(privKey) != PrivateKeySize {
		return nil, fmt.Errorf("invalid private key size: got %d, want %d", len(privKey), PrivateKeySize)
	}
	var skArr [PrivateKeySize]byte
	copy(skArr[:], privKey)
	var sk mldsa44.PrivateKey
	sk.Unpack(&skArr)
	for i := range skArr {
		skArr[i] = 0
	}
	pk, ok := sk.Public().(*mldsa44.PublicKey)
	if !ok {
		return nil, errors.New("keys: unexpected public key type")
	}
	return pk.Bytes(), nil
}

// AddressFor derives the bech32m v1 address of a public key for an HRP, the
// same way the node does (SHA-256 of the raw 1312-byte key, witness version 1).
func AddressFor(hrp string, pubKey []byte) (string, error) {
	if err := checkPublicKey(pubKey); err != nil {
		return "", err
	}
	pkHash := sha256.Sum256(pubKey)
	return soqaddr.Encode(hrp, 1, pkHash[:])
}

// checkKeyRecord verifies that a record's private key derives its public key,
// that the public key is acceptable to the node, and that the stored address
// is the one that key derives on the address's own network.
func checkKeyRecord(privKey, pubKey []byte, address string) error {
	if err := checkPublicKey(pubKey); err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	derivedPK, err := publicKeyOf(privKey)
	if err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	if string(derivedPK) != string(pubKey) {
		return fmt.Errorf("%s: %w (public key is not derived from the private key)", address, ErrKeyMismatch)
	}
	n, err := soqaddr.NetworkOf(address)
	if err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	want, err := AddressFor(n.HRP, pubKey)
	if err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	if want != address {
		return fmt.Errorf("%s: %w (key derives %s)", address, ErrKeyMismatch, want)
	}
	return nil
}

// NewManager creates a key manager whose keystore is encrypted under a key
// derived from passwd by Argon2id. The passphrase is held for the life of the
// manager, because Save re-derives from it.
func NewManager(keyFile string, passwd string) *Manager {
	return &Manager{
		keyFile: keyFile,
		passwd:  []byte(passwd),
	}
}

// NewManagerWithKey creates a key manager whose keystore is encrypted under a
// 32-byte key held somewhere else: a Vault transit key, an HSM-wrapped key, a
// key unsealed into the process at start. It is the deployment where no
// passphrase exists to be prompted for, typed or left in an environment
// variable, and the operator's at-rest protection is whatever their
// key-management system gives them rather than Argon2id over a human secret.
//
// The key must be ExternalKeySize bytes of secret random data
// (ErrExternalKeySize). The manager keeps its own copy, so the caller may
// zero its buffer as soon as the call returns.
//
// A manager built this way reads and writes only keystores written under an
// external key: handed a passphrase keystore it reports ErrKDFMismatch rather
// than a decryption failure. Version 1 files are passphrase-only, so they are
// opened with NewManager.
func NewManagerWithKey(keyFile string, key []byte) (*Manager, error) {
	if len(key) != ExternalKeySize {
		return nil, fmt.Errorf("%w: got %d", ErrExternalKeySize, len(key))
	}
	return &Manager{
		keyFile: keyFile,
		extKey:  bytes.Clone(key),
	}, nil
}

// Load decrypts and loads keys from the keystore file. A file that does not
// exist is ErrKeystoreMissing: a production signer must not start on an
// empty key set because its path was mistyped. Use LoadOrCreate for the
// first run. A manager that already holds keys returns ErrKeysHeld.
func (m *Manager) Load() error {
	return m.load(false)
}

// LoadOrCreate is Load for the first run: when the keystore file does not
// exist it creates an empty, encrypted one at the path, so every later Load
// on that path succeeds and a second process cannot mistake the path for a
// new one. Any other failure is reported as by Load.
func (m *Manager) LoadOrCreate() error {
	return m.load(true)
}

func (m *Manager) load(create bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.keys) > 0 {
		return fmt.Errorf("%w (%d keys)", ErrKeysHeld, len(m.keys))
	}

	data, err := os.ReadFile(m.keyFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read keystore: %w", err)
		}
		if !create {
			return fmt.Errorf("%w: %s", ErrKeystoreMissing, m.keyFile)
		}
		m.keys = []KeyPair{}
		return m.saveLocked()
	}

	var ks Keystore
	if err := json.Unmarshal(data, &ks); err != nil {
		return fmt.Errorf("parse keystore: %w", err)
	}

	// The additional data is the header as the file carries it, so it is
	// taken before upgradeV1 rewrites the header into the version 2 shape.
	aad, err := aadFor(ks)
	if err != nil {
		return err
	}
	if ks.Version == keystoreVersionV1 {
		if err := upgradeV1(&ks); err != nil {
			return err
		}
	}
	if err := checkHeader(&ks); err != nil {
		return err
	}

	encKey, err := m.deriveKey(&ks)
	if err != nil {
		return err
	}
	gcm, err := openAEAD(encKey)
	if err != nil {
		return err
	}
	plaintext, err := gcm.Open(nil, ks.Nonce, ks.Ciphertext, aad)
	if err != nil {
		// One failure covers a wrong key and an edited header alike: the
		// AEAD cannot tell them apart, and the checks above have already
		// reported every header fault that is decidable without the key.
		return fmt.Errorf("decrypt keystore (wrong key, or the file was altered): %w", err)
	}

	// Parse decrypted key material
	var pk plaintextKeys
	if err := json.Unmarshal(plaintext, &pk); err != nil {
		return fmt.Errorf("parse decrypted keys: %w", err)
	}

	// Wipe plaintext from memory after parsing
	for i := range plaintext {
		plaintext[i] = 0
	}

	// Refuse to serve a record the node could not spend or that this manager
	// could not sign for: a wrong or 0xFF public key here means a deposit
	// address that silently loses funds. Fail loudly, naming the address.
	for _, k := range pk.Keys {
		if err := checkKeyRecord(k.PrivateKey, k.PublicKey, k.Address); err != nil {
			return fmt.Errorf("keystore record rejected: %w", err)
		}
	}

	m.keys = make([]KeyPair, len(pk.Keys))
	for i, k := range pk.Keys {
		m.keys[i] = KeyPair{
			PrivateKey: k.PrivateKey,
			PublicKey:  k.PublicKey,
			Address:    k.Address,
			Index:      k.Index,
		}
	}

	return nil
}

// Save encrypts and persists keys to the keystore file.
func (m *Manager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.saveLocked()
}

// saveLocked is Save with the lock held by the caller.
func (m *Manager) saveLocked() error {
	// Serialize key material
	pk := plaintextKeys{}
	for _, k := range m.keys {
		pk.Keys = append(pk.Keys, struct {
			PrivateKey []byte `json:"sk"`
			PublicKey  []byte `json:"pk"`
			Address    string `json:"addr"`
			Index      uint32 `json:"index"`
		}{
			PrivateKey: k.PrivateKey,
			PublicKey:  k.PublicKey,
			Address:    k.Address,
			Index:      k.Index,
		})
	}

	plaintext, err := json.Marshal(pk)
	if err != nil {
		return fmt.Errorf("serialize keys: %w", err)
	}

	// A version 2 header for this manager's key source, with a fresh salt and
	// nonce. Save always writes version 2, so a version 1 file that was read
	// is rewritten in the new format by the first Save after it.
	ks, err := m.newHeader()
	if err != nil {
		return err
	}

	// Build public key list (unencrypted metadata). It is bound into the
	// AEAD with the rest of the header, so the addresses an operator reads
	// out of the file cannot be swapped for an attacker's.
	ks.PubKeys = make([]KeyPair, len(m.keys))
	for i, k := range m.keys {
		ks.PubKeys[i] = KeyPair{
			PublicKey: k.PublicKey,
			Address:   k.Address,
			Index:     k.Index,
		}
	}

	// The header is checked here, not only on the way in: whatever this
	// package writes, it must be able to read back.
	if err := checkHeader(&ks); err != nil {
		return err
	}
	aad, err := headerAAD(ks)
	if err != nil {
		return err
	}
	encKey, err := m.deriveKey(&ks)
	if err != nil {
		return err
	}
	gcm, err := openAEAD(encKey)
	if err != nil {
		return err
	}
	ks.Ciphertext = gcm.Seal(nil, ks.Nonce, plaintext, aad)

	// Wipe plaintext
	for i := range plaintext {
		plaintext[i] = 0
	}

	data, err := json.MarshalIndent(ks, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize keystore: %w", err)
	}

	// Written, synced and renamed into place, then the directory synced, so a
	// crash at any point leaves the previous keystore or this one and never a
	// partial file; a key added just before a power loss is on disk when
	// Save returns.
	if err := atomicfile.WriteFile(m.keyFile, data, 0600); err != nil {
		return fmt.Errorf("write keystore: %w", err)
	}
	return nil
}

// ImportPrivateKey adds a key held in memory: a derived deposit key at sweep
// time, or a raw key from a wallet.dat dump. Call Save only for a hot-wallet
// key that should persist. The manager keeps its own copy of both slices, so
// the caller may zero its buffers as soon as the call returns.
func (m *Manager) ImportPrivateKey(privKey []byte, pubKey []byte, address string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := checkKeyRecord(privKey, pubKey, address); err != nil {
		return err
	}
	privKey = bytes.Clone(privKey)
	pubKey = bytes.Clone(pubKey)

	// Check for duplicate
	for _, k := range m.keys {
		if k.Address == address {
			return fmt.Errorf("key for address %s already exists", address)
		}
	}

	var nextIndex uint32
	if len(m.keys) > 0 {
		nextIndex = m.keys[len(m.keys)-1].Index + 1
	}

	m.keys = append(m.keys, KeyPair{
		PrivateKey: privKey,
		PublicKey:  pubKey,
		Address:    address,
		Index:      nextIndex,
	})

	return nil
}

// GetAddresses returns all managed addresses.
func (m *Manager) GetAddresses() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	addrs := make([]string, len(m.keys))
	for i, k := range m.keys {
		addrs[i] = k.Address
	}
	return addrs
}

// HasKey reports whether the manager holds a key for address.
func (m *Manager) HasKey(address string) bool {
	_, err := m.keyFor(address)
	return err == nil
}

// keyFor returns the record for an address. The slices in the returned value
// are the manager's own; every exported method that hands bytes out copies
// them first, and the private key leaves only through ExportPrivateKey.
func (m *Manager) keyFor(address string) (KeyPair, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, k := range m.keys {
		if k.Address == address {
			return k, nil
		}
	}
	return KeyPair{}, fmt.Errorf("%w: %s", ErrNoKey, address)
}

// ExportPrivateKey returns a copy of the private key for address. It is the
// only way key material leaves a Manager, named so that every use is found by
// a search of the code base; a sweep to another store or a backup are the
// expected callers. Zero the copy when it is no longer needed. Signing does
// not need it: pass the Manager itself as the tx.Signer.
func (m *Manager) ExportPrivateKey(address string) ([]byte, error) {
	kp, err := m.keyFor(address)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(kp.PrivateKey), nil
}

// Sign signs a message digest with the Dilithium private key for the given address.
// Uses ML-DSA-44 (FIPS 204) via circl mldsa44 — returns 2420-byte signature.
func (m *Manager) Sign(address string, digest []byte) ([]byte, error) {
	kp, err := m.keyFor(address)
	if err != nil {
		return nil, err
	}

	// Load raw bytes into FIPS 204 ML-DSA-44 PrivateKey
	var skArr [PrivateKeySize]byte
	copy(skArr[:], kp.PrivateKey)
	var sk mldsa44.PrivateKey
	sk.Unpack(&skArr)

	// Wipe the array copy from stack
	for i := range skArr {
		skArr[i] = 0
	}

	// Sign the digest with the hedged (randomized) variant FIPS 204 recommends
	// for a signer whose host an attacker may profile; no context string.
	// Verification does not depend on the randomizer, so the node accepts
	// either form and two signatures of one digest legitimately differ.
	sig := make([]byte, mldsa44.SignatureSize)
	if err := mldsa44.SignTo(&sk, digest, nil, true, sig); err != nil {
		return nil, fmt.Errorf("dilithium sign: %w", err)
	}

	return sig, nil
}

// Verify verifies a Dilithium signature against a public key and message digest.
//
// The digest is the caller's. Verify cannot tell whether it is the digest the
// node will compute from the transaction, so a transaction input is checked
// with tx.VerifyInput, which reads the hashtype from the witness and recomputes
// the sighash; use Verify for a signature over a digest you produced yourself.
func Verify(pubKey []byte, digest []byte, signature []byte) (bool, error) {
	// Accept the witness forms as well as the raw ones: consensus requires the
	// public key to be pushed as 0x00||pk (1313 bytes) and the signature to
	// carry a trailing hashtype byte (2421); the interpreter strips both before
	// verifying (src/script/interpreter.cpp). A verifier that rejects the form
	// the SDK itself emits is not usable on the withdrawal path.
	if len(pubKey) == PublicKeySize+1 && pubKey[0] == 0x00 {
		pubKey = pubKey[1:]
	}
	if len(signature) == SignatureSize+1 {
		// The node computes the sighash from this byte. The SDK signs
		// SIGHASH_ALL only, and a caller's digest is a SIGHASH_ALL digest, so
		// any other byte means the node would verify against a different
		// message than the one checked here.
		if signature[SignatureSize] != sigHashAll {
			return false, fmt.Errorf("%w: %#02x", ErrHashType, signature[SignatureSize])
		}
		signature = signature[:SignatureSize]
	}
	if err := checkPublicKey(pubKey); err != nil {
		return false, err
	}
	if len(signature) != SignatureSize {
		return false, fmt.Errorf("invalid signature size: %d", len(signature))
	}

	var pkArr [PublicKeySize]byte
	copy(pkArr[:], pubKey)
	var pk mldsa44.PublicKey
	pk.Unpack(&pkArr)

	return mldsa44.Verify(&pk, digest, nil, signature), nil
}

// GenerateKeyForNetwork generates a new ML-DSA-44 keypair and derives the bech32m
// address for the specified network HRP (e.g., "ssq" for stagenet, "sq" for mainnet).
//
// A key whose public key begins with 0xFF is regenerated, exactly as the node's
// own CKey::MakeNewKey does (src/key.cpp): the node treats that byte as its
// invalid-key sentinel and can never spend from such a key (ErrInvalidPublicKey).
func GenerateKeyForNetwork(hrp string) (*KeyPair, error) {
	for attempt := 0; attempt < maxKeygenAttempts; attempt++ {
		pk, sk, err := mldsa44.GenerateKey(nil)
		if err != nil {
			return nil, fmt.Errorf("generate ML-DSA-44 key: %w", err)
		}
		pkBytes := pk.Bytes()
		if pkBytes[0] == 0xFF {
			skBytes := sk.Bytes()
			for i := range skBytes {
				skBytes[i] = 0
			}
			continue
		}
		skBytes := sk.Bytes()

		// Encode as bech32m: witness version 1 (Dilithium), 32-byte program =
		// SHA-256 of the raw public key.
		addr, err := AddressFor(hrp, pkBytes)
		if err != nil {
			return nil, fmt.Errorf("encode bech32m address: %w", err)
		}

		return &KeyPair{
			PrivateKey: skBytes,
			PublicKey:  pkBytes,
			Address:    addr,
		}, nil
	}
	return nil, errors.New("keys: key generation produced only invalid-marker keys; the random source is broken")
}

// GenerateKey generates a new ML-DSA-44 keypair with a stagenet address (ssq1p...).
//
// Deprecated: the network default is a trap for mainnet integrators. Use
// GenerateKeyForNetwork(types.Mainnet.HRP) or GenerateKeyForNetwork("ssq").
func GenerateKey() (*KeyPair, error) {
	return GenerateKeyForNetwork("ssq")
}

// PubKeyHash returns SHA-256 hash of the public key (32-byte witness program).
func PubKeyHash(pubKey []byte) []byte {
	h := sha256.Sum256(pubKey)
	return h[:]
}

// PubKeyHashHex returns the hex-encoded pubkey hash.
func PubKeyHashHex(pubKey []byte) string {
	return hex.EncodeToString(PubKeyHash(pubKey))
}

// KeyCount returns the number of managed keys.
func (m *Manager) KeyCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}

// PublicKeyFor returns a copy of the public key for a managed address.
//
// This exists so that tx.Signer can be satisfied by *Manager directly, without
// the tx package needing to import this one.
func (m *Manager) PublicKeyFor(address string) ([]byte, error) {
	kp, err := m.keyFor(address)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(kp.PublicKey), nil
}
