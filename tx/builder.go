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

	// Estimated weight per input (witness v0/v1 Dilithium)
	// witness: [sig(2420) + pubkey(1312)] = 3732 bytes witness data
	// + 41 bytes non-witness (prevout:36 + scriptSig:1 + sequence:4)
	EstimatedInputWeight = 41*4 + 3732 // 164 + 3732 = 3896 WU

	// Estimated weight per output (P2WPKH-Dilithium: OP_1 <32-byte hash>)
	// 8 (value) + 1 (script len) + 34 (scriptPubKey) = 43 bytes
	EstimatedOutputWeight = 43 * 4 // 172 WU

	// Transaction overhead: version(4) + marker(1) + flag(1) + input_count(1) + output_count(1) + locktime(4)
	TxOverheadWeight = 12 * 4 // 48 WU (non-witness) + 2 (witness header)

	// DustThreshold is the minimum output value (in satoshis) that soqucoind
	// will accept. MUST match soqucoind's nHardDustLimit (policy.cpp L233):
	//   DEFAULT_HARD_DUST_LIMIT = DEFAULT_DUST_LIMIT / 10 = 100,000 satoshis
	// Reference: DL-PAYOUT-RELIABILITY.md, PF-005
	DustThreshold int64 = 100000
)

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

// EstimateWeight returns the estimated transaction weight in weight units.
func (tx *Transaction) EstimateWeight() int {
	return TxOverheadWeight +
		len(tx.Inputs)*EstimatedInputWeight +
		len(tx.Outputs)*EstimatedOutputWeight
}

// EstimateFee returns the estimated fee for this transaction given a fee rate (sat/vB).
func (tx *Transaction) EstimateFee(feeRate int64) int64 {
	vsize := (tx.EstimateWeight() + 3) / 4 // Round up
	return int64(vsize) * feeRate
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
	if len(authorityPKHash) != 32 {
		return nil, fmt.Errorf("authority pubkey hash must be 32 bytes, got %d", len(authorityPKHash))
	}

	tx := NewTransaction()

	// Add inputs (SOQ UTXOs for fee payment)
	var totalInput int64
	for _, u := range inputs {
		witVer, witProg, err := soqaddr.Decode("ssq", u.Address)
		if err != nil {
			return nil, fmt.Errorf("decode input address %s: %w", u.Address, err)
		}
		inputSPK := soqaddr.WitnessProgram(witVer, witProg)

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
	if change > DustThreshold {
		tx.AddOutput(change, changeScriptPubKey)
	} else {
		// Below dust — donate to miners as additional fee
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
	tx := NewTransaction()

	// Add USDSOQ inputs
	var totalUSDSOQ int64
	for _, u := range usdsoqInputs {
		witVer, witProg, err := soqaddr.Decode("ssq", u.Address)
		if err != nil {
			return nil, fmt.Errorf("decode usdsoq input address %s: %w", u.Address, err)
		}
		inputSPK := soqaddr.WitnessProgram(witVer, witProg)

		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add usdsoq input: %w", err)
		}
		totalUSDSOQ += u.Value
	}

	// Add SOQ inputs (for fee payment)
	var totalSOQ int64
	for _, u := range soqInputs {
		witVer, witProg, err := soqaddr.Decode("ssq", u.Address)
		if err != nil {
			return nil, fmt.Errorf("decode soq input address %s: %w", u.Address, err)
		}
		inputSPK := soqaddr.WitnessProgram(witVer, witProg)

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
	if usdsoqChange > DustThreshold {
		tx.AddOutputUSDSOQ(usdsoqChange, usdsoqChangeScriptPubKey)
	}

	// vout[2]: native SOQ fee change output (witness v1, if above dust)
	if soqChange > DustThreshold {
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
	tx := NewTransaction()

	// Add inputs
	var totalInput int64
	for _, u := range inputs {
		// Derive the input's scriptPubKey from the UTXO's bech32m address.
		// This is critical for BIP143 sighash computation — the scriptCode
		// field must match what the node uses during verification.
		witVer, witProg, err := soqaddr.Decode("ssq", u.Address)
		if err != nil {
			return nil, fmt.Errorf("decode input address %s: %w", u.Address, err)
		}
		inputSPK := soqaddr.WitnessProgram(witVer, witProg)

		if err := tx.AddInput(u, inputSPK); err != nil {
			return nil, fmt.Errorf("add input: %w", err)
		}
		totalInput += u.Value
	}

	// Estimate fee (pessimistic: assume change output exists)
	tx.Outputs = make([]TxOutput, 2) // temporary for weight estimation
	fee := tx.EstimateFee(feeRate)
	tx.Outputs = nil // reset

	// Calculate change
	change := totalInput - amount - fee
	if change < 0 {
		return nil, fmt.Errorf("insufficient funds: inputs=%d, amount=%d, fee=%d",
			totalInput, amount, fee)
	}

	// Add recipient output
	tx.AddOutput(amount, recipientScriptPubKey)

	// Add change output (if above dust threshold)
	if change > DustThreshold {
		tx.AddOutput(change, changeScriptPubKey)
	} else {
		// Below dust — donate to miners as additional fee
		fee += change
	}

	return tx, nil
}
