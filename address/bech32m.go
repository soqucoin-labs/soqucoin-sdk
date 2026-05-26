package address

// Bech32m encoder/decoder for Soqucoin segwit addresses (ssq1p...).
// Implements BIP-350 (bech32m for witness version >= 1).
//
// Usage:
//   witVer, witProg, err := Decode("ssq", "ssq1p...")
//   scriptPubKey := WitnessProgram(witVer, witProg)
//   electrumHash := ScriptHash(scriptPubKey)

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)


var (
	ErrInvalidChecksum = errors.New("bech32m: invalid checksum")
	ErrInvalidLength   = errors.New("bech32m: invalid data length")
	ErrInvalidHRP      = errors.New("bech32m: invalid human-readable part")
	ErrInvalidChar     = errors.New("bech32m: invalid character")
)

// bech32m constant
const bech32mConst = 0x2bc830a3

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

	// BIP-141 witness program length constraints
	if len(witProg) < 2 || len(witProg) > 40 {
		return 0, nil, fmt.Errorf("%w: witness program length %d", ErrInvalidLength, len(witProg))
	}

	return witVer, witProg, nil
}

// Encode encodes a witness version and program to a bech32m string.
func Encode(hrp string, witVer byte, witProg []byte) (string, error) {
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

