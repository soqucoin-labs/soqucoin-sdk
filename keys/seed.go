package keys

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// SeedSize is the length of the seed FIPS 204 KeyGen takes; FromSeed takes
// the same one.
const SeedSize = mldsa44.SeedSize

// MinMasterSize is the shortest master secret DeriveSeed accepts. A seed is
// the private key, so the master that produces every seed needs at least the
// entropy of one seed.
const MinMasterSize = 32

// seedDomain separates SDK deposit-key seeds from any other value an
// integrator derives from the same master secret, and, with the HRP that
// follows it, one network's seeds from another's. Changing it changes every
// derived address; it is part of the scheme, not a tunable. v1 had no
// network in the message and was replaced before any integrator derived an
// address under it.
const seedDomain = "soqucoin-sdk/keys/seed/v2/"

var (
	// ErrShortMaster marks a master secret shorter than MinMasterSize bytes.
	ErrShortMaster = errors.New("keys: master secret is shorter than 32 bytes")

	// ErrUnknownHRP marks an address prefix that belongs to no Soqucoin
	// network. DeriveSeed refuses it rather than derive keys for a network
	// that does not exist.
	ErrUnknownHRP = errors.New("keys: address prefix belongs to no known network")
)

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
// The seed is the private key. FromSeed zeroes the copy it was passed and
// keeps no reference; the key object the ML-DSA library builds also carries
// the seed and is left to the garbage collector (docs/SECURITY.md, Memory
// hygiene). Zero the caller's copy when the KeyPair is no longer needed.
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
// master secret, for the network whose address prefix is hrp:
//
//	HMAC-SHA256(key = master, message = "soqucoin-sdk/keys/seed/v2/" || hrp || "/" || index as 4 bytes big-endian)
//
// One master secret held in your key-management system therefore yields one
// key and one address per network and index, and the master plus the index a
// user was given are the only recovery material. Pass the same hrp to
// FromSeed. When FromSeed refuses the seed for an index
// (ErrInvalidPublicKey), give the user the next index and record it; the
// refused index stays unused.
//
// The network is part of the message so that one master derives different
// keys on mainnet and stagenet: a production master that reaches a stagenet
// host yields that host stagenet keys only, never a key that also spends
// mainnet funds. Regtest shares the prefix "sq" with mainnet, so the binding
// gives a regtest host no separation; give it a master of its own. Nor is the
// binding a defence against the master itself leaking; whoever holds the
// master derives every key on every network.
//
// The master must be at least MinMasterSize bytes of secret random data.
// DeriveSeed refuses a shorter one (ErrShortMaster) because every address
// derived from a guessable master is spendable by whoever guesses it. An hrp
// that belongs to no Soqucoin network is refused (ErrUnknownHRP).
func DeriveSeed(master []byte, hrp string, index uint32) ([SeedSize]byte, error) {
	var seed [SeedSize]byte
	if len(master) < MinMasterSize {
		return seed, ErrShortMaster
	}
	if len(types.GenesisHashesForHRP(hrp)) == 0 {
		return seed, fmt.Errorf("%w: %q", ErrUnknownHRP, hrp)
	}
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(seedDomain))
	mac.Write([]byte(hrp))
	mac.Write([]byte{'/'})
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	mac.Write(idx[:])
	copy(seed[:], mac.Sum(nil))
	return seed, nil
}
