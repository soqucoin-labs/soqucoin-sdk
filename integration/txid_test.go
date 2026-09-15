//go:build integration

package integration

import "testing"

// The lying broadcaster only tests what it claims to if its fake txid is never
// the real one. This runs without a node so the property is not left to the one
// run in sixteen where a real txid starts with "f".
func TestDifferentTxIDNeverReturnsItsInput(t *testing.T) {
	for _, got := range []string{
		"f56bdff434d7f73d4f9c30800aedd0065af1e97abcf3bdae0dec49859aa679f8",
		"056bdff434d7f73d4f9c30800aedd0065af1e97abcf3bdae0dec49859aa679f8",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"0000000000000000000000000000000000000000000000000000000000000000",
		"",
	} {
		fake := differentTxID(got)
		if fake == got {
			t.Errorf("differentTxID(%q) returned its input", got)
		}
		if len(got) > 0 && len(fake) != len(got) {
			t.Errorf("differentTxID(%q) changed the length to %d", got, len(fake))
		}
	}
}
