package withdraw

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// bech32m permits an all-upper or all-lower spelling of one address, and
// checkDestination decodes either, so a second Submit of one id with the
// other spelling is the same withdrawal, not a conflict.
func TestSubmitAcceptsTheOtherCaseSpellingOfOneDestination(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	a, created, err := e.Submit(context.Background(), "w1", dst, 1_000_000, 1000)
	if err != nil || !created {
		t.Fatalf("first submit: %v created=%v", err, created)
	}
	upper := strings.ToUpper(dst)
	b, created, err := e.Submit(context.Background(), "w1", upper, 1_000_000, 1000)
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("upper-case spelling of the same destination: %v created=%v", err, created)
	}
	if b.Address != dst {
		t.Fatalf("the stored spelling changed to %q", b.Address)
	}
	if _, _, err := e.Submit(context.Background(), "w1", dst, 2_000_000, 1000); !errors.Is(err, ErrConflict) {
		t.Errorf("same id, different amount accepted: %v", err)
	}
}
