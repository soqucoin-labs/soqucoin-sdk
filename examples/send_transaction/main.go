// Command send_transaction demonstrates how to construct, sign, and serialize
// a Soqucoin transaction using the SDK.
//
// This example builds a transaction offline — it does NOT broadcast.
// For broadcasting, use rpc.Client.SendRawTransaction() with a live node.
//
// Usage:
//
//	go run ./examples/send_transaction/
package main

import (
	"crypto/sha256"
	"fmt"
	"log"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func main() {
	fmt.Println("=== Soqucoin Transaction Construction Example ===")
	fmt.Println()

	// 1. Generate sender and recipient keypairs
	sender, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		log.Fatal("generate sender:", err)
	}
	recipient, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		log.Fatal("generate recipient:", err)
	}

	fmt.Println("Sender:    ", sender.Address)
	fmt.Println("Recipient: ", recipient.Address)
	fmt.Println()

	// 2. Create a mock UTXO (in production, fetch from ElectrumX)
	mockUTXO := types.UTXO{
		TxID:    "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		Vout:    0,
		Value:   10_000_000_00, // 10 SOQ in satoshis
		Height:  1000,
		Address: sender.Address,
	}

	// 3. Build recipient scriptPubKey from their address
	witVer, witProg, err := address.Decode(types.Stagenet.HRP, recipient.Address)
	if err != nil {
		log.Fatal("decode recipient address:", err)
	}
	recipientSPK := address.WitnessProgram(witVer, witProg)

	// 4. Build change scriptPubKey (back to sender)
	sWitVer, sWitProg, err := address.Decode(types.Stagenet.HRP, sender.Address)
	if err != nil {
		log.Fatal("decode sender address:", err)
	}
	changeSPK := address.WitnessProgram(sWitVer, sWitProg)

	// 5. Build the unsigned transaction
	unsignedTx, err := tx.BuildSendTransaction(
		[]types.UTXO{mockUTXO},
		recipientSPK,
		5_000_000_00, // Send 5 SOQ
		changeSPK,
		10, // Fee rate: 10 sat/vB
	)
	if err != nil {
		log.Fatal("build transaction:", err)
	}

	fmt.Printf("Transaction built: %d inputs, %d outputs\n", len(unsignedTx.Inputs), len(unsignedTx.Outputs))
	fmt.Printf("Estimated weight: %d WU (%d vB)\n", unsignedTx.EstimateWeight(), (unsignedTx.EstimateWeight()+3)/4)
	fmt.Printf("Estimated fee: %d satoshis\n", unsignedTx.EstimateFee(10))
	fmt.Println()

	// 6. Sign each input
	mgr := keys.NewManager("/dev/null", "example-passphrase")
	_ = mgr.ImportPrivateKey(sender.PrivateKey, sender.PublicKey, sender.Address)

	for i := range unsignedTx.Inputs {
		// Compute BIP143 sighash for this input
		sigHash, err := unsignedTx.ComputeSigHash(i, tx.SigHashAll)
		if err != nil {
			log.Fatal("compute sighash:", err)
		}

		// Sign with Dilithium
		sig, err := mgr.Sign(sender.Address, sigHash)
		if err != nil {
			log.Fatal("sign:", err)
		}

		// Set witness data: [signature, pubkey]
		unsignedTx.Inputs[i].WitnessData = [][]byte{sig, sender.PublicKey}

		fmt.Printf("Input %d signed: %d-byte Dilithium signature\n", i, len(sig))
	}

	// 7. Serialize the signed transaction
	rawTx := unsignedTx.SerializeHex()
	txID := unsignedTx.TxID()

	fmt.Println()
	fmt.Println("TXID:", txID)
	fmt.Printf("Raw TX: %d bytes (hex: %d chars)\n", len(unsignedTx.Serialize()), len(rawTx))
	fmt.Println()

	// 8. Verify the signature
	senderPKHash := sha256.Sum256(sender.PublicKey)
	_ = senderPKHash // Used for address derivation verification

	sigValid, err := keys.Verify(sender.PublicKey, func() []byte {
		sh, _ := unsignedTx.ComputeSigHash(0, tx.SigHashAll)
		return sh
	}(), unsignedTx.Inputs[0].WitnessData[0])
	if err != nil {
		log.Fatal("verify:", err)
	}
	fmt.Printf("Signature verification: %v ✓\n", sigValid)
	fmt.Println()
	fmt.Println("In production, broadcast via: rpc.Client.SendRawTransaction(rawTx)")
}
