package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Keystore is the on-disk file. Everything outside Ciphertext is public and
// is bound into the AEAD as additional data (headerAAD), so a header edited
// after the file was written is refused as a forgery.
//
// Version 1 (v0.3.6 and earlier) had no KDFParams and no additional data: its
// Argon2id parameters were compiled into the reader, so changing them would
// have been a format break with a migration. Version 2 carries them. A
// version 1 file is read (v1Params below) and rewritten as version 2 by the
// next Save.
type Keystore struct {
	Version    int        `json:"version"`             // Format version (1 or 2)
	KDF        string     `json:"kdf"`                 // KDFArgon2id or KDFHKDFSHA256
	KDFParams  *KDFParams `json:"kdfparams,omitempty"` // Argon2id only; absent for an external key
	Salt       []byte     `json:"salt"`                // 32-byte random salt, fresh on every Save
	Nonce      []byte     `json:"nonce"`               // 12-byte AES-GCM nonce, fresh on every Save
	Ciphertext []byte     `json:"ciphertext"`          // AES-256-GCM encrypted key material
	PubKeys    []KeyPair  `json:"pubkeys"`             // Public keys (unencrypted, for address tracking)
}

// KDFParams are the Argon2id parameters the file was written with. They are
// in the file so they can be raised without a format break, and they are
// bound into the AEAD and floored below so that putting them in the file does
// not hand an attacker who can write the file a way to weaken it.
type KDFParams struct {
	Time    uint32 `json:"t"`      // passes
	Memory  uint32 `json:"m"`      // KiB
	Threads uint8  `json:"p"`      // lanes
	KeyLen  uint32 `json:"keylen"` // derived key bytes; 32 for AES-256
}

// The two key sources, as they are named in the file's kdf field. A manager
// opens only the one it was constructed for: a passphrase manager handed a
// file written under an external key reports that, rather than deriving a key
// that cannot decrypt and blaming the passphrase.
const (
	KDFArgon2id   = "argon2id"    // NewManager: Argon2id over the passphrase
	KDFHKDFSHA256 = "hkdf-sha256" // NewManagerWithKey: HKDF over the external key
)

const (
	keystoreVersionV1 = 1
	keystoreVersion   = 2 // what Save writes

	// ExternalKeySize is the length of the key NewManagerWithKey takes.
	ExternalKeySize = 32

	saltSize   = 32
	nonceSize  = 12 // AES-GCM standard nonce
	encKeySize = 32 // AES-256

	// hkdfInfo separates this use of an external key from any other use the
	// same key-management system puts it to. It is part of the scheme.
	hkdfInfo = "soqucoin-sdk/keystore/v2/aes-256-gcm"
)

// defaultParams are RFC 9106's second recommended set, which version 1 also
// used, so a version 1 file rewritten as version 2 costs the same to open.
var defaultParams = KDFParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: encKeySize}

// v1Params are the parameters version 1 compiled into its reader. A version 1
// file carries no parameters; these are what it was written with.
var v1Params = KDFParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: encKeySize}

// The floor and the ceiling on the parameters read from a file. The floor is
// the downgrade guard: without it, moving the parameters into the header
// would let whoever can write the file drop the work factor to nothing and
// brute-force the passphrase offline at leisure. The ceiling is the other
// direction, a header claiming 64 GiB of Argon2id memory is a memory bomb
// against the process that opens it.
const (
	minTime      = 3
	maxTime      = 16
	minMemoryKiB = 64 * 1024   // 64 MiB
	maxMemoryKiB = 1024 * 1024 // 1 GiB
	minThreads   = 1
	maxThreads   = 16
)

var (
	// ErrKeystoreVersion marks a keystore file whose format version this
	// build does not read.
	ErrKeystoreVersion = errors.New("keys: unsupported keystore version")

	// ErrKeystoreHeader marks a keystore header that is malformed: an
	// unknown KDF, parameters present for a KDF that takes none or absent
	// for one that needs them, or a salt or nonce of the wrong length.
	ErrKeystoreHeader = errors.New("keys: keystore header rejected")

	// ErrKDFParams marks Argon2id parameters outside the range this package
	// accepts. Below the floor is the downgrade attack; above the ceiling is
	// a memory bomb.
	ErrKDFParams = errors.New("keys: keystore KDF parameters outside the accepted range")

	// ErrKDFMismatch marks a keystore written under the other key source: a
	// passphrase keystore opened by a manager holding an external key, or
	// the reverse. It is not a wrong passphrase, and saying so saves an
	// operator the wrong search.
	ErrKDFMismatch = errors.New("keys: keystore was written under a different key source")

	// ErrExternalKeySize marks a key handed to NewManagerWithKey that is not
	// ExternalKeySize bytes.
	ErrExternalKeySize = errors.New("keys: external key must be 32 bytes")
)

// check enforces the floor and the ceiling on parameters read from a file.
// Every path that derives a key from Argon2id parameters goes through
// deriveKey, which calls this; nothing else reads the parameters.
func (p *KDFParams) check() error {
	switch {
	case p.Time < minTime || p.Time > maxTime:
		return fmt.Errorf("%w: time %d outside [%d, %d]", ErrKDFParams, p.Time, minTime, maxTime)
	case p.Memory < minMemoryKiB || p.Memory > maxMemoryKiB:
		return fmt.Errorf("%w: memory %d KiB outside [%d, %d]", ErrKDFParams, p.Memory, minMemoryKiB, maxMemoryKiB)
	case p.Threads < minThreads || p.Threads > maxThreads:
		return fmt.Errorf("%w: threads %d outside [%d, %d]", ErrKDFParams, p.Threads, minThreads, maxThreads)
	case p.KeyLen != encKeySize:
		return fmt.Errorf("%w: derived key length %d, want %d", ErrKDFParams, p.KeyLen, encKeySize)
	}
	return nil
}

// checkHeader is the one statement of what a well-formed header is. Both
// directions call it: load on the header it has just parsed, and save on the
// header it is about to write, so this package cannot write a file it would
// refuse to read.
func checkHeader(ks *Keystore) error {
	if ks.Version != keystoreVersion {
		return fmt.Errorf("%w: %d", ErrKeystoreVersion, ks.Version)
	}
	switch ks.KDF {
	case KDFArgon2id:
		if ks.KDFParams == nil {
			return fmt.Errorf("%w: kdf %q with no parameters", ErrKeystoreHeader, ks.KDF)
		}
	case KDFHKDFSHA256:
		if ks.KDFParams != nil {
			return fmt.Errorf("%w: kdf %q takes no parameters", ErrKeystoreHeader, ks.KDF)
		}
	default:
		return fmt.Errorf("%w: unknown kdf %q", ErrKeystoreHeader, ks.KDF)
	}
	if len(ks.Salt) != saltSize {
		return fmt.Errorf("%w: salt is %d bytes, want %d", ErrKeystoreHeader, len(ks.Salt), saltSize)
	}
	if len(ks.Nonce) != nonceSize {
		return fmt.Errorf("%w: nonce is %d bytes, want %d", ErrKeystoreHeader, len(ks.Nonce), nonceSize)
	}
	return nil
}

// upgradeV1 turns the header of a version 1 file into the version 2 header
// that describes it, so that everything after it in load reads one shape. The
// parameters are the ones version 1 compiled in; the salt, nonce and
// ciphertext are the file's own.
func upgradeV1(ks *Keystore) error {
	if ks.KDF != KDFArgon2id {
		return fmt.Errorf("%w: version 1 kdf %q, want %q", ErrKeystoreHeader, ks.KDF, KDFArgon2id)
	}
	if ks.KDFParams != nil {
		return fmt.Errorf("%w: version 1 carries no parameters", ErrKeystoreHeader)
	}
	params := v1Params
	ks.Version = keystoreVersion
	ks.KDFParams = &params
	return nil
}

// headerAAD is the additional data the AEAD binds: every field of the file
// except the ciphertext, in the encoding the file itself uses. It is derived
// from the Keystore value rather than from a hand-written list of fields so
// that a field added to the header later is bound without anyone having to
// remember to bind it; TestHeaderAADBindsEveryHeaderField enumerates the
// struct and fails if one is missing.
//
// Version 1 wrote no additional data and is opened with none (aadFor). What
// binding buys over version 1's accident: there, editing the salt produced a
// different key and a "wrong passphrase" error, and editing anything else
// produced nothing at all. Here every edit is one refusal with one reason.
func headerAAD(ks Keystore) ([]byte, error) {
	ks.Ciphertext = nil
	aad, err := json.Marshal(ks)
	if err != nil {
		return nil, fmt.Errorf("serialize keystore header: %w", err)
	}
	return aad, nil
}

// aadFor is headerAAD for a version 2 file and nothing for a version 1 file,
// which was written without it. Call it on the header as parsed, before
// upgradeV1 fills in the version 2 shape: the additional data must be the
// bytes the file was sealed against, not the bytes we would write today.
func aadFor(ks Keystore) ([]byte, error) {
	if ks.Version == keystoreVersionV1 {
		return nil, nil
	}
	return headerAAD(ks)
}

// kdfName is the KDF this manager writes and the only one it opens. The key
// source is fixed at construction, so both directions ask this one question
// rather than each testing extKey for itself.
func (m *Manager) kdfName() string {
	if m.extKey != nil {
		return KDFHKDFSHA256
	}
	return KDFArgon2id
}

// keySourceOf names a KDF the way an operator holds it, for the one error
// where the KDF identifier alone would not say what to go and fix.
func keySourceOf(kdf string) string {
	if kdf == KDFHKDFSHA256 {
		return "an external key"
	}
	return "a passphrase"
}

// deriveKey produces the AES-256 key for a header, and is the only place in
// this package that produces one. A file written under the other key source is
// named as such: a decryption failure would send an operator looking for a
// wrong passphrase that was never used.
func (m *Manager) deriveKey(ks *Keystore) ([]byte, error) {
	if ks.KDF != m.kdfName() {
		return nil, fmt.Errorf("%w: the file was written under %s, this manager holds %s",
			ErrKDFMismatch, keySourceOf(ks.KDF), keySourceOf(m.kdfName()))
	}
	switch ks.KDF {
	case KDFArgon2id:
		if err := ks.KDFParams.check(); err != nil {
			return nil, err
		}
		p := ks.KDFParams
		return argon2.IDKey(m.passwd, ks.Salt, p.Time, p.Memory, p.Threads, p.KeyLen), nil
	case KDFHKDFSHA256:
		// The external key is a key, not a passphrase, so it needs no work
		// factor. HKDF is here for the salt: it gives every Save a different
		// AES key, so an external key that never changes does not put the
		// whole burden of never repeating on the nonce.
		return hkdf.Key(sha256.New, m.extKey, ks.Salt, hkdfInfo, encKeySize)
	default:
		return nil, fmt.Errorf("%w: unknown kdf %q", ErrKeystoreHeader, ks.KDF)
	}
}

// newHeader builds the version 2 header Save writes for this manager's key
// source, with a fresh salt and nonce.
func (m *Manager) newHeader() (Keystore, error) {
	ks := Keystore{
		Version: keystoreVersion,
		KDF:     m.kdfName(),
		Salt:    make([]byte, saltSize),
		Nonce:   make([]byte, nonceSize),
	}
	if ks.KDF == KDFArgon2id {
		params := defaultParams
		ks.KDFParams = &params
	}
	if _, err := rand.Read(ks.Salt); err != nil {
		return Keystore{}, fmt.Errorf("generate salt: %w", err)
	}
	if _, err := rand.Read(ks.Nonce); err != nil {
		return Keystore{}, fmt.Errorf("generate nonce: %w", err)
	}
	return ks, nil
}

// openAEAD builds the AES-256-GCM instance for a derived key and zeroes the
// caller's copy of it: the cipher keeps its own expanded schedule.
func openAEAD(encKey []byte) (cipher.AEAD, error) {
	defer func() {
		for i := range encKey {
			encKey[i] = 0
		}
	}()
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	if gcm.NonceSize() != nonceSize {
		return nil, fmt.Errorf("gcm nonce size %d, want %d", gcm.NonceSize(), nonceSize)
	}
	return gcm, nil
}
