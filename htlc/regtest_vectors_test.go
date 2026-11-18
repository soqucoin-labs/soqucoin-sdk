// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package htlc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// testdata/regtest_spends.json holds one claim and one refund that a node
// accepted and mined: the integration harness (integration/htlc_test.go) built
// them with this package, sent them to a regtest node and wrote them out once
// each was in a block. The file's "source" line names the node build. The
// refund's lock height is 130, the two-byte script number with the added
// zero.
//
// What this holds without a node: the transaction rebuilt from the recorded
// fields serializes to the bytes the node mined, so the witness layout, the
// sequence numbers and the lock time written here are the ones a node took;
// and VerifySpend accepts both, so the verifier does not refuse a spend the
// node accepts in this package's form. The signatures are one run's output,
// since signing is randomised.
type minedSpend struct {
	Kind       string   `json:"kind"`
	Hash       string   `json:"hash"`
	PayeeHash  string   `json:"payee_key_hash"`
	PayerHash  string   `json:"payer_key_hash"`
	LockHeight uint32   `json:"lock_height"`
	FundedTxID string   `json:"funded_txid"`
	FundedVout uint32   `json:"funded_vout"`
	Value      int64    `json:"funded_value"`
	OutValue   int64    `json:"output_value"`
	OutScript  string   `json:"output_script"`
	Witness    []string `json:"witness"`
	TxID       string   `json:"txid"`
	Raw        string   `json:"raw"`
	MinedAt    int64    `json:"mined_at_height"`
}

func loadMinedSpends(t *testing.T) []minedSpend {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "regtest_spends.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Source string       `json:"source"`
		Spends []minedSpend `json:"spends"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Source == "" || len(file.Spends) != 2 || file.Spends[0].Kind != "claim" || file.Spends[1].Kind != "refund" {
		t.Fatalf("fixture shape: source %q, %d spends", file.Source, len(file.Spends))
	}
	return file.Spends
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rebuild makes the recorded spend again from its fields through the
// package's own constants and the transaction's constructors.
func (m minedSpend) rebuild(t *testing.T) (Params, *tx.Transaction) {
	t.Helper()
	var p Params
	copy(p.Hash[:], unhex(t, m.Hash))
	copy(p.PayeeKeyHash[:], unhex(t, m.PayeeHash))
	copy(p.PayerKeyHash[:], unhex(t, m.PayerHash))
	p.LockHeight = Height(m.LockHeight)
	out, err := p.OutputScript()
	if err != nil {
		t.Fatal(err)
	}
	tr := tx.NewTransaction()
	if err := tr.AddInput(types.UTXO{TxID: m.FundedTxID, Vout: m.FundedVout, Value: m.Value}, out); err != nil {
		t.Fatal(err)
	}
	if m.Kind == "claim" {
		tr.Inputs[0].Sequence = ClaimSequence
	} else {
		tr.Inputs[0].Sequence, tr.LockTime = RefundSequence, m.LockHeight
	}
	tr.AddOutput(m.OutValue, unhex(t, m.OutScript))
	for _, item := range m.Witness {
		tr.Inputs[0].WitnessData = append(tr.Inputs[0].WitnessData, unhex(t, item))
	}
	return p, tr
}

func TestSpendsANodeMinedAreTheBytesWrittenHereAndVerify(t *testing.T) {
	for _, m := range loadMinedSpends(t) {
		p, tr := m.rebuild(t)
		if got := tr.SerializeHex(); got != m.Raw {
			t.Errorf("%s: the rebuilt transaction is not the one the node mined\nSDK  %s\nnode %s", m.Kind, got, m.Raw)
		}
		if got := tr.TxID(); got != m.TxID {
			t.Errorf("%s: txid %s, the node's %s", m.Kind, got, m.TxID)
		}
		script := tr.Inputs[0].WitnessData[len(tr.Inputs[0].WitnessData)-2]
		if parsed, err := ParseScript(script); err != nil || parsed != p {
			t.Errorf("%s: the mined witnessScript parses to %+v, %v", m.Kind, parsed, err)
		}
		if err := VerifySpend(tr, p, m.Value); err != nil {
			t.Errorf("%s: VerifySpend refuses a spend the node mined: %v", m.Kind, err)
		}
		// The value is part of what was signed: the same spend of an output
		// worth one shor more does not verify.
		tr.Inputs[0].Value++
		if err := VerifySpend(tr, p, m.Value+1); !errors.Is(err, tx.ErrSignature) {
			t.Errorf("%s: with another value: %v, want tx.ErrSignature", m.Kind, err)
		}
	}
}
