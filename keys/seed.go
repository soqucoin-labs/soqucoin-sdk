package keys

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
)

// SeedSize is the length of the seed FIPS 204 KeyGen takes; FromSeed takes
// the same one.
const SeedSize = mldsa44.SeedSize

// MinMasterSize is the shortest master secret DeriveSeed accepts. A seed is
// the private key, so the master that produces every seed needs at least the
// entropy of one seed.
const MinMasterSize = 32

// seedDomain separates SDK deposit-key seeds from any other value an
// integrator derives from the same master secret. Changing it changes every
// derived address; it is part of the scheme, not a tunable.
const seedDomain = "soqucoin-sdk/keys/seed/v1"

// ErrShortMaster marks a master secret shorter than MinMasterSize bytes.
var ErrShortMaster = errors.New("keys: master secret is shorter than 32 bytes")

// FromSeed derives the ML-DSA-44 keypair FIPS 204 KeyGen produces from seed,
// and its bech32m address for hrp ("sq" mainnet, "ssq" stagenet). The
// derivation is the standard's own, so any conforming ML-DSA-44
// implementation given the same seed produces the same key; the addresses
// are pinned to the node's encoder in the tests.
//
// About 1 seed in 256 produces a public key whose first byte is 0xFF, the
// node's invalid-key marker: its address receives but nothing can spend
// from it. FromSeed returns ErrInvalidPublicKey for such a seed and derives
// nothing else, because a deterministic scheme has no next draw. Record the
// index as skipped and derive the next one (see DeriveSeed).
//
// The seed is the private key. FromSeed zeroes its own copy before returning
// and keeps nothing; zero the caller's copy when the KeyPair is no longer
// needed.
func FromSeed(hrp string, seed [SeedSize]byte) (*KeyPair, error) {
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	pk, sk := mldsa44.NewKeyFromSeed(&seed)
	pkBytes := pk.Bytes()
	addr, err := AddressFor(hrp, pkBytes)
	if err != nil {
		return nil, err
	}
	return &KeyPair{
		PrivateKey: sk.Bytes(),
		PublicKey:  pkBytes,
		Address:    addr,
	}, nil
}

// DeriveSeed returns the FromSeed seed for one derivation index under a
// master secret:
//
//	HMAC-SHA256(key = master, message = "soqucoin-sdk/keys/seed/v1" || index as 4 bytes big-endian)
//
// One master secret held in your key-management system therefore yields one
// key and one address per index, and the master plus the index a user was
// given are the only recovery material. When FromSeed refuses the seed for an
// index (ErrInvalidPublicKey), give the user the next index and record it;
// the refused index stays unused.
//
// The master must be at least MinMasterSize bytes of secret random data.
// DeriveSeed refuses a shorter one (ErrShortMaster) because every address
// derived from a guessable master is spendable by whoever guesses it.
func DeriveSeed(master []byte, index uint32) ([SeedSize]byte, error) {
	var seed [SeedSize]byte
	if len(master) < MinMasterSize {
		return seed, ErrShortMaster
	}
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(seedDomain))
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	mac.Write(idx[:])
	copy(seed[:], mac.Sum(nil))
	return seed, nil
}
