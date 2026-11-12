package withdraw

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/utxo"
)

// addrOn is a witness version 1 address on the network's prefix.
func addrOn(t *testing.T, n types.Network, fill byte) string {
	t.Helper()
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = fill
	}
	a, err := address.Encode(n.HRP, 1, prog)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A destination on another network is refused where it enters: Submit creates
// nothing for it, the error is the per-request kind the breaker ignores, and a
// destination on this network is accepted. The prefix is not part of the
// script, so nothing after Submit would refuse it.
func TestSubmitRefusesADestinationOnAnotherNetwork(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	e.Network = types.Stagenet
	for name, to := range map[string]string{
		"mainnet address":   addrOn(t, types.Mainnet, 0x22),
		"malformed":         "ssq1pdestination",
		"witness version 5": mustEncode(t, types.Stagenet.HRP, 5, 32),
	} {
		in, created, err := e.Submit(context.Background(), "w-"+name, to, 1_000_000, 1000)
		if !errors.Is(err, ErrInvalidIntent) || created || in != nil {
			t.Fatalf("%s: err %v created %v intent %v; want ErrInvalidIntent and nothing created", name, err, created, in)
		}
		if _, ok, _ := e.Store.Get(context.Background(), "w-"+name); ok {
			t.Fatalf("%s: a record was created for a refused destination", name)
		}
	}
	if _, created, err := e.Submit(context.Background(), "w-ok", addrOn(t, types.Stagenet, 0x33), 1_000_000, 1000); err != nil || !created {
		t.Fatalf("stagenet destination on a stagenet engine: %v created %v", err, created)
	}
}

// An engine with no Network is mainnet, as a Monitor with none is.
func TestEngineNetworkUnsetIsMainnet(t *testing.T) {
	e := newEngine(t, NewMemStore(), utxo.NewSpentSet("", nil), &fakeNet{mode: "ok"}, coins())
	e.Network = types.Network{}
	if _, _, err := e.Submit(context.Background(), "w-main", addrOn(t, types.Mainnet, 0x22), 1_000_000, 1000); err != nil {
		t.Fatalf("mainnet destination on an unset engine: %v", err)
	}
	if _, _, err := e.Submit(context.Background(), "w-stage", addrOn(t, types.Stagenet, 0x22), 1_000_000, 1000); !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("stagenet destination on an unset engine: %v, want ErrInvalidIntent", err)
	}
}

// A Created record whose destination is on another network came from a
// Submit that did not check: another release, or an engine on another host
// bound to another network. Build is where it would be signed, so Build
// refuses it there: the intent is Failed with the cause recorded, nothing
// was reserved, and Process reports it as failed from then on.
func TestBuildFailsAStoredIntentWhoseDestinationIsOnAnotherNetwork(t *testing.T) {
	store := NewMemStore()
	spent := utxo.NewSpentSet("", nil)
	net := &fakeNet{mode: "ok"}
	e := newEngine(t, store, spent, net, coins())
	e.Network = types.Stagenet
	foreign := &Intent{ID: "w-foreign", Address: addrOn(t, types.Mainnet, 0x44), Amount: 1_000_000, FeeRate: 1000, State: StateCreated}
	if err := store.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	err := e.Build(context.Background(), foreign)
	if !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("build: %v, want ErrInvalidIntent", err)
	}
	stored, _, _ := store.Get(context.Background(), "w-foreign")
	if stored.State != StateFailed || !strings.Contains(stored.LastError, foreign.Address) {
		t.Fatalf("stored %s with LastError %q; want Failed naming the destination", stored.State, stored.LastError)
	}
	if len(spent.ReservedIntents()) != 0 || net.builds != 0 {
		t.Fatalf("reserved %v, builds %d; want nothing reserved and the signer never called", spent.ReservedIntents(), net.builds)
	}
	if _, err := e.Process(context.Background(), "w-foreign"); !errors.Is(err, ErrFailed) {
		t.Fatalf("process after the refusal: %v, want ErrFailed", err)
	}
}

// A witness version 6 address names a script output and is no payment
// destination. The string address.EncodeWitnessV6 writes is refused where a
// destination enters and again where it would be signed, on each network, in
// lower and in upper case: Submit records nothing, and Build fails a stored
// Created record that holds one, reserves nothing and never calls the signer.
func TestEngineRefusesAWitnessV6Destination(t *testing.T) {
	ctx := context.Background()
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		written, err := address.EncodeWitnessV6(n.HRP, make([]byte, address.WitnessProgramSize))
		if err != nil {
			t.Fatalf("%s: EncodeWitnessV6: %v", n.Name, err)
		}
		for _, to := range []string{written, strings.ToUpper(written)} {
			store := NewMemStore()
			spent := utxo.NewSpentSet("", nil)
			net := &fakeNet{mode: "ok"}
			e := newEngine(t, store, spent, net, coins())
			e.Network = n

			in, created, err := e.Submit(ctx, "w-submitted", to, 1_000_000, 1000)
			if !errors.Is(err, ErrInvalidIntent) || created || in != nil {
				t.Fatalf("%s, submit %s: err %v created %v intent %v; want ErrInvalidIntent and nothing created",
					n.Name, to, err, created, in)
			}
			if _, ok, _ := store.Get(ctx, "w-submitted"); ok {
				t.Fatalf("%s, submit %s: a record was created for a refused destination", n.Name, to)
			}

			stored := &Intent{ID: "w-stored", Address: to, Amount: 1_000_000, FeeRate: 1000, State: StateCreated}
			if err := store.Create(ctx, stored); err != nil {
				t.Fatal(err)
			}
			if err := e.Build(ctx, stored); !errors.Is(err, ErrInvalidIntent) {
				t.Fatalf("%s, build %s: %v, want ErrInvalidIntent", n.Name, to, err)
			}
			got, _, _ := store.Get(ctx, "w-stored")
			if got.State != StateFailed || !strings.Contains(got.LastError, to) {
				t.Fatalf("%s, build %s: stored %s with LastError %q; want Failed naming the destination",
					n.Name, to, got.State, got.LastError)
			}
			if len(spent.ReservedIntents()) != 0 || net.builds != 0 {
				t.Fatalf("%s, build %s: reserved %v, builds %d; want nothing reserved and the signer never called",
					n.Name, to, spent.ReservedIntents(), net.builds)
			}
		}
	}
}

func mustEncode(t *testing.T, hrp string, witVer byte, n int) string {
	t.Helper()
	a, err := address.Encode(hrp, witVer, make([]byte, n))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
