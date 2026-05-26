// Command generate_address demonstrates how to create a new Soqucoin address
// using the soqucoin-sdk. This is the most basic operation any integrator needs.
//
// Usage:
//
//	go run ./examples/generate_address/
package main

import (
	"fmt"
	"log"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func main() {
	// Generate a mainnet address (sq1p...)
	kp, err := keys.GenerateKeyForNetwork(types.Mainnet.HRP)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== New Soqucoin Address (Mainnet) ===")
	fmt.Println("Address:     ", kp.Address)
	fmt.Println("Public Key:  ", keys.PubKeyHashHex(kp.PublicKey))
	fmt.Printf("PubKey Size:  %d bytes (ML-DSA-44)\n", len(kp.PublicKey))
	fmt.Printf("PrivKey Size: %d bytes (ML-DSA-44)\n", len(kp.PrivateKey))
	fmt.Println()

	// Generate a stagenet address (ssq1p...)
	kpStage, err := keys.GenerateKeyForNetwork(types.Stagenet.HRP)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== New Soqucoin Address (Stagenet) ===")
	fmt.Println("Address:     ", kpStage.Address)
	fmt.Println("Public Key:  ", keys.PubKeyHashHex(kpStage.PublicKey))
	fmt.Println()
	fmt.Println("Both addresses use NIST FIPS 204 ML-DSA-44 (Dilithium) signatures.")
	fmt.Println("Signature size: 2,420 bytes | Public key size: 1,312 bytes")
}
