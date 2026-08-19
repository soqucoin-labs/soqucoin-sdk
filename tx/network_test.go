// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// The builders used to call soqaddr.Decode("ssq", …), hardcoding the STAGENET
// prefix. Decode rejects a mismatched prefix, so every builder failed on every
// mainnet address with "expected ssq, got sq" — a message that reads like a bad
// address rather than a wrong assumption in the SDK. The SDK could not construct
// a mainnet transaction at all, and no test noticed because every fixture in the
// suite was a stagenet address.
//
// These tests run the same construction against every supported network.

package tx

import (
	"strings"
	"testing"

	soqaddr "github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// addrFor builds a real bech32m address for the given network so the fixtures
// carry valid checksums rather than hand-written strings.
func addrFor(t *testing.T, hrp string, fill byte) string {
	t.Helper()
	a, err := soqaddr.Encode(hrp, 1, hash32(fill))
	if err != nil {
		t.Fatalf("encode %s address: %v", hrp, err)
	}
	return a
}

// The regression. Every network must build, not just stagenet.
func TestBuildSendTransactionWorksOnEveryNetwork(t *testing.T) {
	for _, net := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		t.Run(net.Name, func(t *testing.T) {
			from := addrFor(t, net.HRP, 0x11)
			in := []types.UTXO{{
				TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: from,
			}}
			tr, err := BuildSendTransaction(in,
				ScriptP2WPKH(hash32(0x22)), 1_000_000_000,
				ScriptP2WPKH(hash32(0x33)), 10)
			if err != nil {
				t.Fatalf("%s: BuildSendTransaction failed: %v", net.Name, err)
			}
			if len(tr.Inputs) != 1 {
				t.Fatalf("inputs = %d, want 1", len(tr.Inputs))
			}
			// The scriptCode BIP143 commits to must be the witness program for
			// this address, on this network.
			wantVer, wantProg, err := soqaddr.Decode(net.HRP, from)
			if err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			want := soqaddr.WitnessProgram(wantVer, wantProg)
			if string(tr.Inputs[0].ScriptPubKey) != string(want) {
				t.Errorf("%s: input scriptPubKey = %x, want %x",
					net.Name, tr.Inputs[0].ScriptPubKey, want)
			}
			// And it must actually be signable.
			if _, err := tr.ComputeSigHash(0, SigHashAll); err != nil {
				t.Errorf("%s: ComputeSigHash failed: %v", net.Name, err)
			}
		})
	}
}

// Mixing networks in one transaction is never legitimate. Building it silently
// would spend against the wrong chain's scriptCode.
func TestBuildRejectsMixedNetworks(t *testing.T) {
	mainAddr := addrFor(t, types.Mainnet.HRP, 0x11)
	stageAddr := addrFor(t, types.Stagenet.HRP, 0x22)

	in := []types.UTXO{
		{TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: mainAddr},
		{TxID: "ff" + displayTxID[2:], Vout: 1, Value: 5_000_000_000, Address: stageAddr},
	}
	_, err := BuildSendTransaction(in, ScriptP2WPKH(hash32(0x33)), 1_000_000_000,
		ScriptP2WPKH(hash32(0x44)), 10)
	if err == nil {
		t.Fatal("built a transaction mixing mainnet and stagenet inputs")
	}
	if !strings.Contains(err.Error(), "mix networks") {
		t.Errorf("error = %q, want it to name the mixed-network cause", err)
	}
}

// A prefix that is not one of our networks must be refused even if its checksum
// is valid, otherwise a fabricated prefix would be accepted.
func TestBuildRejectsUnknownNetworkPrefix(t *testing.T) {
	bogus, err := soqaddr.Encode("evil", 1, hash32(0x55))
	if err != nil {
		t.Fatalf("encode bogus address: %v", err)
	}
	in := []types.UTXO{{TxID: displayTxID, Vout: 0, Value: 5_000_000_000, Address: bogus}}
	_, err = BuildSendTransaction(in, ScriptP2WPKH(hash32(0x66)), 1_000_000_000,
		ScriptP2WPKH(hash32(0x77)), 10)
	if err == nil {
		t.Fatal("accepted an address with an unknown network prefix")
	}
	if !strings.Contains(err.Error(), "unknown network prefix") {
		t.Errorf("error = %q, want it to name the unknown prefix", err)
	}
}

func TestHRPOf(t *testing.T) {
	for _, net := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		a := addrFor(t, net.HRP, 0x01)
		got, err := soqaddr.HRPOf(a)
		if err != nil {
			t.Fatalf("%s: %v", net.Name, err)
		}
		if got != net.HRP {
			t.Errorf("HRPOf(%s) = %q, want %q", a, got, net.HRP)
		}
	}
	for _, bad := range []string{"", "1", "nodelimiter", "1tooshort"} {
		if _, err := soqaddr.HRPOf(bad); err == nil {
			t.Errorf("HRPOf(%q) accepted a malformed address", bad)
		}
	}
}
