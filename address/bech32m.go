package address

// Bech32m encoder/decoder for Soqucoin segwit addresses (ssq1p...).
// Implements BIP-350 (bech32m for witness version >= 1).
//
// Usage:
//   scriptPubKey, err := ScriptFor("ssq1p...")   // network derived from the address
//   electrumHash := ScriptHash(scriptPubKey)
//
// Decode and WitnessProgram are the lower-level primitives behind ScriptFor and
// require the caller to know the HRP in advance. Prefer ScriptFor.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

var (
	ErrInvalidChecksum = errors.New("bech32m: invalid checksum")
	ErrInvalidLength   = errors.New("bech32m: invalid data length")
	ErrInvalidHRP      = errors.New("bech32m: invalid human-readable part")
	ErrInvalidChar     = errors.New("bech32m: invalid character")
	ErrInvalidVersion  = errors.New("bech32m: witness version out of range")

	// ErrUnsupportedWitnessVersion is returned for an address whose witness
	// version this chain does not spend. See supportedWitnessVersions.
	ErrUnsupportedWitnessVersion = errors.New("bech32m: unsupported witness version for Soqucoin")
)

// bech32m constant
const bech32mConst = 0x2bc830a3

// WitnessProgramSize is the only witness-program length the node accepts as a
// destination: every Soqucoin witness version is OP_N <32 bytes>, and the
// node's DecodeDestination (src/utiladdress.cpp) refuses anything else. An
// address at another length is therefore not a Soqucoin address at all, even
// though BIP-141 would allow 2..40 bytes; paying one produces a non-standard
// output that relay rejects and a miner accepting it would burn.
const WitnessProgramSize = 32

// MaxAddressLength matches the node's bech32 decoder limit (src/bech32.cpp).
const MaxAddressLength = 90

// supportedWitnessVersions is the allowlist of witness versions this SDK will
// decode as a Soqucoin address and build a scriptPubKey for.
//
// ⛔ WHY AN ALLOWLIST AND NOT A LENGTH CHECK. Every Soqucoin witness version
// shares the same HRP, so an HRP+length check alone accepts an address for a
// version this chain does not spend. Witness versions without an active
// consensus rule are anyone-can-spend under BIP-141, so building an output for
// one is a loss-of-funds path: the payment confirms and any observer may then
// sweep it. This is the same defence the mining pool already applies in
// soqupool-server/bitcoin/soqucoin.go (bead gp9); it belongs in every consumer
// that turns an address into a scriptPubKey, not just the pool.
//
//	v1 — Dilithium (ML-DSA-44) P2WPKH. The only form the node accepts as an
//	     address (src/utiladdress.cpp DecodeDestination: data[0]==1, 32 bytes).
//
// v5 (USDSOQ authority marker) and v7 (USDSOQ holding) are NOT payment
// destinations and are not in this set. The node does not decode them as
// addresses; on mainnet creating such an output is consensus-rejected while
// the USDSOQ deployment is not scheduled, and where it is active a v5 witness
// is anyone-can-spend until the rule applies and a v7 output's value is USDSOQ,
// so paying SOQ into it fails conservation. Consumers that build USDSOQ scripts
// use tx.ScriptWitnessV5 / tx.ScriptV7USDSOQHolding explicitly; nothing that
// starts from a user-supplied address may reach them (2026-09-03 review).
//
// ⛔ Do NOT add a version here because it "exists" or because an opcode for it
// is active. Add it only when the node's DecodeDestination accepts it AND its
// consensus rule makes outputs of that version require a real authorization to
// spend. Notably v2 (PAT attestation) must NOT be added: PAT commits to
// signatures rather than verifying them, so a v2 output authorizes nothing
// (bead pat-v2-anyone-can-spend-ae6u).
var supportedWitnessVersions = map[byte]bool{
	1: true,
}

// IsSupportedWitnessVersion reports whether this SDK will accept an address at
// the given witness version as a payment destination.
func IsSupportedWitnessVersion(witVer byte) bool {
	return supportedWitnessVersions[witVer]
}

var charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var charsetRev = func() [128]int8 {
	var rev [128]int8
	for i := range rev {
		rev[i] = -1
	}
	for i, c := range charset {
		rev[c] = int8(i)
	}
	return rev
}()

func polymod(values []int) int {
	gen := [5]int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []int {
	result := make([]int, 0, len(hrp)*2+1)
	for _, c := range hrp {
		result = append(result, int(c>>5))
	}
	result = append(result, 0)
	for _, c := range hrp {
		result = append(result, int(c&31))
	}
	return result
}

func verifyChecksum(hrp string, data []int) bool {
	values := append(hrpExpand(hrp), data...)
	return polymod(values) == bech32mConst
}

func createChecksum(hrp string, data []int) []int {
	values := append(hrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ bech32mConst
	result := make([]int, 6)
	for i := 0; i < 6; i++ {
		result[i] = (mod >> uint(5*(5-i))) & 31
	}
	return result
}

// Decode decodes a bech32m address string.
// Returns witness version (0-16) and witness program bytes.
func Decode(hrp, addr string) (byte, []byte, error) {
	if len(addr) > MaxAddressLength {
		return 0, nil, fmt.Errorf("%w: %d characters, limit %d", ErrInvalidLength, len(addr), MaxAddressLength)
	}
	addrLower := strings.ToLower(addr)
	if addrLower != addr && strings.ToUpper(addr) != addr {
		return 0, nil, ErrInvalidChar
	}
	addr = addrLower

	pos := strings.LastIndex(addr, "1")
	if pos < 1 || pos+7 > len(addr) {
		return 0, nil, ErrInvalidHRP
	}

	gotHRP := addr[:pos]
	if gotHRP != strings.ToLower(hrp) {
		return 0, nil, fmt.Errorf("%w: expected %s, got %s", ErrInvalidHRP, hrp, gotHRP)
	}

	dataStr := addr[pos+1:]
	data := make([]int, len(dataStr))
	for i, c := range dataStr {
		if c > 127 || charsetRev[c] == -1 {
			return 0, nil, ErrInvalidChar
		}
		data[i] = int(charsetRev[c])
	}

	if !verifyChecksum(gotHRP, data) {
		return 0, nil, ErrInvalidChecksum
	}

	// Strip checksum (last 6 chars)
	data = data[:len(data)-6]
	if len(data) < 1 {
		return 0, nil, ErrInvalidLength
	}

	witVer := byte(data[0])
	witProg, err := convertBits(data[1:], 5, 8, false)
	if err != nil {
		return 0, nil, err
	}

	// Reject witness versions this chain does not spend. A well-formed address
	// at an unsupported version is NOT a valid Soqucoin address: paying it
	// creates an anyone-can-spend or unspendable output. Fail here, at parse
	// time, so no caller can reach script construction with one.
	if !supportedWitnessVersions[witVer] {
		return 0, nil, fmt.Errorf("%w: v%d (only v1 Dilithium addresses are payment destinations)",
			ErrUnsupportedWitnessVersion, witVer)
	}

	// The node accepts exactly 32-byte programs (see WitnessProgramSize); the
	// BIP-141 2..40 range is wider than what this chain decodes as an address.
	if len(witProg) != WitnessProgramSize {
		return 0, nil, fmt.Errorf("%w: witness program length %d, want %d",
			ErrInvalidLength, len(witProg), WitnessProgramSize)
	}

	return witVer, witProg, nil
}

// Encode encodes a witness version and program to a bech32m string.
func Encode(hrp string, witVer byte, witProg []byte) (string, error) {
	if witVer > 16 {
		return "", fmt.Errorf("%w: v%d (max 16)", ErrInvalidVersion, witVer)
	}
	// The checksum is defined over the lowercase HRP and Decode (like the node)
	// rejects mixed case, so an upper-case HRP would encode an undecodable
	// string. Normalise instead of emitting one.
	hrp = strings.ToLower(hrp)
	// Convert []byte to []int for convertBits
	progInts := make([]int, len(witProg))
	for i, b := range witProg {
		progInts[i] = int(b)
	}
	convBytes, err := convertBits(progInts, 8, 5, true)
	if err != nil {
		return "", err
	}
	// Build data: witness version + converted program
	data := make([]int, 0, 1+len(convBytes))
	data = append(data, int(witVer))
	for _, b := range convBytes {
		data = append(data, int(b))
	}
	checksum := createChecksum(hrp, data)
	data = append(data, checksum...)

	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, d := range data {
		sb.WriteByte(charset[d])
	}
	return sb.String(), nil
}

// convertBits converts between bit groupings.
func convertBits(data []int, fromBits, toBits uint, pad bool) ([]byte, error) {
	acc := 0
	bits := uint(0)
	result := make([]byte, 0, len(data)*int(fromBits)/int(toBits)+1)
	maxv := (1 << toBits) - 1

	for _, d := range data {
		if d < 0 || d >= (1<<fromBits) {
			return nil, fmt.Errorf("invalid data value: %d", d)
		}
		acc = (acc << fromBits) | d
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			result = append(result, byte((acc>>bits)&maxv))
		}
	}

	if pad {
		if bits > 0 {
			result = append(result, byte((acc<<(toBits-bits))&maxv))
		}
	} else if bits >= fromBits || (acc<<(toBits-bits))&maxv != 0 {
		return nil, errors.New("invalid padding")
	}

	return result, nil
}

// WitnessProgram builds a scriptPubKey for the given witness version and program.
// Format: OP_witVer <push len> <witProg>
// For witness v1+ (Dilithium), OP_1 is 0x51
func WitnessProgram(witVer byte, witProg []byte) []byte {
	// Defence in depth: Decode already rejects unsupported versions, but this
	// is the function that actually mints the scriptPubKey, so it refuses too.
	// A nil return is a hard failure the caller cannot mistake for a script.
	if !supportedWitnessVersions[witVer] || len(witProg) != WitnessProgramSize {
		return nil
	}

	// OP_0 = 0x00, OP_1..OP_16 = 0x51..0x60
	var verByte byte
	if witVer == 0 {
		verByte = 0x00
	} else {
		verByte = 0x50 + witVer
	}

	script := make([]byte, 0, 2+len(witProg))
	script = append(script, verByte)
	script = append(script, byte(len(witProg)))
	script = append(script, witProg...)
	return script
}

// ScriptHash computes the ElectrumX script hash for a scriptPubKey.
// ElectrumX uses SHA256(scriptPubKey) reversed (little-endian hex).
func ScriptHash(scriptPubKey []byte) string {
	h := sha256.Sum256(scriptPubKey)
	// Reverse for ElectrumX
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}

// AddressToScriptHash is a convenience function: address → ElectrumX script hash.
func AddressToScriptHash(hrp, addr string) (string, error) {
	witVer, witProg, err := Decode(hrp, addr)
	if err != nil {
		return "", fmt.Errorf("decode address: %w", err)
	}
	spk := WitnessProgram(witVer, witProg)
	return ScriptHash(spk), nil
}

// --- Network-aware convenience functions ---

// New generates a bech32m address from a witness version and pubkey hash for the given network.
// For standard Dilithium P2WPKH: witVer=1, pubkeyHash=SHA256(publicKey).
func New(hrp string, witVer byte, pubkeyHash []byte) (string, error) {
	return Encode(hrp, witVer, pubkeyHash)
}

// Validate checks if an address is valid for the given network HRP.
// Returns nil if valid, or an error describing why the address is invalid.
func Validate(hrp string, addr string) error {
	_, _, err := Decode(hrp, addr)
	return err
}

// HRPOf extracts the human-readable prefix from a bech32m address without
// requiring the caller to know it in advance.
//
// Every other function here takes the HRP as a parameter, which forces a caller
// that is handling addresses from an unknown network to guess. Guessing is how
// tx.Build*Transaction came to hardcode "ssq" and silently reject every mainnet
// address: Decode returns ErrInvalidHRP on mismatch, so the failure looked like a
// malformed address rather than a wrong assumption.
//
// This performs no network validation. It returns the prefix as written, and the
// caller decides whether that prefix is a network it accepts. Pair it with Decode,
// which validates the checksum against the prefix and so rejects a fabricated one.
func HRPOf(addr string) (string, error) {
	a := strings.ToLower(strings.TrimSpace(addr))
	pos := strings.LastIndex(a, "1")
	if pos < 1 || pos+7 > len(a) {
		return "", ErrInvalidHRP
	}
	return a[:pos], nil
}

// NetworkOf returns the network an address belongs to, derived from its prefix.
// A prefix that belongs to no supported network is refused, because a fabricated
// prefix can otherwise carry a perfectly valid checksum.
// Note: regtest shares the mainnet HRP ("sq") in the node's chainparams, so a
// regtest address resolves to Mainnet here — the address alone cannot tell
// them apart. Only stagenet ("ssq") is distinguishable by prefix.
func NetworkOf(addr string) (types.Network, error) {
	hrp, err := HRPOf(addr)
	if err != nil {
		return types.Network{}, err
	}
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		if hrp == n.HRP {
			return n, nil
		}
	}
	return types.Network{}, fmt.Errorf("%w: unknown network prefix %q in %s",
		ErrInvalidHRP, hrp, addr)
}

// ScriptFor returns the scriptPubKey for an address, deriving the network from
// the address itself.
//
// Prefer this over Decode plus WitnessProgram, which require the caller to supply
// the HRP and so invite hardcoding one. The script produced here is what BIP143
// commits to as the scriptCode, which makes a wrong network a signing fault
// rather than merely a decoding one, so it should come from the address and not
// from a constant at the call site.
func ScriptFor(addr string) ([]byte, error) {
	n, err := NetworkOf(addr)
	if err != nil {
		return nil, err
	}
	witVer, witProg, err := Decode(n.HRP, addr)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", addr, err)
	}
	return WitnessProgram(witVer, witProg), nil
}
