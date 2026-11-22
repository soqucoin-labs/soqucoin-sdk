//go:build integration

package integration

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The push scenarios believe the fake indexer, and that is only safe while the
// fake answers every method electrumx.Client sends. The two drift apart
// silently: a method the client sends and the fake does not answer meets
// "unknown method", which the client reads as an error from the server, so a
// scenario that reaches that path takes the error branch and reports success
// without exercising what it names.
//
// So the client's outbound call sites are read out of its own source
// (callsites_test.go) and compared with the handlers in indexerMethods. The
// reading has its own corpus in callsites_corpus_test.go, because it has been
// wrong twice; a guard nobody has seen fail is a reminder.
//
// These tests need no node and run in milliseconds.

// clientMethodFloor is how many distinct methods the reading finds today. It
// guards against a reading that has stopped reading: rename a request method
// and the count drops, which fails here instead of leaving a test that asserts
// nothing. It does not catch a single missed call site, and is not relied on
// for that — the corpus is.
const clientMethodFloor = 9

func TestTheFakeIndexerAnswersEveryMethodTheClientSends(t *testing.T) {
	sent := methodsTheClientSends(t)
	if len(sent) < clientMethodFloor {
		t.Fatalf("found %d methods in the electrumx client (%v), want at least %d: the call sites are no longer being read, so this test is checking nothing",
			len(sent), sent, clientMethodFloor)
	}
	for _, m := range sent {
		if _, ok := indexerMethods[m]; !ok {
			t.Errorf("electrumx.Client sends %s and the fake indexer has no handler for it: a scenario reaching that path meets an error, not a server", m)
		}
	}
}

// The reverse direction is not an error but it is worth knowing about: a
// handler for a method the client no longer sends is dead weight in the
// double, and a reader will take it for a path under test.
func TestTheFakeIndexerHasNoHandlerTheClientNeverUses(t *testing.T) {
	sent := map[string]bool{}
	for _, m := range methodsTheClientSends(t) {
		sent[m] = true
	}
	for m := range indexerMethods {
		if !sent[m] {
			t.Errorf("the fake indexer answers %s, which electrumx.Client no longer sends: remove the handler or the reader will read it as a path a scenario covers", m)
		}
	}
}

// The double's selection is the indexer's, case for case. The rows and the
// kept lists are those of the indexer's own tests of bounded_utxos
// (tests/server/test_utxo_cache.py at its commit 3c6dcb2), so a scenario that
// reads a bounded list from the double reads what a server would send.
func TestTheFakeIndexersBoundedSelectionIsTheIndexers(t *testing.T) {
	row := func(tx byte, pos uint32, height, value int64) types.UTXO {
		return types.UTXO{TxID: strings.Repeat("0", 62) + hex.EncodeToString([]byte{tx}), Vout: pos, Height: height, Value: value}
	}
	type kept struct {
		tx     byte
		pos    uint32
		height int64
	}
	// Three confirmed outputs of one transaction and two pending of another.
	passedOver := []types.UTXO{row(1, 0, 100, 50), row(1, 1, 100, 50), row(1, 2, 100, 50), row(2, 0, 0, 10), row(2, 1, 0, 11)}
	cases := []struct {
		name  string
		rows  []types.UTXO
		limit int
		want  []kept
	}{
		{"a confirmed transaction that does not fit gives its places to a pending one that does",
			passedOver, 2, []kept{{2, 1, 0}, {2, 0, 0}}},
		{"the confirmed transaction fits and nothing pending is listed",
			passedOver, 3, []kept{{1, 0, 100}, {1, 1, 100}, {1, 2, 100}}},
		{"the place left goes to the next output of the walk",
			passedOver, 4, []kept{{1, 0, 100}, {1, 1, 100}, {1, 2, 100}, {2, 1, 0}}},
		{"no transaction fits one place",
			passedOver, 1, []kept{{1, 0, 100}}},
		{"the kept list is ordered by value whatever the height",
			[]types.UTXO{row(1, 0, 100, 10), row(2, 0, 0, 1000), row(3, 0, 100, 5), row(4, 0, 0, 500)}, 3,
			[]kept{{2, 0, 0}, {1, 0, 100}, {3, 0, 100}}},
		{"places left after the walk go to confirmed outputs before larger pending ones",
			[]types.UTXO{row(1, 0, 100, 100), row(2, 0, 100, 5), row(2, 1, 100, 5), row(2, 2, 100, 5), row(3, 0, 0, 1000), row(3, 1, 0, 1000), row(3, 2, 0, 1000)}, 2,
			[]kept{{1, 0, 100}, {2, 0, 100}}},
		{"a transaction keeps its outputs together while they fit",
			[]types.UTXO{row(1, 0, 100, 50), row(1, 1, 100, 50), row(1, 2, 100, 50), row(2, 0, 100, 60), row(3, 0, 100, 40)}, 4,
			[]kept{{2, 0, 100}, {1, 0, 100}, {1, 1, 100}, {1, 2, 100}}},
		{"a list within the limit comes back as given",
			[]types.UTXO{row(3, 0, 0, 5), row(1, 0, 100, 10)}, 2, []kept{{3, 0, 0}, {1, 0, 100}}},
	}
	for _, tc := range cases {
		got := boundedUTXOs(append([]types.UTXO(nil), tc.rows...), tc.limit)
		if len(got) != len(tc.want) {
			t.Errorf("%s: kept %d outputs, want %d: %+v", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i, w := range tc.want {
			if g := got[i]; g.TxID != row(w.tx, 0, 0, 0).TxID || g.Vout != w.pos || g.Height != w.height {
				t.Errorf("%s: entry %d is %s:%d at height %d, want transaction %d:%d at height %d", tc.name, i, g.TxID[60:], g.Vout, g.Height, w.tx, w.pos, w.height)
			}
		}
	}
}

// methodsTheClientSends is the reading applied to the electrumx package. A
// call site it could not resolve fails the test: that site is precisely the one
// whose coverage would otherwise go unchecked in silence.
func methodsTheClientSends(t *testing.T) []string {
	t.Helper()
	got, err := readCallSites(filepath.Join("..", "electrumx"), "Client")
	if err != nil {
		t.Fatalf("read the electrumx call sites: %v", err)
	}
	if len(got.problems) != 0 {
		t.Errorf("the electrumx package has call sites this reading cannot resolve, so the fake's coverage of them is unchecked:\n  %s",
			strings.Join(got.problems, "\n  "))
	}
	return got.methods
}
