// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
)

// The node's vector: src/test/lightning_script_tests.cpp, b1_script_vectors,
// at soqucoin a9692212d. The hash is 32 bytes of 0xab, the payee's key hash
// 32 of 0x55, the payer's 32 of 0x66, the lock height 500, and the node
// asserts the script's hex with BOOST_CHECK_EQUAL.
const nodeVectorHex = "6382012088a820abababababababababababababababababababababababababababababababab88205555555555555555555555555555555555555555555555555555555555555555b6516702f401b175206666666666666666666666666666666666666666666666666666666666666666b65168"

func nodeVectorParams() Params {
	var p Params
	for i := range p.Hash {
		p.Hash[i], p.PayeeKeyHash[i], p.PayerKeyHash[i] = 0xab, 0x55, 0x66
	}
	p.LockHeight = 500
	return p
}

func mustScript(t *testing.T, p Params) []byte {
	t.Helper()
	s, err := p.Script()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScriptIsTheNodeVector(t *testing.T) {
	want, err := hex.DecodeString(nodeVectorHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 117 {
		t.Fatalf("the node's vector is %d bytes, want 117", len(want))
	}
	got := mustScript(t, nodeVectorParams())
	if !bytes.Equal(got, want) {
		t.Fatalf("script differs from the node's vector\nSDK  %x\nnode %x", got, want)
	}
	parsed, err := ParseScript(want)
	if err != nil {
		t.Fatalf("ParseScript refuses the node's vector: %v", err)
	}
	if parsed != nodeVectorParams() {
		t.Errorf("ParseScript read %+v from the node's vector", parsed)
	}
}

// The lock height is a script number of one to four bytes, written as a data
// push. The node asserts the template at one height, 500, so the bytes below
// are pinned by this SDK alone until the node carries a vectors file for
// them: each row is the push as the node's CScriptNum serializer writes the
// number (little-endian, a zero byte added when the top bit of the last byte
// is set), worked out by hand. The heights are the edges of each length, the
// two sides of the OP_1 to OP_16 range that another builder would write as
// one opcode, and the two sides of 65,536, where the three-byte form stops
// carrying the added zero.
func TestLockHeightPushAtTheBoundaryHeights(t *testing.T) {
	cases := []struct {
		height Height
		push   string
	}{
		{1, "0101"},
		{16, "0110"},
		{17, "0111"},
		{127, "017f"},
		{128, "028000"},
		{255, "02ff00"},
		{256, "020001"},
		{32767, "02ff7f"},
		{32768, "03008000"},
		{65535, "03ffff00"},
		{65536, "03000001"},
		{8388607, "03ffff7f"},
		{8388608, "0400008000"},
		{499999999, "04ff64cd1d"},
	}
	for _, c := range cases {
		p := nodeVectorParams()
		p.LockHeight = c.height
		script := mustScript(t, p)
		push, _ := hex.DecodeString(c.push)
		if len(script) != 115+len(push)-1 {
			t.Errorf("height %d: script of %d bytes, want %d", c.height, len(script), 115+len(push)-1)
			continue
		}
		if got := script[76 : 76+len(push)]; !bytes.Equal(got, push) {
			t.Errorf("height %d: push %x, want %s", c.height, got, c.push)
		}
		if script[76+len(push)] != 0xb1 {
			t.Errorf("height %d: byte after the push is %#02x, want OP_CHECKLOCKTIMEVERIFY", c.height, script[76+len(push)])
		}
		parsed, err := ParseScript(script)
		if err != nil || parsed != p {
			t.Errorf("height %d: ParseScript gave %+v, %v", c.height, parsed, err)
		}
	}
}

func TestValidateHoldsTheLockHeightToABlockHeight(t *testing.T) {
	for _, h := range []Height{1, 2, 500, MaxLockHeight} {
		p := nodeVectorParams()
		p.LockHeight = h
		if err := p.Validate(); err != nil {
			t.Errorf("height %d refused: %v", h, err)
		}
	}
	for _, h := range []Height{0, MaxLockHeight + 1, 500_000_001, 1 << 31, 1<<32 - 1} {
		p := nodeVectorParams()
		p.LockHeight = h
		if err := p.Validate(); !errors.Is(err, ErrLockHeight) {
			t.Errorf("height %d: Validate gave %v, want ErrLockHeight", h, err)
		}
		if s, err := p.Script(); !errors.Is(err, ErrLockHeight) || s != nil {
			t.Errorf("height %d: Script gave %x, %v", h, s, err)
		}
		if s, err := p.OutputScript(); !errors.Is(err, ErrLockHeight) || s != nil {
			t.Errorf("height %d: OutputScript gave %x, %v", h, s, err)
		}
		if a, err := p.Address("sq"); !errors.Is(err, ErrLockHeight) || a != "" {
			t.Errorf("height %d: Address gave %q, %v", h, a, err)
		}
	}
}

// splice returns script with the bytes from..to replaced by with.
func splice(script []byte, from, to int, with ...byte) []byte {
	out := append([]byte(nil), script[:from]...)
	out = append(out, with...)
	return append(out, script[to:]...)
}

// Each script is a contract another builder could write, or the template with
// one thing changed. Each is another script with another output, which this
// package does not build and does not spend. Some of them a node spends on
// the swap's own terms: the lock height as OP_5, OP_NOTIF for OP_IF and the
// two branches exchanged.
func TestParseScriptRefusesWhatIsNotTheTemplate(t *testing.T) {
	v, _ := hex.DecodeString(nodeVectorHex)
	low := nodeVectorParams()
	low.LockHeight = 5
	five := mustScript(t, low) // lock height push 01 05 at offset 76

	cases := map[string][]byte{
		"nil":                                nil,
		"empty":                              {},
		"one byte short":                     v[:len(v)-1],
		"one trailing byte":                  append(append([]byte(nil), v...), 0x00),
		"OP_HASH160 for the hashlock":        splice(v, 5, 6, 0xa9),
		"OP_HASH256 for the hashlock":        splice(v, 5, 6, 0xaa),
		"OP_CHECKSIG on the claim branch":    splice(v, 73, 74, 0xac),
		"OP_CHECKSIG on the refund branch":   splice(v, 114, 115, 0xac),
		"a relative lock":                    splice(v, 79, 80, 0xb2),
		"no length check on the secret":      splice(v, 1, 5),
		"a length check of 33":               splice(v, 3, 4, 0x21),
		"OP_NOTIF for OP_IF":                 splice(v, 0, 1, 0x64),
		"OP_0 for the final OP_1":            splice(v, 115, 116, 0x00),
		"a 20-byte hash":                     splice(v, 6, 39, append([]byte{0x14}, make([]byte, 20)...)...),
		"a key in place of the key hash":     splice(v, 40, 73, append([]byte{0x4d, 0x20, 0x05}, make([]byte, 1312)...)...),
		"the lock height as OP_5":            splice(five, 76, 78, 0x55),
		"the lock height with a spare zero":  splice(five, 76, 78, 0x02, 0x05, 0x00),
		"the lock height negative":           splice(five, 76, 78, 0x01, 0x85),
		"the lock height zero":               splice(five, 76, 78, 0x01, 0x00),
		"the lock height as an empty push":   splice(five, 76, 78, 0x00),
		"a three-byte height, no added zero": splice(v, 76, 79, 0x02, 0x00, 0x80),
		"a negative three-byte height":       splice(v, 76, 79, 0x03, 0x00, 0x80, 0x80),
		"a time for the lock":                splice(v, 76, 79, 0x04, 0x00, 0x65, 0xcd, 0x1d),
		"a five-byte lock":                   splice(v, 76, 79, 0x05, 0x00, 0x65, 0xcd, 0x1d, 0x00),
		"a four-byte lock with the sign set": splice(v, 76, 79, 0x04, 0x00, 0x65, 0xcd, 0x9d),
		"the branches exchanged": append(append(append([]byte{0x63},
			v[76:116]...), 0x67), append(append([]byte(nil), v[1:75]...), 0x68)...),
	}
	for name, script := range cases {
		p, err := ParseScript(script)
		if !errors.Is(err, ErrNotTemplate) {
			t.Errorf("%s: ParseScript gave %v, want ErrNotTemplate", name, err)
		}
		if p != (Params{}) {
			t.Errorf("%s: ParseScript returned parameters with its refusal: %+v", name, p)
		}
	}
}

// Every single-byte change of the node's vector is either refused, or is the
// template of other parameters: the script built again from what ParseScript
// returned is the changed script. No change at a byte that is not part of a
// parameter is accepted.
func TestParseScriptOnEverySingleByteChangeOfTheVector(t *testing.T) {
	v, _ := hex.DecodeString(nodeVectorHex)
	parameter := func(i int) bool {
		return (i >= 7 && i < 39) || (i >= 41 && i < 73) || (i >= 77 && i < 79) || (i >= 82 && i < 114)
	}
	accepted := 0
	for i := range v {
		for b := 0; b < 256; b++ {
			if byte(b) == v[i] {
				continue
			}
			changed := append([]byte(nil), v...)
			changed[i] = byte(b)
			p, err := ParseScript(changed)
			if err != nil {
				if !errors.Is(err, ErrNotTemplate) {
					t.Fatalf("offset %d, byte %#02x: error %v does not wrap ErrNotTemplate", i, b, err)
				}
				if parameter(i) && (i < 77 || i >= 79) {
					t.Errorf("offset %d, byte %#02x: a change inside a 32-byte parameter is refused: %v", i, b, err)
				}
				continue
			}
			accepted++
			if !parameter(i) {
				t.Errorf("offset %d, byte %#02x: accepted a change outside the parameters", i, b)
			}
			if !bytes.Equal(mustScript(t, p), changed) {
				t.Errorf("offset %d, byte %#02x: the parameters returned do not build the script given", i, b)
			}
			if p == nodeVectorParams() {
				t.Errorf("offset %d, byte %#02x: a changed script gave the vector's parameters", i, b)
			}
		}
	}
	// 96 parameter bytes take any value: 96 x 255. Of the two height bytes
	// (f4 01), the low byte takes any other value (255) and the high byte any
	// value from 00 to 7f but its own (127): f4 00 is 244, whose one byte has
	// the top bit set and so carries the added zero, and 80 up sets the sign.
	if want := 96*255 + 255 + 127; accepted != want {
		t.Errorf("%d single-byte changes accepted, want %d", accepted, want)
	}
}

func TestOutputScriptAndAddressCommitToTheScript(t *testing.T) {
	p := nodeVectorParams()
	script := mustScript(t, p)
	program := sha256.Sum256(script)

	out, err := p.OutputScript()
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{0x56, 0x20}, program[:]...); !bytes.Equal(out, want) {
		t.Errorf("output script %x, want OP_6 and the script's SHA-256 %x", out, want)
	}
	viaTx, _ := tx.ScriptWitnessV6(program[:])
	if !bytes.Equal(out, viaTx) {
		t.Errorf("output script %x differs from tx.ScriptWitnessV6 %x", out, viaTx)
	}

	for _, hrp := range []string{"sq", "ssq"} {
		addr, err := p.Address(hrp)
		if err != nil {
			t.Fatalf("%s: %v", hrp, err)
		}
		decoded, err := address.DecodeWitnessV6(hrp, addr)
		if err != nil || !bytes.Equal(decoded, program[:]) {
			t.Errorf("%s: address %s decodes to %x, %v; want the program %x", hrp, addr, decoded, err, program)
		}
		// The address is not a payment address: no function that takes one
		// accepts it.
		if _, err := address.ScriptFor(addr); err == nil {
			t.Errorf("%s: address.ScriptFor accepts the contract's address %s", hrp, addr)
		}
	}
	if a, err := p.Address("bc"); err == nil {
		t.Errorf("Address under a prefix of no network gave %q", a)
	}

	// One changed parameter is another output.
	q := p
	q.LockHeight++
	other, _ := q.OutputScript()
	if bytes.Equal(out, other) {
		t.Error("two lock heights give one output script")
	}
}

func TestKeyHashTakesTheRawKeyOnly(t *testing.T) {
	key := bytes.Repeat([]byte{0x07}, 1312)
	got, err := KeyHash(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != sha256.Sum256(key) {
		t.Errorf("KeyHash %x is not the SHA-256 of the key", got)
	}
	for _, n := range []int{0, 32, 1311, 1313, 2420} {
		if h, err := KeyHash(make([]byte, n)); !errors.Is(err, ErrKeySize) || h != ([32]byte{}) {
			t.Errorf("a key of %d bytes gave %x, %v; want ErrKeySize", n, h, err)
		}
	}
	if _, err := KeyHash(nil); !errors.Is(err, ErrKeySize) {
		t.Errorf("a nil key gave %v", err)
	}
	// The node reads a first byte of 0xff as its invalid-key marker and
	// refuses every spend under such a key (src/pubkey.h, CPubKey::GetLen).
	marker := bytes.Repeat([]byte{0x07}, 1312)
	marker[0] = 0xff
	if h, err := KeyHash(marker); !errors.Is(err, keys.ErrInvalidPublicKey) || h != ([32]byte{}) {
		t.Errorf("a key that begins 0xff gave %x, %v; want keys.ErrInvalidPublicKey", h, err)
	}
	marker[0] = 0xfe
	if _, err := KeyHash(marker); err != nil {
		t.Errorf("a key that begins 0xfe: %v", err)
	}
}
