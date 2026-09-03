package address

import (
	"errors"
	"strings"
	"testing"
)

// Witness-version pin (bead wallet-sdk-witness-version-pin-u38l).
//
// Every Soqucoin witness version shares the same HRP. A version with no active
// consensus rule is anyone-can-spend under BIP-141, so decoding such an address
// and building a scriptPubKey for it is a loss-of-funds path: the payment
// confirms and any observer sweeps it. The mining pool already pins this
// (soqupool-server/bitcoin/soqucoin.go, bead gp9); these tests pin it here, on
// the exchange-facing path.
//
// The v2 case is the one that motivated this: PAT commits to signatures rather
// than verifying them, so a witness-v2 output authorizes nothing at all
// (bead pat-v2-anyone-can-spend-ae6u).

const testHRP = "sq"

// craft builds a real, checksum-valid bech32m address at an arbitrary witness
// version — exactly what an attacker or a buggy tool would hand us. Encode()
// is deliberately used because it is the honest way to produce one.
func craft(t *testing.T, witVer byte) string {
	t.Helper()
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = byte(i)
	}
	addr, err := Encode(testHRP, witVer, prog)
	if err != nil {
		t.Fatalf("crafting a v%d address failed: %v", witVer, err)
	}
	return addr
}

// The headline case: a well-formed v2 address must be refused, not decoded.
func TestDecodeRejectsWitnessV2(t *testing.T) {
	addr := craft(t, 2)

	_, _, err := Decode(testHRP, addr)
	if err == nil {
		t.Fatalf("Decode ACCEPTED a witness-v2 address (%s). Paying it creates an "+
			"anyone-can-spend output — see bead pat-v2-anyone-can-spend-ae6u.", addr)
	}
	if !errors.Is(err, ErrUnsupportedWitnessVersion) {
		t.Errorf("wrong error for v2: got %v, want ErrUnsupportedWitnessVersion", err)
	}
	// The operator has to be able to tell WHY from the message.
	if !strings.Contains(err.Error(), "v2") {
		t.Errorf("error should name the offending version: %v", err)
	}
}

// Every version outside the allowlist is refused, not just v2.
func TestDecodeRejectsAllUnsupportedVersions(t *testing.T) {
	for v := byte(0); v <= 16; v++ {
		addr := craft(t, v)
		_, _, err := Decode(testHRP, addr)

		if IsSupportedWitnessVersion(v) {
			if err != nil {
				t.Errorf("v%d is supported but Decode rejected it: %v", v, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("v%d is NOT supported but Decode accepted it — loss-of-funds path", v)
		} else if !errors.Is(err, ErrUnsupportedWitnessVersion) {
			t.Errorf("v%d: got %v, want ErrUnsupportedWitnessVersion", v, err)
		}
	}
}

// The supported set is exactly v1. This is a drift detector: adding a version
// here without the node decoding it as an address AND a consensus rule that
// requires real authorization is how the v2 hazard got created, and v5/v7 sat
// here until the 2026-09-03 review showed the node's DecodeDestination accepts
// only v1 (a v5 destination is anyone-can-spend where USDSOQ is unscheduled).
func TestSupportedSetIsExactlyV1(t *testing.T) {
	want := map[byte]bool{1: true}
	for v := byte(0); v <= 16; v++ {
		if IsSupportedWitnessVersion(v) != want[v] {
			t.Errorf("v%d: supported=%v, want %v. Do NOT widen this set unless the node "+
				"decodes that version as an address and its consensus rule makes its "+
				"outputs require a real authorization to spend.", v, IsSupportedWitnessVersion(v), want[v])
		}
	}
}

// v5 and v7 are USDSOQ script forms, not destinations. A user-supplied
// address at either version must be refused everywhere an address is turned
// into a scriptPubKey, matching the node (src/utiladdress.cpp).
func TestDecodeRejectsUSDSOQVersionsAsDestinations(t *testing.T) {
	for _, v := range []byte{5, 7} {
		addr := craft(t, v)
		if _, _, err := Decode(testHRP, addr); !errors.Is(err, ErrUnsupportedWitnessVersion) {
			t.Errorf("v%d accepted as a destination (%s): got %v", v, addr, err)
		}
		if _, err := ScriptFor(addr); err == nil {
			t.Errorf("ScriptFor built a script for a v%d address", v)
		}
	}
}

// The node accepts exactly 32-byte programs. Well-formed v1 addresses at other
// BIP-141-legal lengths are not Soqucoin addresses: relay rejects the output as
// non-standard and a miner accepting it would burn the funds.
func TestDecodeRequiresExactly32ByteProgram(t *testing.T) {
	for _, n := range []int{2, 20, 31, 33, 40} {
		prog := make([]byte, n)
		for i := range prog {
			prog[i] = 0xab
		}
		addr, err := Encode(testHRP, 1, prog)
		if err != nil {
			t.Fatalf("encode %d-byte program: %v", n, err)
		}
		_, _, err = Decode(testHRP, addr)
		if !errors.Is(err, ErrInvalidLength) {
			t.Errorf("%d-byte v1 program accepted (%s): got %v, want ErrInvalidLength", n, addr, err)
		}
		if got := WitnessProgram(1, prog); got != nil {
			t.Errorf("WitnessProgram built a script for a %d-byte program", n)
		}
	}
	// Negative vectors recorded in the 2026-09-03 review (node verdict INVALID).
	for _, addr := range []string{
		"sq1p4w46h2at4w46h2at4w46h2at4w46h2atl52wv6", // v1, 20-byte program
		"sq1p4w4s2vnvrf", // v1, 2-byte program
	} {
		if _, _, err := Decode(testHRP, addr); err == nil {
			t.Errorf("node-invalid address accepted: %s", addr)
		}
	}
}

// Encode must not produce strings that Decode (and the node) cannot read.
func TestEncodeHygiene(t *testing.T) {
	prog := make([]byte, 32)
	if _, err := Encode(testHRP, 32, prog); !errors.Is(err, ErrInvalidVersion) {
		t.Errorf("Encode with witness version 32: got %v, want ErrInvalidVersion (it used to panic)", err)
	}
	addr, err := Encode("SQ", 1, prog)
	if err != nil {
		t.Fatalf("Encode upper-case HRP: %v", err)
	}
	if _, _, err := Decode(testHRP, addr); err != nil {
		t.Errorf("Encode with an upper-case HRP produced an undecodable address %s: %v", addr, err)
	}
	long := addr + strings.Repeat("q", MaxAddressLength)
	if _, _, err := Decode(testHRP, long); !errors.Is(err, ErrInvalidLength) {
		t.Errorf("over-long address: got %v, want ErrInvalidLength", err)
	}
}

// Defence in depth: even called directly, the script builder refuses.
func TestWitnessProgramRefusesUnsupportedVersion(t *testing.T) {
	prog := make([]byte, 32)

	if got := WitnessProgram(2, prog); got != nil {
		t.Errorf("WitnessProgram built a v2 scriptPubKey (%x) — it must refuse", got)
	}
	if got := WitnessProgram(1, prog); got == nil {
		t.Error("WitnessProgram refused v1, which is the primary spendable form")
	} else if got[0] != 0x51 {
		t.Errorf("v1 script opcode = %#x, want 0x51 (OP_1)", got[0])
	}
}
