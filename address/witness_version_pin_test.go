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

// The supported set is exactly v1/v5/v7. This is a drift detector: adding a
// version here without a consensus rule that requires real authorization is
// how the v2 hazard got created in the first place.
func TestSupportedSetIsExactlyV1V5V7(t *testing.T) {
	want := map[byte]bool{1: true, 5: true, 7: true}
	for v := byte(0); v <= 16; v++ {
		if IsSupportedWitnessVersion(v) != want[v] {
			t.Errorf("v%d: supported=%v, want %v. Do NOT widen this set unless that "+
				"version's consensus rule makes its outputs require a real "+
				"authorization to spend.", v, IsSupportedWitnessVersion(v), want[v])
		}
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
