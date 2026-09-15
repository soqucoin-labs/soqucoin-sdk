package deposit

import (
	"context"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// sweepingLedger follows the Ledger contract to the letter: Pending leaves out
// outputs the exchange has spent itself. A swept, credited, non-final output
// is therefore absent from Pending while still within the reorganisation
// horizon.
type sweepingLedger struct {
	*fakeLedger
	swept map[string]bool
}

func (l *sweepingLedger) Pending(ctx context.Context) ([]Deposit, error) {
	all, err := l.fakeLedger.Pending(ctx)
	if err != nil {
		return nil, err
	}
	var out []Deposit
	for _, d := range all {
		if !l.swept[key(d.TxID, d.Vout)] {
			out = append(out, d)
		}
	}
	return out, nil
}

// Absence from Pending is not finality. A credited output the ledger leaves
// out of Pending because the exchange swept it, still within the horizon by
// the node's word, does not enter the final set; when its credit is reversed
// after a reorganisation and the output is mined again, it is credited again.
func TestSweptOutputAbsentFromPendingIsNotFinal(t *testing.T) {
	m, cache, node, led, _, a := setup(t)
	sl := &sweepingLedger{fakeLedger: led, swept: map[string]bool{}}
	m.Ledger = sl
	cache.utxos[a] = []types.UTXO{{TxID: "aa", Vout: 0, Value: 1000, Height: node.tip - 40, Address: a}}
	node.outs[key("aa", 0)] = txout(t, a, 1000, 41, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("credit: %v %+v", err, got)
	}
	// The exchange sweeps the deposit; the ledger leaves it out of Pending.
	// The indexer still lists the output (the sweep is not yet indexed).
	sl.swept[key("aa", 0)] = true
	for i := 0; i < 3; i++ {
		if _, err := m.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if m.finalCount() != 0 {
		t.Fatal("an output within the horizon entered the final set because Pending left it out")
	}
	// A reorganisation within the horizon: the operator reverses the credit;
	// the output is mined again. It must be credited again.
	delete(led.credited, key("aa", 0))
	delete(sl.swept, key("aa", 0))
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("re-credit after a reversal within the horizon: %v %+v", err, got)
	}
	// Past the horizon by the node's word, the same output is final.
	node.outs[key("aa", 0)] = txout(t, a, 1000, types.MaxReorgDepth+1, false)
	sl.swept[key("aa", 0)] = true // absent from Pending again
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.finalCount() != 1 {
		t.Fatal("a credited output past the horizon by the node's word did not enter the final set")
	}
}
