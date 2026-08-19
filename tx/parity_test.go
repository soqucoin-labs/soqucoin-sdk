package tx

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Byte-identical parity with the production soq-signer encoder. Generated from
// soq-signer/internal/txbuilder's serializeAllOutputs semantics.
func TestParityWithProductionSigner(t *testing.T) {
	cases := []struct {
		name  string
		value int64
		spk   []byte
		want  string
	}{
		{"golden OP_TRUE", 12345678, []byte{0x51}, "4e61bc00000000000151"},
		{"v1 P2WPKH", 500000, ScriptP2WPKH(fill32(0xab)), "20a1070000000000225120" + rep("ab", 32)},
		{"v7 USDSOQ", 250000, ScriptV7USDSOQHolding(fill32(0xcd)), "90d0030000000000225720" + rep("cd", 32)},
		{"v5 authority", 0, ScriptWitnessV5(fill32(0xef)), "0000000000000000225520" + rep("ef", 32)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hex.EncodeToString(serializeAllOutputs([]TxOutput{{Value: c.value, ScriptPubKey: c.spk}}))
			if got != c.want {
				t.Errorf("SDK  = %s\nprod = %s", got, c.want)
			}
		})
	}
}

func fill32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
func rep(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
