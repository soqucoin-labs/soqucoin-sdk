package tx

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// Whole-transaction fixture verified by the node.
//
// The transaction below was built by this SDK from fixed inputs, outputs and
// witness bytes, then decoded with the node's own soqucoin-tx (built from the
// v2.3.0 tree). The node computed the same txid and read two inputs, two
// outputs and witness stacks of [2421, 1313] bytes, which pins whole-transaction
// serialization and txid derivation to consensus, not to this package's own
// reading of itself. The earlier golden test pinned only the CTxOut format.
//
// Regenerate only if the wire format changes on purpose, and re-verify with
// soqucoin-tx before committing the new values.
const (
	goldenTxID   = "6c08d9d8d9320ece061d097c73932205e0eca88839c297b46b9a9c1ba605c7cb"
	goldenAddr1  = "sq1pzyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygseyhe8k"
	goldenAddr2  = "sq1pyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3q2sfv76"
	goldenChange = 2_232_517_890 // 5e9 + 1,234,567,890 - 4e9 - (2049+1) vB x 1000
)

// goldenSigner emits fixed-content material of the exact consensus sizes.
type goldenSigner struct{}

func (goldenSigner) Sign(string, []byte) ([]byte, error) {
	s := make([]byte, DilithiumSigSize)
	for i := range s {
		s[i] = byte(0xA0 + i%16)
	}
	return s, nil
}
func (goldenSigner) PublicKeyFor(string) ([]byte, error) {
	p := make([]byte, DilithiumPubKeySize)
	for i := range p {
		p[i] = byte(0x10 + i%16)
	}
	return p, nil
}

func goldenBuild(t *testing.T) (*Transaction, string) {
	t.Helper()
	in := []types.UTXO{
		{TxID: "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", Vout: 0, Value: 5_000_000_000, Address: goldenAddr1},
		{TxID: "f0e0d0c0b0a090807060504030201000ffeeddccbbaa99887766554433221100", Vout: 3, Value: 1_234_567_890, Address: goldenAddr2},
	}
	change, err := address.ScriptFor(goldenAddr1)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := BuildSendTransaction(in, ScriptP2WPKH(hash32(0x33)), 4_000_000_000, change, types.RecommendedFeeRate)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SignAll(goldenSigner{}); err != nil {
		t.Fatal(err)
	}
	return tr, tr.SerializeHex()
}

func TestGoldenTransactionMatchesNodeDecode(t *testing.T) {
	tr, raw := goldenBuild(t)
	if got := tr.TxID(); got != goldenTxID {
		t.Fatalf("txid %s, node computed %s for these bytes", got, goldenTxID)
	}
	if len(tr.Outputs) != 2 || tr.Outputs[1].Value != goldenChange {
		t.Fatalf("outputs %+v, want change %d", tr.Outputs, goldenChange)
	}
	if tr.VSize() != 2049 {
		t.Fatalf("vsize %d, want 2049", tr.VSize())
	}
	if len(raw) != 15324 {
		t.Fatalf("serialized %d hex chars, want 15324", len(raw))
	}
	for i, in := range tr.Inputs {
		if len(in.WitnessData) != 2 || len(in.WitnessData[0]) != 2421 || len(in.WitnessData[1]) != 1313 {
			t.Fatalf("input %d witness sizes wrong: %d items", i, len(in.WitnessData))
		}
	}
	// The txid is the double SHA-256 of the non-witness form; pin that the
	// non-witness bytes are a strict prefix-compatible subset (same version,
	// inputs, outputs, locktime) by checking their hash directly.
	if got := hex.EncodeToString(sha256d(tr.serializeNoWitness())); got != reverseHex(goldenTxID) {
		t.Fatalf("non-witness hash %s does not derive the golden txid", got)
	}
}

// When the node's soqucoin-tx is available (SOQUCOIN_TX=/path/to/soqucoin-tx),
// decode the bytes with it and compare live rather than against the recorded
// values. CI does not have the node; the recorded fixture stands in for it.
func TestGoldenTransactionAgainstLiveNodeTool(t *testing.T) {
	bin := os.Getenv("SOQUCOIN_TX")
	if bin == "" {
		t.Skip("set SOQUCOIN_TX to the node's soqucoin-tx binary to run the live comparison")
	}
	_, raw := goldenBuild(t)
	out, err := exec.Command(bin, "-json", raw).Output()
	if err != nil {
		t.Fatalf("soqucoin-tx: %v", err)
	}
	var decoded struct {
		TxID string `json:"txid"`
		Vin  []struct {
			Witness []string `json:"txinwitness"`
		} `json:"vin"`
		Vout []struct {
			Value float64 `json:"value"`
		} `json:"vout"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TxID != goldenTxID {
		t.Fatalf("node txid %s, SDK %s", decoded.TxID, goldenTxID)
	}
	if len(decoded.Vin) != 2 || len(decoded.Vout) != 2 {
		t.Fatalf("node decoded %d inputs, %d outputs", len(decoded.Vin), len(decoded.Vout))
	}
	for i, in := range decoded.Vin {
		if len(in.Witness) != 2 || len(in.Witness[0])/2 != 2421 || len(in.Witness[1])/2 != 1313 {
			t.Fatalf("node read input %d witness as %d items", i, len(in.Witness))
		}
	}
}

func reverseHex(h string) string {
	b, _ := hex.DecodeString(h)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}
