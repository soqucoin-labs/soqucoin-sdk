package address

import (
	"encoding/hex"
	"testing"
)

func TestDecodeValid(t *testing.T) {
	// Known Soqucoin stagenet addresses (from wallet output)
	tests := []struct {
		addr   string
		wantVer byte
		wantLen int
	}{
		{
			addr:    "ssq1pquy8q7skkgzd3lhqrdspxtz3ywfxgarwfv5ka2ek6lgaalcgedqq233q3x",
			wantVer: 1,
			wantLen: 32, // Dilithium witness v1 = 32-byte program
		},
		{
			addr:    "ssq1pc9p9w8gngpq9jfkev82t0t9ll0my06ldh3hkw950tp9sxtxw0w9s02jxqw",
			wantVer: 1,
			wantLen: 32,
		},
		{
			addr:    "ssq1p52zvzahc8dy4h8l0lu7hlzrjuzw2crq99j3pxc4uywevle67agls4c3ms8",
			wantVer: 1,
			wantLen: 32,
		},
	}

	for _, tt := range tests {
		t.Run(tt.addr[:20], func(t *testing.T) {
			ver, prog, err := Decode("ssq", tt.addr)
			if err != nil {
				t.Fatalf("Decode(%q) error: %v", tt.addr, err)
			}
			if ver != tt.wantVer {
				t.Errorf("witness version = %d, want %d", ver, tt.wantVer)
			}
			if len(prog) != tt.wantLen {
				t.Errorf("witness program length = %d, want %d", len(prog), tt.wantLen)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	// Decode a known address, re-encode, check match
	addr := "ssq1pquy8q7skkgzd3lhqrdspxtz3ywfxgarwfv5ka2ek6lgaalcgedqq233q3x"
	ver, prog, err := Decode("ssq", addr)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}

	encoded, err := Encode("ssq", ver, prog)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	if encoded != addr {
		t.Errorf("round-trip mismatch:\n  got:  %s\n  want: %s", encoded, addr)
	}
}

func TestWitnessProgram(t *testing.T) {
	// Witness v1 program: OP_1 (0x51) + push 32 + 32 bytes
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = byte(i)
	}

	spk := WitnessProgram(1, prog)
	if len(spk) != 34 {
		t.Fatalf("scriptPubKey length = %d, want 34", len(spk))
	}
	if spk[0] != 0x51 {
		t.Errorf("first byte = 0x%02x, want 0x51 (OP_1)", spk[0])
	}
	if spk[1] != 32 {
		t.Errorf("push length = %d, want 32", spk[1])
	}
}

func TestScriptHash(t *testing.T) {
	// ScriptHash should produce reversed SHA256 hex
	spk := []byte{0x51, 0x20, 0x00, 0x01, 0x02, 0x03}
	hash := ScriptHash(spk)
	if len(hash) != 64 {
		t.Errorf("script hash hex length = %d, want 64", len(hash))
	}
	// Verify it's valid hex
	_, err := hex.DecodeString(hash)
	if err != nil {
		t.Errorf("script hash is not valid hex: %v", err)
	}
}

func TestAddressToScriptHash(t *testing.T) {
	addr := "ssq1pquy8q7skkgzd3lhqrdspxtz3ywfxgarwfv5ka2ek6lgaalcgedqq233q3x"
	hash, err := AddressToScriptHash("ssq", addr)
	if err != nil {
		t.Fatalf("AddressToScriptHash error: %v", err)
	}
	if len(hash) != 64 {
		t.Errorf("script hash length = %d, want 64", len(hash))
	}
	t.Logf("Address: %s", addr[:20]+"...")
	t.Logf("Script hash: %s", hash)
}

func TestInvalidAddresses(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"wrong_hrp", "bc1pquy8q7skkgzd3lhqrdspxtz3ywfxgarwfv5ka2ek6lgaalcgedqq233q3x"},
		{"bad_checksum", "ssq1pquy8q7skkgzd3lhqrdspxtz3ywfxgarwfv5ka2ek6lgaalcgedqq233q3z"},
		{"too_short", "ssq1p"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Decode("ssq", tt.addr)
			if err == nil {
				t.Errorf("Decode(%q) should have failed", tt.addr)
			}
		})
	}
}
