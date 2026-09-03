// Package keys provides Dilithium (FIPS 204 ML-DSA-44) key generation, signing,
// verification, and encrypted storage for Soqucoin.
//
// Keys are stored encrypted at rest using AES-256-GCM with Argon2id key derivation.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"golang.org/x/crypto/argon2"
)

// DilithiumKeySize constants matching Soqucoin's FIPS 204 ML-DSA-44 parameters.
const (
	PrivateKeySize = 2560 // ML-DSA-44 private key bytes (FIPS 204)
	PublicKeySize  = 1312 // ML-DSA-44 public key bytes
	SignatureSize  = 2420 // ML-DSA-44 signature bytes
)

// KeyPair holds a Dilithium keypair.
type KeyPair struct {
	PrivateKey []byte `json:"-"` // Never serialized in plain
	PublicKey  []byte `json:"pubkey"`
	Address    string `json:"address"` // Bech32m ssq1p... address
	Index      uint32 `json:"index"`   // Derivation index
}

// Keystore holds encrypted key material on disk.
type Keystore struct {
	Version    int       `json:"version"`    // Format version (1)
	KDF        string    `json:"kdf"`        // "argon2id"
	Salt       []byte    `json:"salt"`       // 32-byte random salt
	Nonce      []byte    `json:"nonce"`      // 12-byte AES-GCM nonce
	Ciphertext []byte    `json:"ciphertext"` // AES-256-GCM encrypted key material
	PubKeys    []KeyPair `json:"pubkeys"`    // Public keys (unencrypted, for address tracking)
}

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
type Manager struct {
	mu      sync.RWMutex
	keys    []KeyPair
	keyFile string
	passwd  []byte
	loaded  bool
}

// NewManager creates a new key manager.
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

func NewManager(keyFile string, passwd string) *Manager {
	return &Manager{
		keyFile: keyFile,
		passwd:  []byte(passwd),
	}
}

// Load decrypts and loads keys from the keystore file.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.keyFile)
	if err != nil {
		if os.IsNotExist(err) {
			// No keystore yet — empty state is valid for first run
			m.keys = []KeyPair{}
			m.loaded = true
			return nil
		}
		return fmt.Errorf("read keystore: %w", err)
	}

	var ks Keystore
	if err := json.Unmarshal(data, &ks); err != nil {
		return fmt.Errorf("parse keystore: %w", err)
	}

	if ks.Version != 1 {
		return fmt.Errorf("unsupported keystore version: %d", ks.Version)
	}

	// Derive encryption key from passphrase via Argon2id
	encKey := argon2.IDKey(m.passwd, ks.Salt, 3, 64*1024, 4, 32)

	// Decrypt with AES-256-GCM
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("create gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, ks.Nonce, ks.Ciphertext, nil)
	if err != nil {
		return fmt.Errorf("decrypt keystore (wrong passphrase?): %w", err)
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

	m.loaded = true
	return nil
}

// Save encrypts and persists keys to the keystore file.
func (m *Manager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

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

	// Generate salt and nonce
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	// Derive encryption key
	encKey := argon2.IDKey(m.passwd, salt, 3, 64*1024, 4, 32)

	// Encrypt with AES-256-GCM
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("create gcm: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Wipe plaintext
	for i := range plaintext {
		plaintext[i] = 0
	}

	// Build public key list (unencrypted metadata)
	pubKeys := make([]KeyPair, len(m.keys))
	for i, k := range m.keys {
		pubKeys[i] = KeyPair{
			PublicKey: k.PublicKey,
			Address:   k.Address,
			Index:     k.Index,
		}
	}

	ks := Keystore{
		Version:    1,
		KDF:        "argon2id",
		Salt:       salt,
		Nonce:      nonce,
		Ciphertext: ciphertext,
		PubKeys:    pubKeys,
	}

	data, err := json.MarshalIndent(ks, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize keystore: %w", err)
	}

	// Atomic write: temp file + rename
	tmpFile := m.keyFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("write keystore: %w", err)
	}
	if err := os.Rename(tmpFile, m.keyFile); err != nil {
		return fmt.Errorf("rename keystore: %w", err)
	}

	return nil
}

// ImportPrivateKey imports a raw Dilithium private key (from wallet.dat dump).
func (m *Manager) ImportPrivateKey(privKey []byte, pubKey []byte, address string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := checkKeyRecord(privKey, pubKey, address); err != nil {
		return err
	}

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

// GetSignableAddresses returns only addresses whose private keys are
// the correct FIPS 204 ML-DSA-44 size (2560 bytes). Keys with legacy
// sizes (e.g., 2528B circl format) cannot be used for signing.
func (m *Manager) GetSignableAddresses() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var addrs []string
	for _, k := range m.keys {
		if len(k.PrivateKey) == PrivateKeySize {
			addrs = append(addrs, k.Address)
		}
	}
	return addrs
}

// GetKeyForAddress returns the keypair for a given address.
func (m *Manager) GetKeyForAddress(address string) (*KeyPair, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, k := range m.keys {
		if k.Address == address {
			return &k, nil
		}
	}
	return nil, fmt.Errorf("no key for address: %s", address)
}

// Sign signs a message digest with the Dilithium private key for the given address.
// Uses ML-DSA-44 (FIPS 204) via circl mldsa44 — returns 2420-byte signature.
func (m *Manager) Sign(address string, digest []byte) ([]byte, error) {
	kp, err := m.GetKeyForAddress(address)
	if err != nil {
		return nil, err
	}

	if len(kp.PrivateKey) != PrivateKeySize {
		return nil, fmt.Errorf("invalid private key size: %d (expected %d)", len(kp.PrivateKey), PrivateKeySize)
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

	// Sign the digest (deterministic, no context string)
	sig := make([]byte, mldsa44.SignatureSize)
	if err := mldsa44.SignTo(&sk, digest, nil, false, sig); err != nil {
		return nil, fmt.Errorf("dilithium sign: %w", err)
	}

	return sig, nil
}

// Verify verifies a Dilithium signature against a public key and message digest.
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

// PublicKeyFor returns the public key for a managed address.
//
// This exists so that tx.Signer can be satisfied by *Manager directly, without
// the tx package needing to import this one.
func (m *Manager) PublicKeyFor(address string) ([]byte, error) {
	kp, err := m.GetKeyForAddress(address)
	if err != nil {
		return nil, err
	}
	return kp.PublicKey, nil
}
