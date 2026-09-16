package electrumx

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// setHRP pins the network prefix in a test and fails the test if the client
// refuses it. Tests used to assign the field directly, which is the race
// SetHRP closes; going through the setter here keeps the refusals on the path
// every test takes.
func setHRP(tb testing.TB, c *Client, hrp string) {
	tb.Helper()
	if err := c.SetHRP(hrp); err != nil {
		tb.Fatalf("SetHRP(%q): %v", hrp, err)
	}
}

// A prefix no supported network uses can only produce script hashes no server
// answers for, so the client refuses it at the setter rather than refreshing
// nothing for the life of the process. The prefix must be left unset, so that
// TrackAddresses still infers it from the addresses themselves.
func TestSetHRPRefusesAPrefixNoNetworkUses(t *testing.T) {
	c := NewClient("127.0.0.1:1", time.Second, nil)
	err := c.SetHRP("zzq")
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("SetHRP(\"zzq\") returned %v, want ErrNetworkMismatch", err)
	}
	if got := c.hrp(); got != "" {
		t.Fatalf("the refused prefix was stored: hrp() is %q", got)
	}

	a := craftAddr(t, 0x31)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatalf("track after a refused prefix: %v", err)
	}
	if got := c.hrp(); got != types.Stagenet.HRP {
		t.Fatalf("hrp() is %q after tracking a stagenet address, want %q", got, types.Stagenet.HRP)
	}
}

// Moving a client between networks once addresses are tracked is refused: the
// UTXO cache, the subscriptions and every script hash were derived under the
// old prefix, so the client would be asking one network's server about another
// network's keys and quietly answering "no coins" for every address.
func TestSetHRPRefusesMovingATrackedClientToAnotherNetwork(t *testing.T) {
	c := NewClient("127.0.0.1:1", time.Second, nil)
	a := craftAddr(t, 0x41)
	if err := c.TrackAddresses([]string{a}); err != nil {
		t.Fatal(err)
	}

	err := c.SetHRP(types.Mainnet.HRP)
	if !errors.Is(err, ErrNetworkMismatch) {
		t.Fatalf("SetHRP(mainnet) on a tracked stagenet client returned %v, want ErrNetworkMismatch", err)
	}
	if got := c.hrp(); got != types.Stagenet.HRP {
		t.Fatalf("hrp() is %q, want the tracked network %q", got, types.Stagenet.HRP)
	}
	if got := len(c.GetUTXOs(a)); got != 0 {
		t.Fatalf("GetUTXOs returned %d outputs for an address with none", got)
	}

	// The same prefix again is not a change and is accepted, so a caller that
	// pins the network at startup and again after a reconnect is not refused.
	if err := c.SetHRP(types.Stagenet.HRP); err != nil {
		t.Fatalf("SetHRP with the prefix already in force: %v", err)
	}
}

// The prefix is written under the lock every reader of it takes, so pinning it
// on a running client is safe. Under -race an unlocked write against the
// refresher's read is reported; this is the test that reports it.
func TestSetHRPIsSafeAgainstAConcurrentRead(t *testing.T) {
	c := NewClient("127.0.0.1:1", time.Second, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	// The prefix alternates, so every iteration is a real write rather than
	// the "already in force" early return: one write is a race the detector
	// can miss.
	prefixes := []string{types.Stagenet.HRP, types.Mainnet.HRP}
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := c.SetHRP(prefixes[i%2]); err != nil {
				t.Errorf("SetHRP: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = c.hrp()
		}
	}()
	wg.Wait()
}
