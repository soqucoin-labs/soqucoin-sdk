// Package tx constructs raw Soqucoin transactions.
//
// Handles witness v0/v1 P2WPKH-Dilithium transactions with BIP143 sighash
// computation, fee estimation, and Soqucoin-specific CTxOut serialization
// CTxOut is standard Bitcoin (value + scriptPubKey) since migration Phase 4;
// asset and visibility follow the witness version, not extension bytes.
package tx

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Soqucoin transaction constants.
const (
	// Witness marker and flag bytes (BIP144)
	WitnessMarker = 0x00
	WitnessFlag   = 0x01

	// Sighash types
	SigHashAll          = 0x01
	SigHashNone         = 0x02
	SigHashSingle       = 0x03
	SigHashAnyoneCanPay = 0x80

	// Transaction version
	TxVersion = 2

	// Sequence number (no RBF, no relative locktime)
	DefaultSequence = 0xffffffff

	// Dilithium signature size
	DilithiumSigSize = 2420

	// Dilithium public key size
	DilithiumPubKeySize = 1312

	// Weight of one signed input, for documentation and rough planning; the
	// builder measures the real serialized form (see Weight). Non-witness:
	// prevout 36 + scriptSig length 1 + sequence 4 = 41 bytes at 4 WU each.
	// Witness: item count 1 + varint(2421) 3 + signature||hashtype 2421 +
	// varint(1313) 3 + 0x00||pubkey 1313 = 3741 bytes at 1 WU each. The
	// earlier value omitted the count and the two 3-byte varints, which
	// underestimated vsize by about 2.25 vB per input, enough to fall below
	// the miner's default fee floor at the documented rate (review, 2026-09-03).
	EstimatedInputWeight = 41*4 + 3741 // 3905 WU

	// Weight of one v1 output: value 8 + script length 1 + script 34 = 43 bytes.
	EstimatedOutputWeight = 43 * 4 // 172 WU

	// Fixed overhead: version 4 + input count 1 + output count 1 + locktime 4
	// = 10 non-witness bytes, plus the 2 witness bytes (marker, flag).
	TxOverheadWeight = 10*4 + 2 // 42 WU

	// UTXOCostPerByte is the node's relay-policy floor on output value:
	// IsStandardTx rejects any output worth less than this many satoshis per
	// serialized output byte (src/policy/policy.cpp, UTXO_COST_PER_BYTE),
	// independently of any deployment. A 34-byte v1 script serializes to 43
	// bytes, so the floor for a normal output is 279,500 satoshis.
	UTXOCostPerByte int64 = 6500

	// DustThreshold is the relay floor for a standard v1 output (43 serialized
	// bytes). Outputs below it are never emitted: change is folded into the
	// fee and a recipient amount below it is refused. Use MinOutputValue for
	// other script sizes.
	DustThreshold int64 = UTXOCostPerByte * 43 // 279,500

	// MaxMoney is the node's per-transaction ceiling on any amount
	// (src/amount.h): 20e9 SOQ.
	MaxMoney int64 = 20_000_000_000 * types.SatoshisPerSOQ

	// FeeMarginVBytes is added to the measured vsize before the fee is
	// computed, so a rounding difference against the node's own vsize can
	// never put a transaction one satoshi under a fee floor.
	FeeMarginVBytes int64 = 1
)

// Fee sanity limits. A fee above either is refused with ErrFeeTooHigh before
// anything is signed. They are variables so an operator can tighten them; the
// defaults are loose enough for any standard transaction at the recommended
// rate (a maximal 80-input payout at 1000 sat/vB is about 0.75 SOQ) and tight
// enough to stop a fee-rate typo from burning a hot wallet up to the node's
// own 100 SOQ limit.
var (
	MaxFeeSat          int64 = 2 * types.SatoshisPerSOQ // 2 SOQ
	MaxFeeRateSatPerVB int64 = 100_000                  // 100x the recommended rate
)

// Builder errors. All are permanent: the request itself is wrong.
var (
	ErrInvalidAmount     = errors.New("tx: amount must be positive and at most MaxMoney")
	ErrBelowDust         = errors.New("tx: output below the node's relay floor (UTXOCostPerByte x serialized size)")
	ErrFeeTooHigh        = errors.New("tx: fee exceeds the configured sanity limit")
	ErrInsufficientFunds = errors.New("tx: insufficient funds")
	ErrInputOverflow     = errors.New("tx: input values overflow")
)

// MinOutputValue is the node's relay floor for an output carrying this
// scriptPubKey: UTXOCostPerByte times the serialized size (8 value bytes,
// the script length varint, the script).
func MinOutputValue(scriptPubKey []byte) int64 {
	return UTXOCostPerByte * int64(8+varIntSize(uint64(len(scriptPubKey)))+len(scriptPubKey))
}

func varIntSize(v uint64) int {
	switch {
	case v < 0xfd:
		return 1
	case v <= 0xffff:
		return 3
	case v <= 0xffffffff:
		return 5
	default:
		return 9
	}
}

// checkAmount enforces the node's amount range on a single output value.
func checkAmount(v int64) error {
	if v <= 0 || v > MaxMoney {
		return fmt.Errorf("%w: %d", ErrInvalidAmount, v)
	}
	return nil
}

// sumInputs adds input values with overflow detection.
func sumInputs(inputs []types.UTXO) (int64, error) {
	var total int64
	for _, u := range inputs {
		if err := checkAmount(u.Value); err != nil {
			return 0, fmt.Errorf("input %s:%d: %w", u.TxID, u.Vout, err)
		}
		if total > MaxMoney-u.Value {
			return 0, ErrInputOverflow
		}
		total += u.Value
	}
	return total, nil
}

// checkFee applies the fee sanity limits.
func checkFee(fee, feeRate int64) error {
	if feeRate <= 0 {
		return fmt.Errorf("%w: fee rate %d sat/vB", ErrInvalidAmount, feeRate)
	}
	if feeRate > MaxFeeRateSatPerVB {
		return fmt.Errorf("%w: fee rate %d sat/vB exceeds MaxFeeRateSatPerVB %d", ErrFeeTooHigh, feeRate, MaxFeeRateSatPerVB)
	}
	if fee > MaxFeeSat {
		return fmt.Errorf("%w: fee %d sat exceeds MaxFeeSat %d", ErrFeeTooHigh, fee, MaxFeeSat)
	}
	return nil
}

// TxInput represents a transaction input.
type TxInput struct {
	TxID         [32]byte // Previous TX hash (internal byte order)
	Vout         uint32   // Previous output index
	Sequence     uint32   // Sequence number
	Value        int64    // Input value (for sighash computation)
	ScriptPubKey []byte   // Previous output's scriptPubKey (for sighash)
	WitnessData  [][]byte // Witness stack items [signature, pubkey]
	Address      string   // Source address (for key lookup during signing)
}

// TxOutput represents a transaction output.
//
// CTxOut migration Phase 4: the nVisibility/nAssetType extension bytes were
// REMOVED. CTxOut is now standard Bitcoin (value + scriptPubKey), identical to the
// foreign/AuxPoW-parent encoding. Asset and visibility follow the WITNESS VERSION
// (USDSOQ = v7 OP_7, confidential = v4 OP_4), so an output carries no extra bytes.
//
// This mirrors soq-signer/internal/txbuilder, the production reference, and is
// pinned byte-identically to the node by the golden vector in the tests.
type TxOutput struct {
	Value        int64  // Output value in satoshis
	ScriptPubKey []byte // Output script
}

// Transaction represents a raw Soqucoin transaction.
type Transaction struct {
	Version  uint32
	Inputs   []TxInput
	Outputs  []TxOutput
	LockTime uint32
}

// NewTransaction creates a new unsigned transaction.
func NewTransaction() *Transaction {
	return &Transaction{
		Version:  TxVersion,
		LockTime: 0,
	}
}

// AddInput adds an input from a UTXO.
func (tx *Transaction) AddInput(u types.UTXO, scriptPubKey []byte) error {
	txidBytes, err := hex.DecodeString(u.TxID)
	if err != nil {
		return fmt.Errorf("decode txid: %w", err)
	}
	if len(txidBytes) != 32 {
		return fmt.Errorf("invalid txid length: %d", len(txidBytes))
	}

	var txid [32]byte
	// Reverse byte order (display order → internal order)
	for i := 0; i < 32; i++ {
		txid[i] = txidBytes[31-i]
	}

	tx.Inputs = append(tx.Inputs, TxInput{
		TxID:         txid,
		Vout:         u.Vout,
		Sequence:     DefaultSequence,
		Value:        u.Value,
		ScriptPubKey: scriptPubKey,
		Address:      u.Address,
	})
	return nil
}

// AddOutput adds an output to the transaction.
func (tx *Transaction) AddOutput(value int64, scriptPubKey []byte) {
	tx.Outputs = append(tx.Outputs, TxOutput{
		Value:        value,
		ScriptPubKey: scriptPubKey,
	})
}

// EstimateWeight returns the transaction weight in weight units, measured on
// the serialized form: 3 x the non-witness serialization plus the full
// serialization (BIP 141). Inputs not yet signed are counted with the exact
// witness this SDK emits (2421-byte signature||hashtype, 1313-byte 0x00||pk),
// so the value is the node's own weight for the signed transaction, not an
// approximation. Because every witness item has a fixed size, the estimate
// before signing equals the measurement after.
func (tx *Transaction) EstimateWeight() int {
	base := len(tx.serializeNoWitness())
	witness := 2 // marker + flag
	for _, in := range tx.Inputs {
		if len(in.WitnessData) > 0 {
			witness += varIntSize(uint64(len(in.WitnessData)))
			for _, item := range in.WitnessData {
				witness += varIntSize(uint64(len(item))) + len(item)
			}
			continue
		}
		witness += 1 + varIntSize(DilithiumSigSize+1) + (DilithiumSigSize + 1) +
			varIntSize(DilithiumPubKeySize+1) + (DilithiumPubKeySize + 1)
	}
	return base*4 + witness
}

// VSize returns the virtual size in vbytes, weight divided by four rounded up,
// as the node computes it for fee purposes.
func (tx *Transaction) VSize() int64 {
	return int64((tx.EstimateWeight() + 3) / 4)
}

// EstimateFee returns the fee for this transaction at feeRate (sat/vB):
// (vsize + FeeMarginVBytes) x feeRate.
func (tx *Transaction) EstimateFee(feeRate int64) int64 {
	return (tx.VSize() + FeeMarginVBytes) * feeRate
}

// ComputeSigHash computes the BIP143 sighash for a specific input.
// This is the message digest that gets signed with Dilithium.
func (tx *Transaction) ComputeSigHash(inputIndex int, hashType uint32) ([]byte, error) {
	if inputIndex < 0 || inputIndex >= len(tx.Inputs) {
		return nil, fmt.Errorf("input index %d out of range [0, %d)", inputIndex, len(tx.Inputs))
	}

	input := tx.Inputs[inputIndex]

	// BIP143 sighash preimage components:
	// 1. hashPrevouts = SHA256d(all outpoints)
	hashPrevouts := sha256d(serializeAllOutpoints(tx.Inputs))

	// 2. hashSequence = SHA256d(all sequences)
	hashSequence := sha256d(serializeAllSequences(tx.Inputs))

	// 3. outpoint = this input's outpoint
	var outpoint bytes.Buffer
	outpoint.Write(input.TxID[:])
	binary.Write(&outpoint, binary.LittleEndian, input.Vout)

	// 4. scriptCode = the previous output's scriptPubKey
	// For P2WPKH: OP_DUP OP_HASH160 <20-byte-hash> OP_EQUALVERIFY OP_CHECKSIG
	// For Soqucoin P2WPKH-Dilithium (witness v0/v1): the scriptPubKey itself
	scriptCode := input.ScriptPubKey

	// 5. value = input amount (8 bytes LE)
	// 6. nSequence = this input's sequence (4 bytes LE)

	// 7. hashOutputs = SHA256d(all outputs)
	hashOutputs := sha256d(serializeAllOutputs(tx.Outputs))

	// Build the preimage
	var preimage bytes.Buffer

	// nVersion (4 bytes LE)
	binary.Write(&preimage, binary.LittleEndian, tx.Version)

	// hashPrevouts (32 bytes)
	preimage.Write(hashPrevouts)

	// hashSequence (32 bytes)
	preimage.Write(hashSequence)

	// outpoint (36 bytes)
	preimage.Write(outpoint.Bytes())

	// scriptCode (varint + script)
	writeVarInt(&preimage, uint64(len(scriptCode)))
	preimage.Write(scriptCode)

	// value (8 bytes LE)
	binary.Write(&preimage, binary.LittleEndian, input.Value)

	// nSequence (4 bytes LE)
	binary.Write(&preimage, binary.LittleEndian, input.Sequence)

	// hashOutputs (32 bytes)
	preimage.Write(hashOutputs)

	// nLockTime (4 bytes LE)
	binary.Write(&preimage, binary.LittleEndian, tx.LockTime)

	// nHashType (4 bytes LE)
	binary.Write(&preimage, binary.LittleEndian, hashType)

	// Double SHA-256 the preimage
	hash := sha256d(preimage.Bytes())
	return hash, nil
}

// Serialize returns the fully serialized transaction (with witness data) as raw bytes.
func (tx *Transaction) Serialize() []byte {
	var buf bytes.Buffer

	// Version
	binary.Write(&buf, binary.LittleEndian, tx.Version)

	// Witness marker + flag
	hasWitness := false
	for _, input := range tx.Inputs {
		if len(input.WitnessData) > 0 {
			hasWitness = true
			break
		}
	}
	if hasWitness {
		buf.WriteByte(WitnessMarker)
		buf.WriteByte(WitnessFlag)
	}

	// Input count
	writeVarInt(&buf, uint64(len(tx.Inputs)))

	// Inputs
	for _, input := range tx.Inputs {
		buf.Write(input.TxID[:])
		binary.Write(&buf, binary.LittleEndian, input.Vout)
		// scriptSig is always empty for SegWit
		writeVarInt(&buf, 0)
		binary.Write(&buf, binary.LittleEndian, input.Sequence)
	}

	// Output count
	writeVarInt(&buf, uint64(len(tx.Outputs)))

	// Outputs
	for _, output := range tx.Outputs {
		binary.Write(&buf, binary.LittleEndian, output.Value)
		writeVarInt(&buf, uint64(len(output.ScriptPubKey)))
		buf.Write(output.ScriptPubKey)
	}

	// Witness data (if present)
	if hasWitness {
		for _, input := range tx.Inputs {
			writeVarInt(&buf, uint64(len(input.WitnessData)))
			for _, item := range input.WitnessData {
				writeVarInt(&buf, uint64(len(item)))
				buf.Write(item)
			}
		}
	}

	// Locktime
	binary.Write(&buf, binary.LittleEndian, tx.LockTime)

	return buf.Bytes()
}

// SerializeHex returns the hex-encoded serialized transaction.
func (tx *Transaction) SerializeHex() string {
	return hex.EncodeToString(tx.Serialize())
}

// serializeNoWitness is the legacy (non-witness) serialization, which the
// txid and the base weight are computed over.
func (tx *Transaction) serializeNoWitness() []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, tx.Version)
	writeVarInt(&buf, uint64(len(tx.Inputs)))
	for _, input := range tx.Inputs {
		buf.Write(input.TxID[:])
		binary.Write(&buf, binary.LittleEndian, input.Vout)
		writeVarInt(&buf, 0)
		binary.Write(&buf, binary.LittleEndian, input.Sequence)
	}
	writeVarInt(&buf, uint64(len(tx.Outputs)))
	for _, output := range tx.Outputs {
		binary.Write(&buf, binary.LittleEndian, output.Value)
		writeVarInt(&buf, uint64(len(output.ScriptPubKey)))
		buf.Write(output.ScriptPubKey)
	}
	binary.Write(&buf, binary.LittleEndian, tx.LockTime)
	return buf.Bytes()
}

// TxID computes the transaction ID (double SHA-256 of the non-witness serialization).
func (tx *Transaction) TxID() string {
	var buf bytes.Buffer

	// Version
	binary.Write(&buf, binary.LittleEndian, tx.Version)

	// Input count (no witness marker/flag for txid)
	writeVarInt(&buf, uint64(len(tx.Inputs)))

	// Inputs
	for _, input := range tx.Inputs {
		buf.Write(input.TxID[:])
		binary.Write(&buf, binary.LittleEndian, input.Vout)
		writeVarInt(&buf, 0) // empty scriptSig
		binary.Write(&buf, binary.LittleEndian, input.Sequence)
	}

	// Output count
	writeVarInt(&buf, uint64(len(tx.Outputs)))

	// Outputs
	for _, output := range tx.Outputs {
		binary.Write(&buf, binary.LittleEndian, output.Value)
		writeVarInt(&buf, uint64(len(output.ScriptPubKey)))
		buf.Write(output.ScriptPubKey)
	}

	// Locktime
	binary.Write(&buf, binary.LittleEndian, tx.LockTime)

	hash := sha256d(buf.Bytes())

	// Reverse for display order
	for i, j := 0, len(hash)-1; i < j; i, j = i+1, j-1 {
		hash[i], hash[j] = hash[j], hash[i]
	}

	return hex.EncodeToString(hash)
}

// --- Helpers ---

// sha256d computes double SHA-256.
func sha256d(data []byte) []byte {
	first := sha256.Sum256(data)
	second := sha256.Sum256(first[:])
	return second[:]
}

// serializeAllOutpoints serializes all input outpoints for hashPrevouts.
func serializeAllOutpoints(inputs []TxInput) []byte {
	var buf bytes.Buffer
	for _, input := range inputs {
		buf.Write(input.TxID[:])
		binary.Write(&buf, binary.LittleEndian, input.Vout)
	}
	return buf.Bytes()
}

// serializeAllSequences serializes all input sequences for hashSequence.
func serializeAllSequences(inputs []TxInput) []byte {
	var buf bytes.Buffer
	for _, input := range inputs {
		binary.Write(&buf, binary.LittleEndian, input.Sequence)
	}
	return buf.Bytes()
}

// serializeAllOutputs serializes all outputs for hashOutputs.
func serializeAllOutputs(outputs []TxOutput) []byte {
	var buf bytes.Buffer
	for _, output := range outputs {
		binary.Write(&buf, binary.LittleEndian, output.Value)
		writeVarInt(&buf, uint64(len(output.ScriptPubKey)))
		buf.Write(output.ScriptPubKey)
	}
	return buf.Bytes()
}

// writeVarInt writes a Bitcoin-style variable-length integer.
func writeVarInt(buf *bytes.Buffer, val uint64) {
	switch {
	case val < 0xfd:
		buf.WriteByte(byte(val))
	case val <= 0xffff:
		buf.WriteByte(0xfd)
		binary.Write(buf, binary.LittleEndian, uint16(val))
	case val <= 0xffffffff:
		buf.WriteByte(0xfe)
		binary.Write(buf, binary.LittleEndian, uint32(val))
	default:
		buf.WriteByte(0xff)
		binary.Write(buf, binary.LittleEndian, val)
	}
}

// ScriptP2WPKH creates a P2WPKH scriptPubKey: OP_1 <32-byte-pubkey-hash>
// Soqucoin uses OP_1 (witness v1) for Dilithium P2WPKH.
func ScriptP2WPKH(pubkeyHash []byte) []byte {
	if len(pubkeyHash) != 32 {
		panic(fmt.Sprintf("invalid pubkey hash length: %d, want 32", len(pubkeyHash)))
	}
	script := make([]byte, 34)
	script[0] = 0x51 // OP_1 (witness version 1)
	script[1] = 0x20 // Push 32 bytes
	copy(script[2:], pubkeyHash)
	return script
}

// ScriptV7USDSOQHolding creates a witness v7 USDSOQ holding scriptPubKey:
// OP_7 || PUSH_32 || SHA256(pubkey).
//
// Post-Phase-4 the witness version IS the asset discriminator. The node checks
// exactly this shape (CTxOut::IsV7USDSOQHolding: size 34, byte0 OP_7, byte1 32),
// and a v7 holding is spent through the audited v1 single-key Dilithium path.
func ScriptV7USDSOQHolding(pubkeyHash []byte) []byte {
	if len(pubkeyHash) != 32 {
		panic(fmt.Sprintf("invalid pubkey hash length: %d, want 32", len(pubkeyHash)))
	}
	script := make([]byte, 34)
	script[0] = 0x57 // OP_7 (witness version 7 = USDSOQ holding)
	script[1] = 0x20 // Push 32 bytes
	copy(script[2:], pubkeyHash)
	return script
}

// AddOutputUSDSOQ adds a USDSOQ output.
//
// The caller must pass a witness v7 scriptPubKey (see ScriptV7USDSOQHolding).
// There is no nAssetType byte to set: asset type is carried by the witness
// version, so a non-v7 script here would produce a NATIVE SOQ output rather than a
// USDSOQ one. The check below makes that failure loud instead of silent.
// Used for minting USDSOQ tokens. The recipient receives USDSOQ, while any
// change outputs should use AddOutput (native SOQ for fee change).
func (tx *Transaction) AddOutputUSDSOQ(value int64, scriptPubKey []byte) {
	if !(len(scriptPubKey) == 34 && scriptPubKey[0] == 0x57 && scriptPubKey[1] == 0x20) {
		panic("AddOutputUSDSOQ requires a witness v7 scriptPubKey (OP_7 <32>); " +
			"asset type follows the witness version, use ScriptV7USDSOQHolding")
	}
	tx.Outputs = append(tx.Outputs, TxOutput{
		Value:        value,
		ScriptPubKey: scriptPubKey,
	})
}

// ScriptWitnessV5 creates a witness v5 (USDSOQ authority) scriptPubKey:
// OP_5 || PUSH_32 || SHA256(authority_pubkey)
// This is checked by ConnectBlock (validation.cpp L2211-2213) to identify
// authority transactions that are exempted from asset isolation.
func ScriptWitnessV5(authorityPKHash []byte) []byte {
	if len(authorityPKHash) != 32 {
		panic(fmt.Sprintf("invalid authority pubkey hash length: %d, want 32", len(authorityPKHash)))
	}
	script := make([]byte, 34)
	script[0] = 0x55 // OP_5 (witness version 5)
	script[1] = 0x20 // Push 32 bytes
	copy(script[2:], authorityPKHash)
	return script
}

// AddOutputWitnessV5 adds a 0-value witness v5 authority marker output.
// This output serves as the on-chain record of the USDSOQ authority operation.
// ConnectBlock checks for this OP_5 output to set isAuthorityTx=true,
// which exempts the TX from the USDSOQ input-side asset isolation check.
// The witness v5 handler in VerifyScript (interpreter.cpp L874-909) will
// validate the authority signatures when this output is later spent.
func (tx *Transaction) AddOutputWitnessV5(authorityPKHash []byte) {
	tx.Outputs = append(tx.Outputs, TxOutput{
		Value:        0, // No value locked in the authority marker
		ScriptPubKey: ScriptWitnessV5(authorityPKHash),
	})
}

// BuildMintUSDSOQTransaction constructs an unsigned USDSOQ authority mint transaction.
//
// The transaction structure is:
//
//	vout[0]: USDSOQ recipient output (witness v7, amount=mint amount)
//	vout[1]: Witness v5 authority marker (OP_5 || 0x20 || SHA256(authority_pk), value=0)
//	vout[2]: native SOQ change output (witness v1, for fee change, if above dust)
//
// The witness v5 authority marker is required by ConnectBlock (validation.cpp L2210-2216)
// to identify this as an authority TX. Without it, the asset isolation check rejects
// the TX because it has SOQ inputs but USDSOQ outputs ('bad-txns-usdsoq-input-mismatch').
//
// authorityPKHash is SHA256(authority_public_key) — the 32-byte witness v5 program.
func BuildMintUSDSOQTransaction(
	inputs []types.UTXO,
	recipientScriptPubKey []byte,
	amount int64,
	changeScriptPubKey []byte,
	authorityPKHash []byte,
	feeRate int64,
) (*Transaction, error) {
	if err := requireSameNetwork(inputs); err != nil {
		return nil, err
	}
	if len(authorityPKHash) != 32 {
		return nil, fmt.Errorf("authority pubkey hash must be 32 bytes, got %d", len(authorityPKHash))
	}

	tx := NewTransaction()

	// Add inputs (SOQ UTXOs for fee payment)
	var totalInput int64
	for _, u := range inputs {
		inputSPK, err := inputScriptPubKey(u.Address)
		if err != nil {
			return nil, err
		}

		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add input: %w", err)
		}
		totalInput += u.Value
	}

	// Estimate fee (pessimistic: assume 3 outputs — recipient + authority + change)
	tx.Outputs = make([]TxOutput, 3) // temporary for weight estimation
	fee := tx.EstimateFee(feeRate)
	tx.Outputs = nil // reset

	// Calculate change (inputs are SOQ for fees, USDSOQ amount is created ex nihilo)
	// The fee is paid from SOQ inputs. The USDSOQ amount is NOT deducted from inputs.
	change := totalInput - fee
	if change < 0 {
		return nil, fmt.Errorf("insufficient SOQ for fees: inputs=%d, fee=%d",
			totalInput, fee)
	}

	// vout[0]: USDSOQ recipient output (witness v7)
	tx.AddOutputUSDSOQ(amount, recipientScriptPubKey)

	// vout[1]: Witness v5 authority marker output (value=0, OP_5 program)
	// This is the critical output that ConnectBlock uses to detect authority TXs.
	tx.AddOutputWitnessV5(authorityPKHash)

	// vout[2]: Native SOQ change output (witness v1, for fee change)
	if change >= MinOutputValue(changeScriptPubKey) {
		tx.AddOutput(change, changeScriptPubKey)
	} else {
		// Below the relay floor — left to miners as additional fee
		fee += change
	}

	return tx, nil
}

// BuildSendUSDSOQTransaction constructs an unsigned USDSOQ transfer transaction.
//
// Asset isolation rules require separate input sets:
//   - usdsoqInputs: USDSOQ UTXOs (witness v7) that fund the recipient + USDSOQ change
//   - soqInputs: native SOQ UTXOs that pay the transaction fee
//
// The transaction structure is:
//
//	vout[0]: USDSOQ recipient output (witness v7, amount=transfer amount)
//	vout[1]: USDSOQ change output (witness v7, if above dust)
//	vout[2]: native SOQ fee change output (witness v1, if above dust)
func BuildSendUSDSOQTransaction(
	usdsoqInputs []types.UTXO,
	soqInputs []types.UTXO,
	recipientScriptPubKey []byte,
	amount int64,
	usdsoqChangeScriptPubKey []byte,
	soqChangeScriptPubKey []byte,
	feeRate int64,
) (*Transaction, error) {
	if err := requireSameNetwork(usdsoqInputs, soqInputs); err != nil {
		return nil, err
	}
	tx := NewTransaction()

	// Add USDSOQ inputs
	var totalUSDSOQ int64
	for _, u := range usdsoqInputs {
		inputSPK, err := inputScriptPubKey(u.Address)
		if err != nil {
			return nil, err
		}

		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add usdsoq input: %w", err)
		}
		totalUSDSOQ += u.Value
	}

	// Add SOQ inputs (for fee payment)
	var totalSOQ int64
	for _, u := range soqInputs {
		inputSPK, err := inputScriptPubKey(u.Address)
		if err != nil {
			return nil, err
		}

		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add soq input: %w", err)
		}
		totalSOQ += u.Value
	}

	// Estimate fee (pessimistic: assume 3 outputs — recipient + usdsoq change + soq change)
	tx.Outputs = make([]TxOutput, 3) // temporary for weight estimation
	fee := tx.EstimateFee(feeRate)
	tx.Outputs = nil // reset

	// Validate USDSOQ balance
	if totalUSDSOQ < amount {
		return nil, fmt.Errorf("insufficient USDSOQ: inputs=%d, amount=%d",
			totalUSDSOQ, amount)
	}

	// Validate SOQ balance for fees
	soqChange := totalSOQ - fee
	if soqChange < 0 {
		return nil, fmt.Errorf("insufficient SOQ for fees: inputs=%d, fee=%d",
			totalSOQ, fee)
	}

	// vout[0]: USDSOQ recipient output (witness v7)
	tx.AddOutputUSDSOQ(amount, recipientScriptPubKey)

	// vout[1]: USDSOQ change output (witness v7, if above dust)
	usdsoqChange := totalUSDSOQ - amount
	if usdsoqChange >= MinOutputValue(usdsoqChangeScriptPubKey) {
		tx.AddOutputUSDSOQ(usdsoqChange, usdsoqChangeScriptPubKey)
	}

	// vout[2]: native SOQ fee change output (witness v1, if at or above the relay floor)
	if soqChange >= MinOutputValue(soqChangeScriptPubKey) {
		tx.AddOutput(soqChange, soqChangeScriptPubKey)
	}

	return tx, nil
}

// BuildSendTransaction constructs a complete unsigned transaction for a simple send.
// Returns the transaction ready for signing.
func BuildSendTransaction(
	inputs []types.UTXO,
	recipientScriptPubKey []byte,
	amount int64,
	changeScriptPubKey []byte,
	feeRate int64,
) (*Transaction, error) {
	if err := requireSameNetwork(inputs); err != nil {
		return nil, err
	}
	if err := checkAmount(amount); err != nil {
		return nil, err
	}
	if floor := MinOutputValue(recipientScriptPubKey); amount < floor {
		return nil, fmt.Errorf("%w: %d sat to recipient, floor %d", ErrBelowDust, amount, floor)
	}
	totalInput, err := sumInputs(inputs)
	if err != nil {
		return nil, err
	}
	tx := NewTransaction()

	// Add inputs
	for _, u := range inputs {
		// Derive the input's scriptPubKey from the UTXO's bech32m address.
		// This is critical for BIP143 sighash computation — the scriptCode
		// field must match what the node uses during verification.
		inputSPK, err := inputScriptPubKey(u.Address)
		if err != nil {
			return nil, err
		}

		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add input: %w", err)
		}
	}

	// Fee for the two-output form (recipient + change), measured on the real
	// serialization; if change is later folded away the fee only grows.
	tx.AddOutput(amount, recipientScriptPubKey)
	tx.AddOutput(0, changeScriptPubKey) // placeholder for measurement
	fee := tx.EstimateFee(feeRate)
	tx.Outputs = tx.Outputs[:1]
	if err := checkFee(fee, feeRate); err != nil {
		return nil, err
	}

	// Calculate change
	change := totalInput - amount - fee
	if change < 0 {
		return nil, fmt.Errorf("%w: inputs=%d, amount=%d, fee=%d",
			ErrInsufficientFunds, totalInput, amount, fee)
	}

	// Change output only above the relay floor for its script; otherwise it is
	// left to the miner as fee (the node would reject the output as dust).
	if change >= MinOutputValue(changeScriptPubKey) {
		tx.AddOutput(change, changeScriptPubKey)
	}

	return tx, nil
}

// inputScriptPubKey derives an input's scriptPubKey from its bech32m address,
// taking the network from the address rather than assuming one.
//
// The scriptPubKey is what BIP143 commits to as the scriptCode, so the network
// must never be a constant at the call site: a mismatch would not simply fail to
// decode, it would sign over the wrong message.
//
// The derivation lives in soqaddr.ScriptFor so the SDK keeps exactly one
// implementation of "address to scriptCode" rather than two that could drift.
func inputScriptPubKey(addr string) ([]byte, error) {
	return soqaddr.ScriptFor(addr)
}

// requireSameNetwork rejects a set of inputs that mix networks. Mixing them is
// never legitimate, and silently building such a transaction would spend against
// the wrong chain's scriptCode.
func requireSameNetwork(utxos ...[]types.UTXO) error {
	var first, firstAddr string
	for _, set := range utxos {
		for _, u := range set {
			hrp, err := soqaddr.HRPOf(u.Address)
			if err != nil {
				return fmt.Errorf("address %s: %w", u.Address, err)
			}
			if first == "" {
				first, firstAddr = hrp, u.Address
				continue
			}
			if hrp != first {
				return fmt.Errorf("inputs mix networks: %s is %q but %s is %q",
					firstAddr, first, u.Address, hrp)
			}
		}
	}
	return nil
}

// ── Signing ────────────────────────────────────────────────────────────────

// Signer produces a Dilithium signature over a digest for a managed address, and
// the corresponding public key. *keys.Manager satisfies this interface.
type Signer interface {
	Sign(address string, digest []byte) ([]byte, error)
	PublicKeyFor(address string) ([]byte, error)
}

// SignInput signs input i and installs the witness in the exact format consensus
// requires.
//
// The format is not the obvious one, and getting it wrong produces a transaction
// the node rejects with "bad-txns-requires-dilithium":
//
//	stack[0] = signature || sighash-type byte   (2421 bytes)
//	stack[1] = 0x00 || public key               (1313 bytes)
//
// The trailing sighash byte follows Bitcoin convention. The leading 0x00 on the
// public key is required by CTransaction::HasDilithiumSignatures, which checks
// pk_blob[0] == 0x00 because NIST FIPS 204 Table 3 specifies that ML-DSA-44
// public keys begin with that byte.
//
// Before this helper existed, the only signing example in the SDK assembled
// [][]byte{sig, pubKey} with neither the sighash byte nor the 0x00 prefix, so
// every transaction it produced was rejected by the node.
func (tx *Transaction) SignInput(i int, signer Signer, hashType uint32) error {
	if i < 0 || i >= len(tx.Inputs) {
		return fmt.Errorf("input index %d out of range [0, %d)", i, len(tx.Inputs))
	}
	addr := tx.Inputs[i].Address
	if addr == "" {
		return fmt.Errorf("input %d has no address to sign for", i)
	}

	digest, err := tx.ComputeSigHash(i, hashType)
	if err != nil {
		return fmt.Errorf("sighash for input %d: %w", i, err)
	}
	sig, err := signer.Sign(addr, digest)
	if err != nil {
		return fmt.Errorf("sign input %d: %w", i, err)
	}
	pub, err := signer.PublicKeyFor(addr)
	if err != nil {
		return fmt.Errorf("public key for input %d (%s): %w", i, addr, err)
	}
	if len(sig) != types.SignatureSize {
		return fmt.Errorf("input %d: signature is %d bytes, want %d",
			i, len(sig), types.SignatureSize)
	}
	if len(pub) != types.PublicKeySize {
		return fmt.Errorf("input %d: public key is %d bytes, want %d",
			i, len(pub), types.PublicKeySize)
	}

	sigWithHashType := make([]byte, len(sig)+1)
	copy(sigWithHashType, sig)
	sigWithHashType[len(sig)] = byte(hashType)

	prefixedPK := make([]byte, 1+len(pub))
	prefixedPK[0] = 0x00 // FIPS 204 ML-DSA-44 marker, checked by consensus
	copy(prefixedPK[1:], pub)

	tx.Inputs[i].WitnessData = [][]byte{sigWithHashType, prefixedPK}
	return nil
}

// SignAll signs every input with SIGHASH_ALL.
func (tx *Transaction) SignAll(signer Signer) error {
	for i := range tx.Inputs {
		if err := tx.SignInput(i, signer, SigHashAll); err != nil {
			return err
		}
	}
	return nil
}

// BuildAndSign builds, signs and serializes a simple send in one call, returning
// the raw hex ready for sendrawtransaction and the transaction id.
//
// Prefer this over hand-wiring build, sighash, sign, witness assembly and
// serialize: the witness format is Soqucoin-specific and easy to get wrong by a
// byte at each end. Use BuildSendTransaction plus SignAll directly only when you
// need the transaction itself, for example to read the change output's value.
func BuildAndSign(
	inputs []types.UTXO,
	recipientScriptPubKey []byte,
	amount int64,
	changeScriptPubKey []byte,
	feeRate int64,
	signer Signer,
) (rawHex string, txid string, err error) {
	t, err := BuildSendTransaction(inputs, recipientScriptPubKey, amount,
		changeScriptPubKey, feeRate)
	if err != nil {
		return "", "", err
	}
	if err := t.SignAll(signer); err != nil {
		return "", "", err
	}
	return t.SerializeHex(), t.TxID(), nil
}
