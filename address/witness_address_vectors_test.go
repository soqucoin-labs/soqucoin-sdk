// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// The witness address vectors shared with the node.
//
// testdata/witness_address_vectors.json is a byte-for-byte copy of
// src/test/data/witness_address_vectors.json in the soqucoin repository at
// commit ed615ae9333275a05b392b16e95c8f2d9039757c. Its generator is
// gen_witness_address_vectors.py in the same directory there. The copy is
// replaced as a whole when the node's file changes and is never edited here:
// vectorsSHA256 is the digest of the copy, and loadVectors refuses a file with
// another digest.
//
// The file holds two address forms. Version 1 is the payment address this
// package has always decoded; its vectors run against the existing functions
// and pass with or without the version 6 form in the tree. Version 6 is the
// script-hash form; its vectors run against EncodeWitnessV6 and
// DecodeWitnessV6.

package address

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const (
	vectorsFile   = "testdata/witness_address_vectors.json"
	vectorsSHA256 = "8a1666fbdf4f8675aa34a6a54125de949b6bba2ebb7de49fffc7514b49fa4c4b"
)

type validVector struct {
	Prefix  string `json:"prefix"`
	Version byte   `json:"version"`
	Program string `json:"program"`
	Address string `json:"address"`
}

type invalidVector struct {
	Prefix  string `json:"prefix"`
	Version byte   `json:"version"`
	Address string `json:"address"`
	Reason  string `json:"reason"`
}

type vectorSet struct {
	Description []string          `json:"description"`
	Networks    map[string]string `json:"networks"`
	Valid       []validVector     `json:"valid"`
	Invalid     []invalidVector   `json:"invalid"`
}

func (v validVector) program(t *testing.T) []byte {
	t.Helper()
	p, err := hex.DecodeString(v.Program)
	if err != nil {
		t.Fatalf("vector %s: program is not hex: %v", v.Address, err)
	}
	return p
}

func loadVectors(t *testing.T) vectorSet {
	t.Helper()
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatalf("read the vectors: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != vectorsSHA256 {
		t.Fatalf("%s has SHA-256 %s, want %s: the file is a copy of the node's and is replaced as a whole",
			vectorsFile, got, vectorsSHA256)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var set vectorSet
	if err := dec.Decode(&set); err != nil {
		t.Fatalf("parse the vectors: %v", err)
	}
	return set
}

// reasonClass maps each refusal reason of the vectors to the error
// DecodeWitnessV6 returns for it. The reasons are the first check a string
// fails, in the order the file's description lists them, so a decoder that
// applies its checks in another order returns another class for some vector.
var reasonClass = map[string]error{
	"empty":             ErrInvalidLength,
	"whitespace":        ErrInvalidChar,
	"mixed_case":        ErrInvalidChar,
	"length_over_90":    ErrInvalidLength,
	"separator_missing": ErrInvalidHRP,
	"prefix_empty":      ErrInvalidHRP,
	"checksum_short":    ErrInvalidLength,
	"character":         ErrInvalidChar,
	"checksum_bech32":   ErrInvalidChecksum,
	"checksum":          ErrInvalidChecksum,
	"prefix":            ErrInvalidHRP,
	"version":           ErrUnsupportedWitnessVersion,
	"padding":           ErrInvalidPadding,
	"program_length":    ErrInvalidLength,
}

// script is the output script the vectors' description gives for a program:
// the version opcode, the push of 32 bytes, the program.
func script(opcode byte, program []byte) []byte {
	return append([]byte{opcode, 0x20}, program...)
}

func TestWitnessAddressVectorsFile(t *testing.T) {
	set := loadVectors(t)

	if len(set.Valid) != 32 || len(set.Invalid) != 406 {
		t.Fatalf("valid = %d, invalid = %d, want 32 and 406", len(set.Valid), len(set.Invalid))
	}

	// The prefixes the file names are the prefixes of this SDK's networks.
	// The node's testnet has no network type here; it shares mainnet's prefix.
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		if got := set.Networks[n.ChainID]; got != n.HRP {
			t.Errorf("network %q: the vectors say prefix %q, the SDK says %q", n.ChainID, got, n.HRP)
		}
	}
	if len(set.Networks) != 4 || set.Networks["test"] != types.Mainnet.HRP {
		t.Errorf("networks = %v, want main, test, regtest and stagenet, with test on mainnet's prefix", set.Networks)
	}

	type form struct {
		prefix  string
		version byte
	}
	valid := map[form]int{}
	for _, v := range set.Valid {
		valid[form{v.Prefix, v.Version}]++
		if len(v.program(t)) != WitnessProgramSize {
			t.Errorf("valid vector %s: program is %d bytes", v.Address, len(v.program(t)))
		}
	}
	reasons := map[form]map[string]int{}
	for _, v := range set.Invalid {
		if _, known := reasonClass[v.Reason]; !known {
			t.Errorf("invalid vector %q: reason %q has no error class here", v.Address, v.Reason)
		}
		f := form{v.Prefix, v.Version}
		if reasons[f] == nil {
			reasons[f] = map[string]int{}
		}
		reasons[f][v.Reason]++
	}
	for _, prefix := range []string{types.Mainnet.HRP, types.Stagenet.HRP} {
		for _, version := range []byte{1, 6} {
			f := form{prefix, version}
			if valid[f] != 8 {
				t.Errorf("%s version %d: %d valid vectors, want 8", prefix, version, valid[f])
			}
			for reason := range reasonClass {
				if reasons[f][reason] == 0 {
					t.Errorf("%s version %d: no invalid vector with reason %q", prefix, version, reason)
				}
			}
		}
	}
	if len(valid) != 4 || len(reasons) != 4 {
		t.Errorf("the vectors hold %d valid and %d invalid forms, want 4 and 4 (two prefixes, versions 1 and 6)",
			len(valid), len(reasons))
	}
}

func TestWitnessV6VectorsEncodeAndDecode(t *testing.T) {
	for _, v := range loadVectors(t).Valid {
		if v.Version != 6 {
			continue
		}
		program := v.program(t)

		got, err := EncodeWitnessV6(v.Prefix, program)
		if err != nil || got != v.Address {
			t.Errorf("EncodeWitnessV6(%q, %x) = %q, %v; want %q", v.Prefix, program, got, err, v.Address)
		}
		for _, written := range []string{v.Address, strings.ToUpper(v.Address)} {
			decoded, err := DecodeWitnessV6(v.Prefix, written)
			if err != nil || !bytes.Equal(decoded, program) {
				t.Errorf("DecodeWitnessV6(%q, %q) = %x, %v; want %x", v.Prefix, written, decoded, err, program)
			}
		}
	}
}

func TestWitnessV6DecoderRefusesInvalidVectors(t *testing.T) {
	for _, v := range loadVectors(t).Invalid {
		if v.Version != 6 {
			continue
		}
		program, err := DecodeWitnessV6(v.Prefix, v.Address)
		if program != nil {
			t.Errorf("%s vector %q under %q: decoded to %x", v.Reason, v.Address, v.Prefix, program)
		}
		if want := reasonClass[v.Reason]; !errors.Is(err, want) {
			t.Errorf("%s vector %q under %q: error %v, want one that wraps %q", v.Reason, v.Address, v.Prefix, err, want)
		}
	}
}

// A version 1 address is not a version 6 address, and an address of one prefix
// is not an address of the other, whatever the program.
func TestWitnessV6DecoderRefusesVersion1AndOtherPrefixes(t *testing.T) {
	other := map[string]string{types.Mainnet.HRP: types.Stagenet.HRP, types.Stagenet.HRP: types.Mainnet.HRP}
	for _, v := range loadVectors(t).Valid {
		for _, written := range []string{v.Address, strings.ToUpper(v.Address)} {
			if v.Version == 1 {
				if program, err := DecodeWitnessV6(v.Prefix, written); program != nil || !errors.Is(err, ErrUnsupportedWitnessVersion) {
					t.Errorf("version 1 address %q: DecodeWitnessV6 = %x, %v; want ErrUnsupportedWitnessVersion", written, program, err)
				}
				continue
			}
			if program, err := DecodeWitnessV6(other[v.Prefix], written); program != nil || !errors.Is(err, ErrInvalidHRP) {
				t.Errorf("%q under prefix %q: DecodeWitnessV6 = %x, %v; want ErrInvalidHRP", written, other[v.Prefix], program, err)
			}
		}
	}
}

// The prefix argument is one of this SDK's networks' prefixes, written as the
// network writes it. A prefix of no network carries a valid checksum as easily
// as a real one, so the codec refuses it in both directions.
func TestWitnessV6RefusesAPrefixOfNoNetwork(t *testing.T) {
	program := progOf(0x5a)
	for _, hrp := range []string{"", "bc", "tb", "s", "sqq", "soq", "SQ", "Ssq", " sq"} {
		if got, err := EncodeWitnessV6(hrp, program); got != "" || !errors.Is(err, ErrInvalidHRP) {
			t.Errorf("EncodeWitnessV6(%q) = %q, %v; want ErrInvalidHRP", hrp, got, err)
		}
		// A well-formed version 6 string under this prefix, from the generic encoder.
		written, err := Encode(hrp, 6, program)
		if err != nil {
			t.Fatalf("Encode(%q, 6): %v", hrp, err)
		}
		if got, err := DecodeWitnessV6(hrp, written); got != nil || !errors.Is(err, ErrInvalidHRP) {
			t.Errorf("DecodeWitnessV6(%q, %q) = %x, %v; want ErrInvalidHRP", hrp, written, got, err)
		}
	}
}

// An OP_6 output whose program is not 32 bytes can never be spent, so the
// encoder writes an address for 32 bytes and for no other length.
func TestWitnessV6EncoderTakes32BytesOnly(t *testing.T) {
	for _, hrp := range []string{types.Mainnet.HRP, types.Stagenet.HRP} {
		for size := 0; size <= 40; size++ {
			got, err := EncodeWitnessV6(hrp, make([]byte, size))
			if size == WitnessProgramSize {
				if err != nil || got == "" {
					t.Errorf("%s, 32 bytes: %q, %v", hrp, got, err)
				}
				continue
			}
			if got != "" || !errors.Is(err, ErrInvalidLength) {
				t.Errorf("%s, %d bytes: EncodeWitnessV6 = %q, %v; want ErrInvalidLength", hrp, size, got, err)
			}
		}
		if got, err := EncodeWitnessV6(hrp, nil); got != "" || !errors.Is(err, ErrInvalidLength) {
			t.Errorf("%s, nil program: EncodeWitnessV6 = %q, %v; want ErrInvalidLength", hrp, got, err)
		}
	}
}

// The existing version 1 path, against the same file. Every assertion here
// holds on the tree before the version 6 form exists and on the tree after:
// the payment address functions decode and build what the node does for
// version 1, and refuse every version 6 string, so nothing that sends funds to
// a user-supplied address can reach a version 6 output.
func TestVersion1PathAgainstTheVectors(t *testing.T) {
	set := loadVectors(t)

	if !IsSupportedWitnessVersion(1) || IsSupportedWitnessVersion(6) {
		t.Fatal("the payment-destination allowlist is version 1 and nothing else")
	}

	for _, v := range set.Valid {
		program := v.program(t)
		for _, written := range []string{v.Address, strings.ToUpper(v.Address)} {
			if v.Version == 6 {
				if _, _, err := Decode(v.Prefix, written); !errors.Is(err, ErrUnsupportedWitnessVersion) {
					t.Errorf("Decode(%q): %v, want ErrUnsupportedWitnessVersion", written, err)
				}
				if err := Validate(v.Prefix, written); !errors.Is(err, ErrUnsupportedWitnessVersion) {
					t.Errorf("Validate(%q): %v, want ErrUnsupportedWitnessVersion", written, err)
				}
				if spk, err := ScriptFor(written); spk != nil || !errors.Is(err, ErrUnsupportedWitnessVersion) {
					t.Errorf("ScriptFor(%q) = %x, %v; want ErrUnsupportedWitnessVersion", written, spk, err)
				}
				if sh, err := AddressToScriptHash(v.Prefix, written); sh != "" || !errors.Is(err, ErrUnsupportedWitnessVersion) {
					t.Errorf("AddressToScriptHash(%q) = %q, %v; want ErrUnsupportedWitnessVersion", written, sh, err)
				}
				continue
			}

			want := script(0x51, program)
			version, decoded, err := Decode(v.Prefix, written)
			if err != nil || version != 1 || !bytes.Equal(decoded, program) {
				t.Errorf("Decode(%q) = %d, %x, %v; want 1, %x", written, version, decoded, err, program)
			}
			if err := Validate(v.Prefix, written); err != nil {
				t.Errorf("Validate(%q): %v", written, err)
			}
			if spk, err := ScriptFor(written); err != nil || !bytes.Equal(spk, want) {
				t.Errorf("ScriptFor(%q) = %x, %v; want %x", written, spk, err, want)
			}
			if sh, err := AddressToScriptHash(v.Prefix, written); err != nil || sh != ScriptHash(want) {
				t.Errorf("AddressToScriptHash(%q) = %q, %v; want %q", written, sh, err, ScriptHash(want))
			}
		}

		if v.Version == 6 {
			if spk := WitnessProgram(6, program); spk != nil {
				t.Errorf("WitnessProgram(6, %x) = %x, want nil", program, spk)
			}
			continue
		}
		if got, err := Encode(v.Prefix, 1, program); err != nil || got != v.Address {
			t.Errorf("Encode(%q, 1, %x) = %q, %v; want %q", v.Prefix, program, got, err, v.Address)
		}
		if got, err := New(v.Prefix, 1, program); err != nil || got != v.Address {
			t.Errorf("New(%q, 1, %x) = %q, %v; want %q", v.Prefix, program, got, err, v.Address)
		}
		if spk := WitnessProgram(1, program); !bytes.Equal(spk, script(0x51, program)) {
			t.Errorf("WitnessProgram(1, %x) = %x", program, spk)
		}
	}

	for _, v := range set.Invalid {
		if v.Version != 1 {
			continue
		}
		if version, program, err := Decode(v.Prefix, v.Address); err == nil {
			t.Errorf("%s vector %q under %q: Decode = %d, %x", v.Reason, v.Address, v.Prefix, version, program)
		}
		if err := Validate(v.Prefix, v.Address); err == nil {
			t.Errorf("%s vector %q under %q: Validate accepted it", v.Reason, v.Address, v.Prefix)
		}
	}
}

// Whatever DecodeWitnessV6 accepts is the one string EncodeWitnessV6 writes
// for that program, in lower or in upper case, and is refused by the payment
// address decoder.
//
// The name does not begin with FuzzDecode: the Makefile's fuzz target selects
// the payment address target with that pattern, and go test refuses a pattern
// that matches two fuzz targets.
func FuzzWitnessV6Decode(f *testing.F) {
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		f.Fatalf("read the vectors: %v", err)
	}
	var set vectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		f.Fatalf("parse the vectors: %v", err)
	}
	for _, v := range set.Valid {
		f.Add(v.Prefix, v.Address)
	}
	for _, v := range set.Invalid {
		f.Add(v.Prefix, v.Address)
	}
	f.Fuzz(func(t *testing.T, hrp, s string) {
		program, err := DecodeWitnessV6(hrp, s)
		if err != nil {
			if program != nil {
				t.Fatalf("DecodeWitnessV6(%q, %q) returned a program with error %v", hrp, s, err)
			}
			return
		}
		if len(program) != WitnessProgramSize {
			t.Fatalf("DecodeWitnessV6(%q, %q) accepted a %d-byte program", hrp, s, len(program))
		}
		written, err := EncodeWitnessV6(hrp, program)
		if err != nil {
			t.Fatalf("EncodeWitnessV6(%q, %x): %v", hrp, program, err)
		}
		if s != written && s != strings.ToUpper(written) {
			t.Fatalf("DecodeWitnessV6(%q) accepted %q; the address of its program is %q", hrp, s, written)
		}
		if _, _, err := Decode(hrp, s); !errors.Is(err, ErrUnsupportedWitnessVersion) {
			t.Fatalf("Decode(%q, %q): %v, want ErrUnsupportedWitnessVersion", hrp, s, err)
		}
	})
}
