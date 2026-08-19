// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.
//
// ScriptFor exists because callers reliably hardcode the HRP. Every builder in
// this SDK passed "ssq" unconditionally, so mainnet transactions could not be
// built at all. The script produced here is what BIP143 commits to as the
// scriptCode, which makes a wrong network a signing fault rather than a decoding
// one, so these tests check the derivation on every network and check that a
// prefix belonging to no network is refused even with a valid checksum.

package address

import (
	"bytes"
	"errors"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

func progOf(fill byte) []byte {
	p := make([]byte, 32)
	for i := range p {
		p[i] = fill
	}
	return p
}

func TestScriptForDerivesNetworkFromAddress(t *testing.T) {
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		t.Run(n.Name, func(t *testing.T) {
			prog := progOf(0xab)
			addr, err := Encode(n.HRP, 1, prog)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := ScriptFor(addr)
			if err != nil {
				t.Fatalf("ScriptFor(%s): %v", addr, err)
			}
			want := WitnessProgram(1, prog)
			if !bytes.Equal(got, want) {
				t.Errorf("script = %x, want %x", got, want)
			}
		})
	}
}

// The regression proper: the same program on two networks must yield the same
// script, because the network lives in the prefix and not in the program. If
// ScriptFor ever pinned a network, this is where it would show.
func TestScriptForIsNetworkIndependentGivenTheSameProgram(t *testing.T) {
	prog := progOf(0x5c)
	mainAddr, _ := Encode(types.Mainnet.HRP, 1, prog)
	stageAddr, _ := Encode(types.Stagenet.HRP, 1, prog)

	mainScript, err := ScriptFor(mainAddr)
	if err != nil {
		t.Fatalf("mainnet: %v", err)
	}
	stageScript, err := ScriptFor(stageAddr)
	if err != nil {
		t.Fatalf("stagenet: %v", err)
	}
	if !bytes.Equal(mainScript, stageScript) {
		t.Errorf("mainnet script %x != stagenet script %x for the same program",
			mainScript, stageScript)
	}
}

func TestScriptForRejectsUnknownPrefix(t *testing.T) {
	// A valid checksum over a prefix that is not one of ours.
	bogus, err := Encode("evil", 1, progOf(0x01))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := ScriptFor(bogus); err == nil {
		t.Fatal("accepted an address whose prefix belongs to no network")
	} else if !errors.Is(err, ErrInvalidHRP) {
		t.Errorf("error = %v, want it to wrap ErrInvalidHRP", err)
	}
}

func TestScriptForRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "1", "nodelimiter", "1tooshort", "sq1notbech32m"} {
		if _, err := ScriptFor(bad); err == nil {
			t.Errorf("ScriptFor(%q) accepted a malformed address", bad)
		}
	}
}

func TestNetworkOf(t *testing.T) {
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		addr, _ := Encode(n.HRP, 1, progOf(0x02))
		got, err := NetworkOf(addr)
		if err != nil {
			t.Fatalf("%s: %v", n.Name, err)
		}
		if got.Name != n.Name {
			t.Errorf("NetworkOf(%s) = %q, want %q", addr, got.Name, n.Name)
		}
	}
}

// Stagenet is "ssq" and mainnet is "sq". A prefix match that is not anchored to
// the full HRP would read a stagenet address as mainnet.
func TestNetworkOfDoesNotConfuseOverlappingPrefixes(t *testing.T) {
	stage, _ := Encode(types.Stagenet.HRP, 1, progOf(0x03))
	n, err := NetworkOf(stage)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if n.Name != types.Stagenet.Name {
		t.Errorf("stagenet address read as %q", n.Name)
	}
	rt, _ := Encode(types.Regtest.HRP, 1, progOf(0x04))
	n, err = NetworkOf(rt)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if n.Name != types.Regtest.Name {
		t.Errorf("regtest address read as %q", n.Name)
	}
}
