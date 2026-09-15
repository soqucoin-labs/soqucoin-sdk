package keys

import (
	"bytes"
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

// Keystore is the on-disk file. Everything outside Ciphertext is public. In
// version 2 all of it is bound into the AEAD as additional data (headerAAD),
// and load refuses a field it does not know, so a version 2 header edited
// after the file was written is refused as a forgery. Version 1 bound none of
// it, which is why load compares the public key list against the decrypted
// records as well: that check holds for both formats.
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
// in the file so they can be raised without a format break. They are bound
// into the AEAD with the rest of the header, and held between a floor and a
// ceiling whose separate reasons are given where those are declared.
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

// The floor and the ceiling on the parameters read from a file.
//
// The ceiling is load-bearing against a hostile file: check runs before
// argon2.IDKey, so a header claiming 64 GiB of Argon2id memory is refused
// rather than allocated by whatever opens the file.
//
// The floor is not, and it is worth writing down why, because the obvious
// story for it is wrong. Editing the parameters down does not weaken a file:
// they feed the derivation, so the edited file opens for no passphrase at
// all, and an attacker guessing offline against their own copy would run
// the parameters the file was really written with regardless of what its
// header says. What the floor catches is a file that was legitimately
// written weak, by an older writer, another implementation, or a future
// configuration knob, and would otherwise open without a word while its
// at-rest protection was worth far less than the operator believed.
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
	// accepts: below the floor, a file whose at-rest protection is worth less
	// than its holder believes; above the ceiling, a memory bomb.
	ErrKDFParams = errors.New("keys: keystore KDF parameters outside the accepted range")

	// ErrKDFMismatch marks a keystore written under the other key source: a
	// passphrase keystore opened by a manager holding an external key, or
	// the reverse. It is not a wrong passphrase, and saying so saves an
	// operator the wrong search.
	ErrKDFMismatch = errors.New("keys: keystore was written under a different key source")

	// ErrExternalKeySize marks a key handed to NewManagerWithKey that is not
	// ExternalKeySize bytes.
	ErrExternalKeySize = errors.New("keys: external key must be 32 bytes")

	// ErrPubKeyList marks a keystore whose unencrypted public key list does
	// not describe the keys the ciphertext holds. That list is what an
	// operator or an external tool reads to find an address to send to, so a
	// file where the two disagree is refused rather than served.
	ErrPubKeyList = errors.New("keys: the keystore's public key list does not match the keys it holds")
)

// checkPubKeyList compares the file's unencrypted public key list against the
// records that came out of the ciphertext. Version 2 binds the list into the
// AEAD, so this is a second line there; version 1 bound nothing, so it is the
// only line.
func checkPubKeyList(listed []KeyPair, held []plaintextKey) error {
	if len(listed) != len(held) {
		return fmt.Errorf("%w: %d listed, %d held", ErrPubKeyList, len(listed), len(held))
	}
	for i, k := range held {
		switch l := listed[i]; {
		case l.Address != k.Address:
			return fmt.Errorf("%w: entry %d lists %s, holds %s", ErrPubKeyList, i, l.Address, k.Address)
		case !bytes.Equal(l.PublicKey, k.PublicKey):
			return fmt.Errorf("%w: entry %d lists another public key for %s", ErrPubKeyList, i, k.Address)
		case l.Index != k.Index:
			return fmt.Errorf("%w: entry %d lists index %d, holds %d", ErrPubKeyList, i, l.Index, k.Index)
		}
	}
	return nil
}

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
// except the ciphertext, canonicalised by marshalling the Keystore value.
//
// The bound bytes are therefore a function of this struct's definition, and
// that is a constraint on every later release, not a convenience. Add a field
// to Keystore and the encoding changes, so a build carrying the new field
// computes different additional data for a file written without it and every
// version 2 keystore in existence stops opening. The field set of version 2
// is frozen: a new header field means a new format version with its own
// reader, and keeping the old one able to open old files.
//
// Two tests hold that line. TestHeaderAADIsFrozenForVersion2 pins the exact
// bytes for a known header, and TestKeystoreFieldSetIsFrozen fails on any
// change to the struct, so the decision is forced at the point of the edit
// rather than discovered by an integrator whose keys have become unreadable.
// load refuses unknown fields for the same reason: what is bound is the
// canonical re-encoding of this field set, so a file carrying anything else
// is not a file this reader can honestly authenticate.
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
		// Not reachable while kdfName returns one of the two constants and
		// the comparison above has passed. Go needs the arm, and an
		// unreachable arm that returns an error is the right thing to put
		// in it.
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
