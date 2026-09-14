// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package tx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Sighash, witness and txid vector produced by the node's own signing path.
//
// testdata/sighash_node_vector.json was recorded from the soqucoin repository's
// sdk_sighash_vector_tests suite (src/test/sdk_sighash_vector_tests.cpp), which
// builds the transaction below from the same fixed fields, signs both inputs
// through ProduceSignature with a fixed ML-DSA-44 key, and asserts the same
// digests and txid. The two files pin each other: a change to the BIP 143
// preimage on either side fails one of them.
//
// What this proves that the golden transaction test does not: the golden test
// carries fabricated witness bytes, so it pins serialization and txid only. Here
// the signature is the node's, so the SDK's ComputeSigHash is shown to produce
// the message the node signed. An ML-DSA signature verifies over exactly one
// message, so keys.Verify passing over the SDK digest is the proof. The
// stagenet transactions in docs/VERIFICATION.md and every harness run prove the
// same property with a node present; this test does it offline.
//
// The node's ML-DSA signing is randomised, so the witness here is one run's
// output. The digests, txid and key are deterministic. Regenerate by running the
// node suite with --log_level=message and pasting the printed transaction.

type nodeVector struct {
	Description string `json:"description"`
	NodeSource  string `json:"node_source"`
	Key         struct {
		SeedLabel    string `json:"seed_label"`
		Seed         string `json:"seed"`
		PackedSHA256 string `json:"packed_sha256"`
		Program      string `json:"program"`
		AddressSq    string `json:"address_sq"`
	} `json:"key"`
	Version  uint32 `json:"version"`
	LockTime uint32 `json:"locktime"`
	Inputs   []struct {
		TxID         string `json:"txid"`
		Vout         uint32 `json:"vout"`
		Value        int64  `json:"value"`
		Sequence     uint32 `json:"sequence"`
		ScriptPubKey string `json:"script_pubkey"`
	} `json:"inputs"`
	Outputs []struct {
		Value        int64  `json:"value"`
		ScriptPubKey string `json:"script_pubkey"`
	} `json:"outputs"`
	Digests  []string   `json:"digests"`
	Witness  [][]string `json:"witness"`
	TxID     string     `json:"txid"`
	SignedTx string     `json:"signed_tx"`
}

func loadNodeVector(t *testing.T) *nodeVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sighash_node_vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v nodeVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Inputs) != 2 || len(v.Outputs) != 2 || len(v.Digests) != 2 || len(v.Witness) != 2 {
		t.Fatalf("fixture shape: %d inputs, %d outputs, %d digests, %d witnesses",
			len(v.Inputs), len(v.Outputs), len(v.Digests), len(v.Witness))
	}
	return &v
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildNodeVector rebuilds the fixture's transaction from its fields through
// the SDK's own constructors, unsigned.
func buildNodeVector(t *testing.T, v *nodeVector) *Transaction {
	t.Helper()
	tr := NewTransaction()
	tr.Version = v.Version
	tr.LockTime = v.LockTime
	for i, in := range v.Inputs {
		u := types.UTXO{TxID: in.TxID, Vout: in.Vout, Value: in.Value, Address: v.Key.AddressSq}
		if err := tr.AddInput(u, mustHex(t, in.ScriptPubKey)); err != nil {
			t.Fatal(err)
		}
		if tr.Inputs[i].Sequence != in.Sequence {
			t.Fatalf("input %d: AddInput sequence %#x, node used %#x", i, tr.Inputs[i].Sequence, in.Sequence)
		}
	}
	for _, out := range v.Outputs {
		tr.AddOutput(out.Value, mustHex(t, out.ScriptPubKey))
	}
	return tr
}

// attachNodeWitness puts the node's witness stacks on the rebuilt transaction.
func attachNodeWitness(t *testing.T, v *nodeVector, tr *Transaction) {
	t.Helper()
	for i, w := range v.Witness {
		if len(w) != 2 {
			t.Fatalf("input %d: node witness has %d items", i, len(w))
		}
		tr.Inputs[i].WitnessData = [][]byte{mustHex(t, w[0]), mustHex(t, w[1])}
	}
}

func TestSighashMatchesNodeVector(t *testing.T) {
	v := loadNodeVector(t)
	tr := buildNodeVector(t, v)

	// The digests the node handed its signer, from the unsigned transaction.
	for i, want := range v.Digests {
		got, err := tr.ComputeSigHash(i, SigHashAll)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != want {
			t.Errorf("input %d: SDK sighash %x, node computed %s", i, got, want)
		}
	}
	if got := tr.TxID(); got != v.TxID {
		t.Errorf("unsigned txid %s, node computed %s", got, v.TxID)
	}

	attachNodeWitness(t, v, tr)

	// Byte-identical to the node's serialization of its signed transaction.
	if got := tr.SerializeHex(); got != v.SignedTx {
		t.Errorf("serialized transaction differs from the node's\nSDK  %s\nnode %s", got, v.SignedTx)
	}
	if got := tr.TxID(); got != v.TxID {
		t.Errorf("signed txid %s, node computed %s", got, v.TxID)
	}

	// The node's signatures verify over the SDK's digests: the SDK signs the
	// message the node signs. VerifyAll reads the hashtype from the witness
	// and recomputes; keys.Verify is the same check over the recorded digest
	// with the node's signature and key, under the SDK's ML-DSA library.
	if err := tr.VerifyAll(); err != nil {
		t.Fatalf("node witness does not verify under the SDK: %v", err)
	}
	for i, in := range tr.Inputs {
		ok, err := keys.Verify(in.WitnessData[1], mustHex(t, v.Digests[i]), in.WitnessData[0])
		if err != nil || !ok {
			t.Errorf("input %d: node signature over the recorded digest: ok=%v err=%v", i, ok, err)
		}
	}
}

func TestNodeVectorKeyIsTheSeedKey(t *testing.T) {
	v := loadNodeVector(t)
	var seed [keys.SeedSize]byte
	copy(seed[:], mustHex(t, v.Key.Seed))
	if label := sha256.Sum256([]byte("soqucoin")); label != seed {
		t.Fatalf("fixture seed is not %s", v.Key.SeedLabel)
	}
	kp, err := keys.FromSeed("sq", seed)
	if err != nil {
		t.Fatal(err)
	}
	packed := sha256.Sum256(append(append([]byte{}, kp.PrivateKey...), kp.PublicKey...))
	if hex.EncodeToString(packed[:]) != v.Key.PackedSHA256 {
		t.Errorf("FromSeed key differs from the key the node signed with (sha256 of sk||pk %x)", packed)
	}
	if kp.Address != v.Key.AddressSq {
		t.Errorf("FromSeed address %s, fixture %s", kp.Address, v.Key.AddressSq)
	}
	program := sha256.Sum256(kp.PublicKey)
	if hex.EncodeToString(program[:]) != v.Key.Program {
		t.Errorf("program %x, fixture %s", program, v.Key.Program)
	}
	for i, w := range v.Witness {
		if pk := mustHex(t, w[1]); hex.EncodeToString(pk[1:]) != hex.EncodeToString(kp.PublicKey) {
			t.Errorf("input %d: node witness public key is not the seed key's", i)
		}
	}
}

// Every field the BIP 143 preimage commits to, mutated one at a time, must
// change the digest and fail verification of exactly the inputs whose preimage
// carries the field. The value and scriptCode of one input reach only that
// input's digest; outpoints and sequences (hashPrevouts, hashSequence),
// outputs, version and locktime reach both. A test that only checked the happy
// path could not tell a correct preimage from one that hashes the whole
// transaction.
func TestNodeVectorRejectsEveryPreimageMutation(t *testing.T) {
	v := loadNodeVector(t)
	cases := []struct {
		name   string
		mutate func(tr *Transaction)
		fail   []int // inputs whose verification must now fail
		want   error
	}{
		{"input 0 value", func(tr *Transaction) { tr.Inputs[0].Value++ }, []int{0}, ErrSignature},
		{"input 1 value", func(tr *Transaction) { tr.Inputs[1].Value++ }, []int{1}, ErrSignature},
		{"input 0 sequence", func(tr *Transaction) { tr.Inputs[0].Sequence-- }, []int{0, 1}, ErrSignature},
		{"input 1 sequence", func(tr *Transaction) { tr.Inputs[1].Sequence-- }, []int{0, 1}, ErrSignature},
		{"input 0 scriptCode", func(tr *Transaction) { tr.Inputs[0].ScriptPubKey[33] ^= 0x01 }, []int{0}, ErrWrongKey},
		{"input 1 outpoint vout", func(tr *Transaction) { tr.Inputs[1].Vout++ }, []int{0, 1}, ErrSignature},
		{"input 0 outpoint txid", func(tr *Transaction) { tr.Inputs[0].TxID[0] ^= 0x01 }, []int{0, 1}, ErrSignature},
		{"output 0 value", func(tr *Transaction) { tr.Outputs[0].Value++ }, []int{0, 1}, ErrSignature},
		{"output 1 script", func(tr *Transaction) { tr.Outputs[1].ScriptPubKey[5] ^= 0x01 }, []int{0, 1}, ErrSignature},
		{"version", func(tr *Transaction) { tr.Version++ }, []int{0, 1}, ErrSignature},
		{"locktime", func(tr *Transaction) { tr.LockTime++ }, []int{0, 1}, ErrSignature},
		{"input 0 signature byte", func(tr *Transaction) { tr.Inputs[0].WitnessData[0][100] ^= 0x01 }, []int{0}, ErrSignature},
		{"input 1 hashtype byte", func(tr *Transaction) { tr.Inputs[1].WitnessData[0][types.SignatureSize] = SigHashNone }, []int{1}, ErrHashType},
		{"input 0 public key byte", func(tr *Transaction) { tr.Inputs[0].WitnessData[1][7] ^= 0x01 }, []int{0}, ErrWrongKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := buildNodeVector(t, v)
			attachNodeWitness(t, v, tr)
			c.mutate(tr)
			mustFail := map[int]bool{}
			for _, i := range c.fail {
				mustFail[i] = true
			}
			for i := range tr.Inputs {
				err := tr.VerifyInput(i)
				switch {
				case mustFail[i] && err == nil:
					t.Errorf("input %d verified after the mutation", i)
				case mustFail[i] && !errors.Is(err, c.want):
					t.Errorf("input %d: got %v, want %v", i, err, c.want)
				case !mustFail[i] && err != nil:
					t.Errorf("input %d must be unaffected, got %v", i, err)
				}
			}
			// The digest of a failing input moved; the digest of an unaffected
			// input did not.
			for i := range tr.Inputs {
				if c.want != ErrSignature || tr.Inputs[i].WitnessData[0][100] != mustHex(t, v.Witness[i][0])[100] {
					continue // a witness-only mutation leaves every digest alone
				}
				got, err := tr.ComputeSigHash(i, SigHashAll)
				if err != nil {
					t.Fatal(err)
				}
				moved := hex.EncodeToString(got) != v.Digests[i]
				if moved != mustFail[i] {
					t.Errorf("input %d: digest moved=%v, verification fails=%v", i, moved, mustFail[i])
				}
			}
		})
	}
}

// With the node's soqucoin-tx available (SOQUCOIN_TX=/path/to/soqucoin-tx),
// decode the node-signed bytes with it and compare live.
func TestNodeVectorAgainstLiveNodeTool(t *testing.T) {
	bin := os.Getenv("SOQUCOIN_TX")
	if bin == "" {
		t.Skip("set SOQUCOIN_TX to the node's soqucoin-tx binary to run the live comparison")
	}
	v := loadNodeVector(t)
	out, err := exec.Command(bin, "-json", v.SignedTx).Output()
	if err != nil {
		t.Fatalf("soqucoin-tx: %v", err)
	}
	var decoded struct {
		TxID string `json:"txid"`
		Vin  []struct {
			Witness []string `json:"txinwitness"`
		} `json:"vin"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TxID != v.TxID {
		t.Fatalf("node txid %s, fixture %s", decoded.TxID, v.TxID)
	}
	for i, in := range decoded.Vin {
		if len(in.Witness) != 2 || in.Witness[0] != v.Witness[i][0] || in.Witness[1] != v.Witness[i][1] {
			t.Fatalf("node read input %d witness differently from the fixture", i)
		}
	}
}
