// Package types provides shared types and constants for the Soqucoin SDK.
//
// This package defines the core data structures used across all SDK packages,
// including UTXO representation, network configuration, and asset type constants.
package types

// UTXO represents an unspent transaction output.
type UTXO struct {
	TxID         string `json:"tx_hash"`
	Vout         uint32 `json:"tx_pos"`
	Value        int64  `json:"value"`       // Satoshis
	Height       int64  `json:"height"`      // Block height (0 = unconfirmed)
	ScriptPubKey []byte `json:"-"`           // Populated on demand
	Address      string `json:"-"`           // Which address owns this UTXO
	SpentPending bool   `json:"-"`           // Marked as spent but not confirmed
	AssetType    uint8  `json:"-"`           // 0=native SOQ, 1=USDSOQ
}

// Asset type constants.
const (
	AssetTypeSOQ    uint8 = 0x00 // Native SOQ
	AssetTypeUSDSOQ uint8 = 0x01 // USDSOQ stablecoin
)

// Visibility constants for CTxOut.
const (
	VisibilityTransparent  uint8 = 0x00 // Standard transparent output
	VisibilityConfidential uint8 = 0x01 // Confidential output (Lattice-BP++)
)

// Network defines chain-specific parameters for Soqucoin networks.
type Network struct {
	Name         string // "mainnet", "stagenet", "regtest"
	HRP          string // Bech32m human-readable part
	DefaultPort  int    // P2P port
	ElectrumPort int    // ElectrumX TCP port
	RPCPort      int    // JSON-RPC port
}

// Pre-defined networks.
var (
	// Mainnet is the production Soqucoin network.
	Mainnet = Network{
		Name:         "mainnet",
		HRP:          "sq",
		DefaultPort:  19335,
		ElectrumPort: 50001,
		RPCPort:      19332,
	}

	// Stagenet is the Soqucoin staging/test network.
	Stagenet = Network{
		Name:         "stagenet",
		HRP:          "ssq",
		DefaultPort:  19335,
		ElectrumPort: 50001,
		RPCPort:      19332,
	}

	// Regtest is the local regression test network.
	Regtest = Network{
		Name:         "regtest",
		HRP:          "ssqrt",
		DefaultPort:  19444,
		ElectrumPort: 50001,
		RPCPort:      19443,
	}
)

// Dilithium key and signature size constants (FIPS 204 ML-DSA-44).
const (
	PrivateKeySize = 2560 // ML-DSA-44 private key bytes
	PublicKeySize  = 1312 // ML-DSA-44 public key bytes
	SignatureSize  = 2420 // ML-DSA-44 signature bytes
)

// SatoshisPerSOQ is the number of satoshis in one SOQ.
const SatoshisPerSOQ int64 = 100_000_000
