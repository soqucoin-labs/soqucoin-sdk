package deposit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// ── fakes ──────────────────────────────────────────────────────────────────

// fakeChain is the node as Tiers reads it.
type fakeChain struct {
	recent, day                float64
	tips                       []rpc.ChainTip
	work                       map[string]int64 // by block hash; a hash that is not here has no work
	recentErr, dayErr, tipsErr error
	workErr                    map[string]error
	workCalls                  []string
	hashCalls                  []int
}

func (c *fakeChain) GetNetworkHashPS(_ context.Context, blocks int) (float64, error) {
	c.hashCalls = append(c.hashCalls, blocks)
	if blocks == recentBlocks {
		return c.recent, c.recentErr
	}
	return c.day, c.dayErr
}

func (c *fakeChain) GetChainTips(context.Context) ([]rpc.ChainTip, error) { return c.tips, c.tipsErr }

func (c *fakeChain) GetChainWork(_ context.Context, hash string) (*big.Int, error) {
	c.workCalls = append(c.workCalls, hash)
	if err := c.workErr[hash]; err != nil {
		return nil, err
	}
	work, ok := c.work[hash]
	if !ok {
		return nil, nil
	}
	return big.NewInt(work), nil
}

// fakeBook is the exchange's records as the ceiling reads them.
type fakeBook struct {
	list  []Deposit
	err   error
	asked int
	onAsk func() // runs while the book is read, when set
}

func (b *fakeBook) NotFinal(context.Context) ([]Deposit, error) {
	b.asked++
	if b.onAsk != nil {
		b.onAsk()
	}
	return b.list, b.err
}

// NotFinal makes the test ledger a Book. It sweeps nothing, so what is not
// final is what Pending lists.
func (l *fakeLedger) NotFinal(ctx context.Context) ([]Deposit, error) { return l.Pending(ctx) }

const activeWork = 1_000_000

// settled is a node with one chain tip, the active one, and a steady hashrate.
func settled(tip int64) *fakeChain {
	return &fakeChain{recent: 100, day: 100,
		tips: []rpc.ChainTip{{Height: tip, Hash: "active", Status: rpc.ChainTipActive}},
		work: map[string]int64{"active": activeWork}}
}

// with adds a branch that ends at height, length blocks off the active chain,
// carrying that much work.
func (c *fakeChain) with(hash string, height, length int64, status string, work int64) *fakeChain {
	c.tips = append(c.tips, rpc.ChainTip{Height: height, Hash: hash, BranchLen: length, Status: status})
	c.work[hash] = work
	return c
}

var testLines = TierLines{SmallBelow: 100, LargeFrom: 1000, Ceiling: 250}

func newTestTiers(t *testing.T, chain ChainReader, book Book) (*Tiers, *time.Time) {
	t.Helper()
	tiers, err := NewTiers(chain, book, testLines)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	tiers.now = func() time.Time { return now }
	return tiers, &now
}

func refreshed(t *testing.T, chain ChainReader, book Book) *Tiers {
	t.Helper()
	tiers, _ := newTestTiers(t, chain, book)
	if err := tiers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return tiers
}

// dep is deposit number n: its own outpoint, that value, in the block at
// height, with that many confirmations, as the node reported them at the tip
// of a settled chain.
func dep(n int, value, height, confs int64) Deposit {
	return Deposit{TxID: fmt.Sprintf("%064x", n), Value: value, Height: height, Confirmations: confs, BestBlock: "active"}
}

// ── the ceiling ────────────────────────────────────────────────────────────

// The attack the ceiling is for: value split over many small deposits that
// reach their tier in one pass. Each is below the first line and none is near
// the ceiling alone. The pass credits them up to the ceiling and no further,
// and the rest are credited at the horizon.
func TestManySmallDepositsInOnePassAreCreditedOnlyUpToTheCeiling(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip)
	node.best = "active"
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	var txids []string
	for i := 1; i <= 5; i++ {
		txid := fmt.Sprintf("%064x", i)
		txids = append(txids, txid)
		cache.utxos[a] = append(cache.utxos[a], types.UTXO{TxID: txid, Vout: 0, Value: 90, Height: 991, Address: a})
		node.outs[key(txid, 0)] = txout(t, a, 90, 10, false)
	}

	// 90 and 90 are 180; a third would make 270, over the ceiling of 250.
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 2 || len(led.credited) != 2 {
		t.Fatalf("the first pass credited %d of five deposits of 90 under a ceiling of 250, want 2 (err %v)", len(got), err)
	}
	if st := tiers.Status(); st.Exposed != 180 {
		t.Errorf("after two credits of 90 the value below the horizon reads %d, want 180", st.Exposed)
	}
	// The next pass reads the two from the book and holds the others again.
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 0 || len(led.credited) != 2 {
		t.Fatalf("the second pass credited %d more, want none (err %v)", len(got), err)
	}
	if len(al.kinds) != 0 {
		t.Errorf("a deposit held under the ceiling raised %v, and a hold is no alarm", al.kinds)
	}

	// At 288 confirmations the three that waited are credited: the ceiling
	// covers what is credited below the horizon.
	node.tip = 991 + types.MaxReorgDepth - 1
	chain.tips[0].Height = node.tip
	for _, txid := range txids {
		node.outs[key(txid, 0)] = txout(t, a, 90, types.MaxReorgDepth, false)
	}
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 3 || len(led.credited) != 5 {
		t.Fatalf("at the horizon the pass credited %d of the three that waited (err %v)", len(got), err)
	}
	if len(al.kinds) != 0 {
		t.Errorf("alerts %v", al.kinds)
	}
}

// A deposit that takes the value below the horizon exactly to the ceiling is
// credited, and one shor more waits.
func TestADepositExactlyAtTheCeilingPassesAndOneShorOverWaits(t *testing.T) {
	for _, c := range []struct {
		name    string
		already int64 // credited below the horizon, by the book
		value   int64
		holds   bool
	}{
		{"the whole ceiling in one deposit", 0, 250, false},
		{"one shor over the ceiling", 0, 251, true},
		{"exactly the room that is left", 100, 150, false},
		{"one shor more than the room", 100, 151, true},
		{"nothing left and a deposit of nothing", 250, 0, false},
		{"nothing left and one shor", 250, 1, true},
		{"the book already over the ceiling", 400, 0, true},
		{"a value below zero", 0, -1, true},
	} {
		book := &fakeBook{}
		if c.already > 0 {
			book.list = []Deposit{dep(99, c.already, 995, 10)}
		}
		tiers := refreshed(t, settled(1000), book)
		if got := tiers.Hold(dep(1, c.value, 991, 10)); got != c.holds {
			t.Errorf("%s: held %v, want %v", c.name, got, c.holds)
		}
		want := c.already
		if !c.holds {
			want += c.value
		}
		if st := tiers.Status(); st.Exposed != want {
			t.Errorf("%s: the value below the horizon reads %d, want %d", c.name, st.Exposed, want)
		}
	}
}

// A deposit that Hold has passed is counted once, however often it is asked
// about again, and the next deposit meets what it took.
func TestADepositPassedOnceIsCountedOnce(t *testing.T) {
	tiers := refreshed(t, settled(1000), &fakeBook{})
	first := dep(1, 200, 991, 10)
	for i := 0; i < 3; i++ {
		if tiers.Hold(first) {
			t.Fatalf("ask %d: a deposit of 200 under a ceiling of 250 was held", i+1)
		}
	}
	if st := tiers.Status(); st.Exposed != 200 {
		t.Fatalf("asked three times, the deposit counts %d against the ceiling, want 200", st.Exposed)
	}
	if !tiers.Hold(dep(2, 51, 991, 10)) {
		t.Error("51 on top of 200 passed a ceiling of 250")
	}
	if tiers.Hold(dep(3, 50, 991, 10)) {
		t.Error("50 on top of 200 was held under a ceiling of 250")
	}
}

// What the book lists as not final counts, whatever height it carries: the
// height is the one a deposit was credited at, and a reorganisation can move
// it to a later block. A deposit credited at the top tier never counts, and
// one listed twice counts once.
func TestTheCeilingCountsWhatTheBookListsAsNotFinal(t *testing.T) {
	const tip = 5000
	for _, c := range []struct {
		name string
		list []Deposit
		want int64
	}{
		{"nothing credited", nil, 0},
		{"credited at 10 confirmations", []Deposit{dep(1, 70, tip-9, 10)}, 70},
		{"credited at 287 confirmations", []Deposit{dep(1, 70, tip-9, 287)}, 70},
		{"credited at the top tier", []Deposit{dep(1, 70, tip-9, 288)}, 0},
		{"400 blocks down by its record and not final", []Deposit{dep(1, 70, tip-399, 10)}, 70},
		{"no height in the book", []Deposit{dep(1, 70, 0, 10)}, 70},
		{"no confirmations in the book", []Deposit{dep(1, 70, tip-9, 0)}, 70},
		{"listed twice", []Deposit{dep(1, 70, tip-9, 10), dep(1, 70, tip-9, 10)}, 70},
		{"two deposits", []Deposit{dep(1, 70, tip-9, 10), dep(2, 30, tip-20, 30)}, 100},
		{"a value below zero is no amount: everything waits", []Deposit{dep(1, -5, tip-9, 10)}, math.MaxInt64},
		{"a sum past int64 stays at the top", []Deposit{dep(1, math.MaxInt64, tip-9, 10), dep(2, 5, tip-9, 10)}, math.MaxInt64},
	} {
		book := &fakeBook{list: c.list}
		tiers := refreshed(t, settled(tip), book)
		if st := tiers.Status(); st.Exposed != c.want {
			t.Errorf("%s: the value below the horizon reads %d, want %d", c.name, st.Exposed, c.want)
		}
		if book.asked != 1 {
			t.Errorf("%s: the book was asked %d times in one reading, want once", c.name, book.asked)
		}
		// A deposit the book lists is not held and not counted a second time.
		if len(c.list) == 1 && c.want == 70 {
			if tiers.Hold(c.list[0]) || tiers.Status().Exposed != 70 {
				t.Errorf("%s: a deposit the book lists was held or counted twice (%d)", c.name, tiers.Status().Exposed)
			}
		}
	}
}

// The attack the book's finality is for. A deposit is credited, a
// reorganisation moves it to a later block, and by the height in the
// exchange's records it is past the horizon while the node has it 180 deep.
// It still counts against the ceiling, so a new deposit that would pass the
// ceiling waits. When the node puts the first one past the horizon the
// Monitor marks it final, and the second is credited on the pass after: the
// pass that gives the mark read the count before it.
func TestACreditedDepositAReorganisationMovedCountsUntilTheNodePutsItPastTheHorizon(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip)
	node.best = "active"
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	first := node.tip - StandardDepth + 1
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 200, Height: first, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 200, StandardDepth, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 || got[0].Height != first {
		t.Fatalf("the deposit of 200 was not credited at 30 confirmations: %v %+v", err, got)
	}

	// 289 blocks above the block in the records, the node has the deposit in
	// a block 109 higher, 180 deep. A deposit of 60 reaches 10 confirmations.
	node.tip = first + types.MaxReorgDepth
	chain.tips[0].Height = node.tip
	moved := first + 109
	cache.utxos[a] = []types.UTXO{
		{TxID: txA, Vout: 0, Value: 200, Height: moved, Address: a},
		{TxID: txB, Vout: 0, Value: 60, Height: node.tip - SmallDepth + 1, Address: a},
	}
	node.outs[key(txA, 0)] = txout(t, a, 200, node.tip-moved+1, false)
	node.outs[key(txB, 0)] = txout(t, a, 60, SmallDepth, false)
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 0 || len(led.credited) != 1 {
		t.Fatalf("60 was credited on top of 200 under a ceiling of 250, the 200 being 180 deep on the node: %v %+v", err, got)
	}
	if st := tiers.Status(); st.Exposed != 200 || len(al.kinds) != 0 {
		t.Fatalf("the value below the horizon reads %d, want 200; alerts %v", st.Exposed, al.kinds)
	}

	// The node puts the first deposit past the horizon: final, and the room
	// under the ceiling is free from the next reading.
	node.tip = moved + types.MaxReorgDepth
	chain.tips[0].Height = node.tip
	node.outs[key(txA, 0)] = txout(t, a, 200, types.MaxReorgDepth+1, false)
	node.outs[key(txB, 0)] = txout(t, a, 60, node.tip-cache.utxos[a][1].Height+1, false)
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 0 || !led.final[key(txA, 0)] {
		t.Fatalf("the pass that marks the first deposit final: %v %+v, final %v", err, got, led.final[key(txA, 0)])
	}
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].TxID != txB {
		t.Fatalf("with the first deposit final the second was not credited: %v %+v", err, got)
	}
	if st := tiers.Status(); st.Exposed != 60 || !led.final[key(txA, 0)] || len(al.kinds) != 0 {
		t.Errorf("after the first deposit is final: below the horizon %d (want 60), final %v, alerts %v", st.Exposed, led.final[key(txA, 0)], al.kinds)
	}
}

// A deposit that waited under the ceiling is credited when it reaches 288
// confirmations, and it takes no room under the ceiling.
func TestADepositAtTheHorizonPassesWhateverTheCeiling(t *testing.T) {
	tiers := refreshed(t, settled(1000), &fakeBook{list: []Deposit{dep(99, 250, 995, 10)}})
	if !tiers.Hold(dep(1, 90, 714, 287)) {
		t.Error("with the ceiling full, a deposit at 287 confirmations was passed")
	}
	if tiers.Hold(dep(1, 90, 713, 288)) {
		t.Error("with the ceiling full, a deposit at 288 confirmations was held")
	}
	if st := tiers.Status(); st.Exposed != 250 {
		t.Errorf("a deposit credited at the horizon moved the value below it to %d", st.Exposed)
	}
}

// ── the unsettled chain ────────────────────────────────────────────────────

// A branch of three or more blocks beside the active chain, ending within the
// last 288 blocks, sends the two lower tiers to 288. The status the node lists
// the branch under is not read: a branch the node left reads as headers-only
// once one header is added on top of it, and as invalid once that block fails.
func TestABranchOfThreeBlocksWithinTheHorizonSendsTheLowerTiersToTheHorizon(t *testing.T) {
	const tip = 1000
	for _, c := range []struct {
		name      string
		height    int64 // of the branch's tip
		length    int64
		status    string
		unsettled bool
	}{
		{"three blocks the node left", tip, 3, "valid-fork", true},
		{"two blocks the node left", tip, 2, "valid-fork", false},
		{"three blocks, 287 below the tip", tip - 287, 3, "valid-fork", true},
		{"three blocks, 288 below the tip", tip - 288, 3, "valid-fork", false},
		{"a header added on a branch that was left", tip + 1, 4, "headers-only", true},
		{"an invalid block added on a branch that was left", tip + 1, 4, "invalid", true},
		{"three blocks the node holds and never followed", tip, 3, "valid-headers", true},
		{"three blocks under a status this client does not know", tip, 3, "unknown", true},
		{"a branch above the tip", tip + 40, 45, "headers-only", true},
	} {
		// The branch carries less work than the chain: only its length speaks.
		chain := settled(tip).with("side", c.height, c.length, c.status, activeWork-1)
		tiers := refreshed(t, chain, &fakeBook{})
		st := tiers.Status()
		if (st.Unsettled != "") != c.unsettled || st.Ahead != "" {
			t.Errorf("%s: unsettled %q, ahead %q", c.name, st.Unsettled, st.Ahead)
		}
		if got := tiers.Hold(dep(1, 50, tip-9, 10)); got != c.unsettled {
			t.Errorf("%s: a small deposit at 10 confirmations held %v, want %v", c.name, got, c.unsettled)
		}
		if got := tiers.Hold(dep(2, 150, tip-286, 287)); got != c.unsettled {
			t.Errorf("%s: a deposit at 287 confirmations held %v, want %v", c.name, got, c.unsettled)
		}
		if tiers.Hold(dep(3, 5000, tip-287, 288)) {
			t.Errorf("%s: a deposit at 288 confirmations was held, and the fallback is 288", c.name)
		}
	}
}

// Headers on top of the active tip are no branch beside the chain, however
// many: a node that is behind is not an unsettled chain.
func TestHeadersOnTopOfTheActiveTipAreNoBranch(t *testing.T) {
	chain := settled(1000).with("next", 1060, 60, "headers-only", activeWork*2)
	tiers := refreshed(t, chain, &fakeBook{})
	if st := tiers.Status(); st.Unsettled != "" || st.Ahead != "" {
		t.Errorf("sixty headers on top of the tip read as unsettled %q, ahead %q", st.Unsettled, st.Ahead)
	}
	if len(chain.workCalls) != 0 {
		t.Errorf("the work of %v was asked for, and nothing is beside the chain", chain.workCalls)
	}
	if tiers.Hold(dep(1, 50, 991, 10)) {
		t.Error("a small deposit was held under headers that extend the chain")
	}
}

// The chain is unsettled while the hashrate of the last 120 blocks is below
// half that of the last 1,440, and while the node gives no usable rate.
func TestAHashrateBelowHalfOrUnreadableSendsTheLowerTiersToTheHorizon(t *testing.T) {
	for _, c := range []struct {
		name        string
		recent, day float64
		unsettled   bool
	}{
		{"the same rate", 100, 100, false},
		{"exactly half", 50, 100, false},
		{"just below half", 49.999, 100, true},
		{"above the day's rate", 300, 100, false},
		{"no recent rate", 0, 100, true},
		{"no rate over the day", 100, 0, true},
		{"neither", 0, 0, true},
		{"an infinite rate", math.Inf(1), 100, true},
		{"a rate that is no number", math.NaN(), 100, true},
		{"a rate below zero", -1, 100, true},
	} {
		chain := settled(1000)
		chain.recent, chain.day = c.recent, c.day
		tiers := refreshed(t, chain, &fakeBook{})
		if st := tiers.Status(); (st.Unsettled != "") != c.unsettled {
			t.Errorf("%s: unsettled %q, want %v", c.name, st.Unsettled, c.unsettled)
		}
		if got := tiers.Hold(dep(1, 50, 991, 10)); got != c.unsettled {
			t.Errorf("%s: a small deposit held %v, want %v", c.name, got, c.unsettled)
		}
		if tiers.Hold(dep(2, 5000, 713, 288)) {
			t.Errorf("%s: a deposit at 288 confirmations was held", c.name)
		}
		if !reflect.DeepEqual(chain.hashCalls, []int{120, 1440}) {
			t.Errorf("%s: the hashrate was asked over %v blocks, want 120 then 1440", c.name, chain.hashCalls)
		}
	}
}

// ── a branch with more work ────────────────────────────────────────────────

// While the node holds a branch that leaves its chain below the tip and
// carries more work, every deposit waits, at any depth: the node follows that
// branch once it has the blocks. The measure is work and never height, since a
// branch of fewer blocks can carry more.
func TestABranchWithMoreWorkHoldsEveryDepositAtAnyDepth(t *testing.T) {
	const tip = 1000
	for _, c := range []struct {
		name   string
		height int64
		length int64
		status string
		work   int64
		ahead  bool
	}{
		{"more work on a branch lower than the tip", tip - 10, 2, "headers-only", activeWork + 1, true},
		{"the same work", tip, 2, "headers-only", activeWork, false},
		{"less work on a branch higher than the tip", tip + 5, 8, "headers-only", activeWork - 1, false},
		{"more work under a tip listed as invalid", tip, 2, "invalid", activeWork + 1, true},
		{"more work, every block held", tip, 2, "valid-headers", activeWork + 1, true},
		{"more work on a branch that forks 400 blocks down", tip - 90, 310, "headers-only", activeWork + 1, true},
	} {
		chain := settled(tip).with("side", c.height, c.length, c.status, c.work)
		tiers := refreshed(t, chain, &fakeBook{})
		if st := tiers.Status(); (st.Ahead != "") != c.ahead {
			t.Errorf("%s: ahead %q, want %v", c.name, st.Ahead, c.ahead)
		}
		for _, d := range []Deposit{dep(1, 5000, tip-287, 288), dep(2, 5000, 1, tip), dep(3, 50, tip-399, 400)} {
			if got := tiers.Hold(d); got != c.ahead {
				t.Errorf("%s: a deposit at %d confirmations held %v, want %v", c.name, d.Confirmations, got, c.ahead)
			}
		}
		if !reflect.DeepEqual(chain.workCalls, []string{"active", "side"}) {
			t.Errorf("%s: work asked of %v, want the active tip and the branch", c.name, chain.workCalls)
		}
	}
}

// The work of a block never changes, so a tip is asked about once. A pass
// that fails over one tip keeps what it read of the others, so a node with
// many tips is read in as many passes as it takes.
func TestTheWorkOfATipIsAskedOnceAndAFailedPassKeepsWhatItRead(t *testing.T) {
	chain := settled(1000).
		with("one", 999, 1, "valid-fork", 5).
		with("two", 998, 1, "valid-headers", 5).
		with("three", 997, 1, "headers-only", 5)
	chain.workErr = map[string]error{"three": errors.New("timeout")}
	tiers, _ := newTestTiers(t, chain, &fakeBook{})
	ctx := context.Background()

	if err := tiers.Refresh(ctx); err == nil {
		t.Fatal("a tip whose work could not be read did not fail the pass")
	}
	if !tiers.Hold(dep(1, 5000, 1, 1000)) {
		t.Error("a deposit was passed on a reading that failed")
	}
	if want := []string{"active", "one", "two", "three"}; !reflect.DeepEqual(chain.workCalls, want) {
		t.Fatalf("work asked of %v, want %v", chain.workCalls, want)
	}

	chain.workErr, chain.workCalls = nil, nil
	if err := tiers.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"three"}; !reflect.DeepEqual(chain.workCalls, want) {
		t.Errorf("after a failed pass, work asked of %v, want only %v", chain.workCalls, want)
	}
	chain.workCalls = nil
	if err := tiers.Refresh(ctx); err != nil || len(chain.workCalls) != 0 {
		t.Errorf("with every tip read, work asked of %v (err %v)", chain.workCalls, err)
	}

	// A new block: the new tip is asked about and the old one is forgotten,
	// as is a branch the node no longer lists.
	chain.tips[0] = rpc.ChainTip{Height: 1001, Hash: "active-2", Status: rpc.ChainTipActive}
	chain.work["active-2"] = activeWork + 10
	chain.tips = chain.tips[:3] // without "three"
	if err := tiers.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"active-2"}; !reflect.DeepEqual(chain.workCalls, want) {
		t.Errorf("after a new block, work asked of %v, want %v", chain.workCalls, want)
	}
	var kept []string
	for _, hash := range []string{"active", "active-2", "one", "two", "three"} {
		if _, ok := tiers.work[hash]; ok {
			kept = append(kept, hash)
		}
	}
	if want := []string{"active-2", "one", "two"}; !reflect.DeepEqual(kept, want) || len(tiers.work) != 3 {
		t.Errorf("the work kept is of %v (%d entries), want %v", kept, len(tiers.work), want)
	}

	// With no branch left nothing is asked and nothing is kept.
	chain.tips, chain.workCalls = chain.tips[:1], nil
	if err := tiers.Refresh(ctx); err != nil || len(chain.workCalls) != 0 || len(tiers.work) != 0 {
		t.Errorf("with no branch: err %v, work asked of %v, %d kept", err, chain.workCalls, len(tiers.work))
	}
}

// A node that gives no work for a tip, or none above zero, has not said which
// chain is the heavier: the pass fails and every deposit waits.
func TestAWorkTheNodeDoesNotGiveFailsThePass(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*fakeChain)
	}{
		{"no work for the branch", func(c *fakeChain) { delete(c.work, "side") }},
		{"no work for the active tip", func(c *fakeChain) { delete(c.work, "active") }},
		{"a work of zero", func(c *fakeChain) { c.work["side"] = 0 }},
		{"a work below zero", func(c *fakeChain) { c.work["side"] = -4 }},
	} {
		chain := settled(1000).with("side", 999, 1, "headers-only", 5)
		c.setup(chain)
		tiers, _ := newTestTiers(t, chain, &fakeBook{})
		if err := tiers.Refresh(context.Background()); err == nil {
			t.Errorf("%s: the pass did not fail", c.name)
		}
		if !tiers.Hold(dep(1, 50, 991, 10)) {
			t.Errorf("%s: a deposit was passed", c.name)
		}
	}
}

// ── the reading ────────────────────────────────────────────────────────────

// Without a reading, or with one older than MaxAge, every deposit waits,
// those at 288 confirmations and deeper among them.
func TestNoReadingOrAnOldOneHoldsEveryDeposit(t *testing.T) {
	tiers, clock := newTestTiers(t, settled(1000), &fakeBook{})
	small, deep := dep(1, 50, 991, 10), dep(2, 5000, 1, 1000)
	if !tiers.Hold(small) || !tiers.Hold(deep) {
		t.Fatal("a deposit was passed before any reading")
	}
	if err := tiers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tiers.Hold(small) || tiers.Hold(deep) {
		t.Fatal("a deposit was held on a fresh reading of a settled chain")
	}
	*clock = clock.Add(DefaultTiersMaxAge)
	if tiers.Hold(deep) {
		t.Error("a reading exactly MaxAge old was refused")
	}
	*clock = clock.Add(time.Nanosecond)
	if !tiers.Hold(deep) || !tiers.Hold(dep(3, 50, 991, 10)) {
		t.Error("a deposit was passed on a reading older than MaxAge")
	}

	tiers.MaxAge = time.Second
	if err := tiers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Second)
	if tiers.Hold(deep) {
		t.Error("with MaxAge one second, a reading one second old was refused")
	}
	*clock = clock.Add(time.Nanosecond)
	if !tiers.Hold(deep) {
		t.Error("with MaxAge one second, a reading older than that was accepted")
	}
}

// Whichever source cannot be read, the reading before it is dropped: Refresh
// returns the error and every deposit waits until a Refresh succeeds.
func TestAnySourceThatCannotBeReadDropsTheReading(t *testing.T) {
	failed := errors.New("the source did not answer")
	for _, c := range []struct {
		name string
		fail func(*fakeChain, *fakeBook)
		mend func(*fakeChain, *fakeBook)
	}{
		{"the recent hashrate", func(c *fakeChain, _ *fakeBook) { c.recentErr = failed }, func(c *fakeChain, _ *fakeBook) { c.recentErr = nil }},
		{"the day's hashrate", func(c *fakeChain, _ *fakeBook) { c.dayErr = failed }, func(c *fakeChain, _ *fakeBook) { c.dayErr = nil }},
		{"the chain tips", func(c *fakeChain, _ *fakeBook) { c.tipsErr = failed }, func(c *fakeChain, _ *fakeBook) { c.tipsErr = nil }},
		{"the work of a tip", func(c *fakeChain, _ *fakeBook) { c.workErr = map[string]error{"side": failed} },
			func(c *fakeChain, _ *fakeBook) { c.workErr = nil }},
		{"the book", func(_ *fakeChain, b *fakeBook) { b.err = failed }, func(_ *fakeChain, b *fakeBook) { b.err = nil }},
	} {
		// The branch is new on the failing pass, so its work is not yet kept.
		chain, book := settled(1000), &fakeBook{}
		tiers, _ := newTestTiers(t, chain, book)
		ctx := context.Background()
		deep := dep(1, 5000, 1, 1000)
		if err := tiers.Refresh(ctx); err != nil || tiers.Hold(deep) {
			t.Fatalf("%s: the first reading failed (%v) or held", c.name, err)
		}
		chain.with("side", 999, 1, "headers-only", 5)
		c.fail(chain, book)
		if err := tiers.Refresh(ctx); !errors.Is(err, failed) {
			t.Errorf("%s: Refresh returned %v, want the source's error", c.name, err)
		}
		if !tiers.Hold(deep) || !tiers.Status().ReadAt.IsZero() {
			t.Errorf("%s: the reading from before the failure still decides", c.name)
		}
		c.mend(chain, book)
		if err := tiers.Refresh(ctx); err != nil || tiers.Hold(deep) {
			t.Errorf("%s: after the source answered again, err %v or still held", c.name, err)
		}
	}
}

// Chain tips that cannot be the node's are refused: none or two active, a
// branch of no length, a fork point below the first block or above the tip.
func TestChainTipsThatCannotBeReadFailThePass(t *testing.T) {
	active := rpc.ChainTip{Height: 1000, Hash: "active", Status: rpc.ChainTipActive}
	for _, c := range []struct {
		name string
		tips []rpc.ChainTip
	}{
		{"no tips", nil},
		{"no active tip", []rpc.ChainTip{{Height: 1000, Hash: "a", BranchLen: 1, Status: "valid-fork"}}},
		{"two active tips", []rpc.ChainTip{active, {Height: 999, Hash: "b", Status: rpc.ChainTipActive}}},
		{"a branch of no length", []rpc.ChainTip{active, {Height: 990, Hash: "b", BranchLen: 0, Status: "valid-fork"}}},
		{"a branch of a length below zero", []rpc.ChainTip{active, {Height: 990, Hash: "b", BranchLen: -2, Status: "valid-fork"}}},
		{"a fork point below the first block", []rpc.ChainTip{active, {Height: 2, Hash: "b", BranchLen: 5, Status: "valid-fork"}}},
		{"a fork point above the active tip", []rpc.ChainTip{active, {Height: 1010, Hash: "b", BranchLen: 5, Status: "headers-only"}}},
		{"an invalid tip of no length", []rpc.ChainTip{active, {Height: 990, Hash: "b", BranchLen: 0, Status: "invalid"}}},
	} {
		chain := settled(1000)
		chain.tips = c.tips
		chain.work["a"], chain.work["b"] = 5, 5
		tiers, _ := newTestTiers(t, chain, &fakeBook{})
		if err := tiers.Refresh(context.Background()); err == nil {
			t.Errorf("%s: the pass did not fail", c.name)
		}
		if !tiers.Hold(dep(1, 5000, 1, 1000)) {
			t.Errorf("%s: a deposit was passed", c.name)
		}
	}
}

// ── the tiers ──────────────────────────────────────────────────────────────

// The tier of a value changes at the two lines, and a value exactly at a line
// belongs to the tier above it. A Tiers that NewTiers did not make gives 288
// for every value and holds every deposit.
func TestTheTierOfAValueChangesAtTheTwoLines(t *testing.T) {
	tiers, _ := newTestTiers(t, settled(1000), &fakeBook{})
	for _, c := range []struct{ value, want int64 }{
		{0, 10}, {1, 10}, {99, 10}, {100, 30}, {999, 30}, {1000, 288}, {types.MaxMoney, 288}, {-1, 288}, {math.MinInt64, 288},
	} {
		if got := tiers.Required(c.value); got != c.want {
			t.Errorf("a deposit of %d shors is credited at %d confirmations, want %d", c.value, got, c.want)
		}
	}
	if SmallDepth != 10 || StandardDepth != 30 || types.MaxReorgDepth != 288 {
		t.Fatalf("the tiers are %d, %d and %d, stated as 10, 30 and 288", SmallDepth, StandardDepth, types.MaxReorgDepth)
	}

	// One line for both: no standard tier.
	if err := tiers.SetLines(TierLines{SmallBelow: 100, LargeFrom: 100, Ceiling: 1}); err != nil {
		t.Fatal(err)
	}
	if tiers.Required(99) != 10 || tiers.Required(100) != 288 {
		t.Errorf("with both lines at 100: %d below, %d at the line", tiers.Required(99), tiers.Required(100))
	}

	var unmade Tiers
	for _, value := range []int64{0, 1, 99, types.MaxMoney, -1} {
		if got := unmade.Required(value); got != types.MaxReorgDepth {
			t.Errorf("a Tiers that NewTiers did not make credits %d shors at %d confirmations", value, got)
		}
	}
	if err := unmade.Refresh(context.Background()); err == nil {
		t.Error("a Tiers that NewTiers did not make read its state")
	}
	if !unmade.Hold(dep(1, 1, 1, 5000)) {
		t.Error("a Tiers that NewTiers did not make passed a deposit")
	}
}

// Lines that are not positive, or out of order, are refused by NewTiers and
// by SetLines, and after a refusal the old lines still decide.
func TestLinesThatAreNotPositiveOrOutOfOrderAreRefused(t *testing.T) {
	chain, book := settled(1000), &fakeBook{}
	if _, err := NewTiers(nil, book, testLines); err == nil {
		t.Error("NewTiers took no node")
	}
	if _, err := NewTiers(chain, nil, testLines); err == nil {
		t.Error("NewTiers took no book")
	}
	tiers, _ := newTestTiers(t, chain, book)
	for _, lines := range []TierLines{
		{},
		{SmallBelow: 0, LargeFrom: 1000, Ceiling: 250},
		{SmallBelow: 100, LargeFrom: 0, Ceiling: 250},
		{SmallBelow: 100, LargeFrom: 1000, Ceiling: 0},
		{SmallBelow: -100, LargeFrom: 1000, Ceiling: 250},
		{SmallBelow: 100, LargeFrom: 1000, Ceiling: -250},
		{SmallBelow: 1001, LargeFrom: 1000, Ceiling: 250},
	} {
		if _, err := NewTiers(chain, book, lines); err == nil {
			t.Errorf("NewTiers took %+v", lines)
		}
		if err := tiers.SetLines(lines); err == nil {
			t.Errorf("SetLines took %+v", lines)
		}
		if tiers.Required(99) != 10 || tiers.Required(100) != 30 || tiers.Required(1000) != 288 {
			t.Fatalf("after refusing %+v the old lines no longer decide", lines)
		}
	}
}

// SetLines moves the two value lines and the ceiling together.
func TestSetLinesMovesTheTiersAndTheCeiling(t *testing.T) {
	tiers := refreshed(t, settled(1000), &fakeBook{})
	if tiers.Required(150) != 30 || !tiers.Hold(dep(1, 260, 991, 30)) {
		t.Fatal("under the first lines, 150 is not at 30 or 260 passed a ceiling of 250")
	}
	if err := tiers.SetLines(TierLines{SmallBelow: 200, LargeFrom: 2000, Ceiling: 300}); err != nil {
		t.Fatal(err)
	}
	if tiers.Required(150) != 10 || tiers.Required(1500) != 30 || tiers.Required(2000) != 288 {
		t.Errorf("under the new lines: 150 at %d, 1500 at %d, 2000 at %d", tiers.Required(150), tiers.Required(1500), tiers.Required(2000))
	}
	if tiers.Hold(dep(1, 260, 991, 30)) || !tiers.Hold(dep(2, 41, 991, 10)) {
		t.Error("under a ceiling of 300, 260 was held or 41 more was passed")
	}
}

// Required, Hold, Status, SetLines and Refresh are called from the scan loop
// and from whatever reads the status; the race detector holds this.
func TestTiersIsSafeUnderConcurrentUse(t *testing.T) {
	tiers := refreshed(t, settled(1000).with("side", 999, 1, "valid-fork", 5), &fakeBook{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tiers.Required(int64(i))
				tiers.Hold(dep(g*1000+i, 1, 991, 10))
				_ = tiers.Status()
				_ = tiers.SetLines(testLines)
			}
		}(g)
	}
	wg.Wait()
	if st := tiers.Status(); st.Exposed != 250 {
		t.Errorf("800 deposits of one shor took the value below the horizon to %d, want the ceiling of 250", st.Exposed)
	}
}

// ── the Monitor and its Safeguard ──────────────────────────────────────────

// recorder is a Safeguard that notes when it is called and what it is asked.
type recorder struct {
	led        *fakeLedger
	refreshErr error
	hold       bool
	holdFinal  bool
	holdMarkOf map[string]bool // by txid: the final marks held when holdFinal is not set
	calls      []string
}

func (r *recorder) Refresh(context.Context) error {
	r.calls = append(r.calls, fmt.Sprintf("refresh with %d credited", len(r.led.credited)))
	return r.refreshErr
}

func (r *recorder) Hold(d Deposit) bool {
	r.calls = append(r.calls, fmt.Sprintf("hold %s at %d", d.TxID[:1], d.Confirmations))
	return r.hold
}

func (r *recorder) HoldFinal(d Deposit) bool {
	r.calls = append(r.calls, fmt.Sprintf("final %s at %d under %q", d.TxID[:1], d.Confirmations, d.BestBlock))
	return r.holdFinal || r.holdMarkOf[d.TxID]
}

// orderedLedger lists what is pending in the order of the transaction ids.
type orderedLedger struct{ *fakeLedger }

func (l orderedLedger) Pending(ctx context.Context) ([]Deposit, error) {
	out, err := l.fakeLedger.Pending(ctx)
	sort.Slice(out, func(i, j int) bool { return out[i].TxID < out[j].TxID })
	return out, err
}

// The Monitor reads the safeguard once in a pass, after the credited deposits
// were rechecked and before a new one is credited, and asks it about each
// deposit the node has confirmed, with the node's depth and the height that
// follows from it. A deposit it holds is not credited and raises nothing, and
// the next pass asks again.
func TestTheMonitorAsksTheSafeguardBeforeEveryCredit(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	guard := &recorder{led: led, hold: true}
	m.Safeguard = guard
	cache.utxos[a] = []types.UTXO{
		{TxID: txA, Vout: 0, Value: 500, Height: 960, Address: a},
		{TxID: txB, Vout: 0, Value: 700, Height: 950, Address: a},
	}
	node.outs[key(txA, 0)] = txout(t, a, 500, 41, false)
	node.outs[key(txB, 0)] = txout(t, a, 700, 51, false)

	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("a held deposit was credited: %v %+v", err, got)
	}
	if want := []string{"refresh with 0 credited", "hold a at 41", "hold b at 51"}; !reflect.DeepEqual(guard.calls, want) {
		t.Fatalf("the safeguard was called %v, want %v", guard.calls, want)
	}
	if len(al.kinds) != 0 {
		t.Errorf("a held deposit raised %v", al.kinds)
	}

	guard.hold, guard.calls = false, nil
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("once released, the pass credited %d of two (err %v)", len(got), err)
	}
	if got[0].Height != 960 || got[0].Confirmations != 41 {
		t.Errorf("the safeguard and the ledger were given height %d at %d confirmations, want 960 at 41", got[0].Height, got[0].Confirmations)
	}
	// A credited deposit is not put to the safeguard again.
	guard.calls = nil
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"refresh with 2 credited"}; !reflect.DeepEqual(guard.calls, want) {
		t.Errorf("with both credited the safeguard was called %v, want %v", guard.calls, want)
	}
}

// A safeguard that cannot read its state ends the pass with nothing new
// credited and its error returned. The recheck of what is already credited
// has run by then, so a credited deposit that vanished is still alarmed.
func TestASafeguardThatCannotReadEndsThePassAfterTheRecheck(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	unread := errors.New("the node did not answer")
	guard := &recorder{led: led, refreshErr: unread}
	m.Safeguard = guard
	led.credited[key(txB, 0)] = Deposit{TxID: txB, Vout: 0, Address: a, Value: 700, Height: 950, Confirmations: 30}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 500, Height: 960, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 500, 41, false)
	// txB is credited and the node no longer has it. A third is credited and
	// the node has it past the horizon.
	led.credited[key("cc", 0)] = Deposit{TxID: "cc", Vout: 0, Address: a, Value: 700, Height: 712, Confirmations: 30}
	node.outs[key("cc", 0)] = txout(t, a, 700, types.MaxReorgDepth+1, false)

	got, err := m.Scan(context.Background())
	if !errors.Is(err, unread) || len(got) != 0 || len(led.credited) != 2 {
		t.Fatalf("the pass returned %v with %d credited, want the safeguard's error and nothing new", err, len(got))
	}
	if len(led.final) != 0 || m.finalCount() != 0 {
		t.Errorf("a deposit was marked final in a pass whose safeguard read nothing: %v", led.final)
	}
	if !strings.Contains(err.Error(), "safeguard") {
		t.Errorf("the error does not say the safeguard failed: %v", err)
	}
	if al.count(AlertDepositVanished) != 1 {
		t.Errorf("the vanished deposit was alarmed %d times before the safeguard ended the pass, want once", al.count(AlertDepositVanished))
	}
	if want := []string{"refresh with 2 credited"}; !reflect.DeepEqual(guard.calls, want) {
		t.Errorf("the safeguard was called %v, want one Refresh and no question", guard.calls)
	}
}

// Why the fallback is a hold and not a higher Required: a deposit credited at
// 10 confirmations stays credited when the chain turns unsettled, and the
// Monitor raises no alarm about it. A count that rose with the chain would
// report it as regressed on every pass.
func TestAnUnsettledChainHoldsNewDepositsAndAlarmsNoCreditedOne(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip)
	node.best = "active"
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 50, Height: 991, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 50, 10, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("on a settled chain the small deposit was not credited at 10: %v %+v", err, got)
	}

	// A branch of three blocks appears, and a second small deposit reaches 10.
	chain.with("side", node.tip, 3, "valid-fork", activeWork-1)
	cache.utxos[a] = append(cache.utxos[a], types.UTXO{TxID: txB, Vout: 0, Value: 60, Height: 991, Address: a})
	node.outs[key(txB, 0)] = txout(t, a, 60, 10, false)
	for pass := 1; pass <= 2; pass++ {
		got, err := m.Scan(context.Background())
		if err != nil || len(got) != 0 || len(led.credited) != 1 {
			t.Fatalf("pass %d on an unsettled chain credited %d (err %v)", pass, len(got), err)
		}
	}
	if len(al.kinds) != 0 {
		t.Errorf("the unsettled chain raised %v: the credited deposit is not regressed and the held one is no alarm", al.kinds)
	}
	if st := tiers.Status(); st.Unsettled == "" {
		t.Error("the status does not say the chain is unsettled")
	}

	// At 288 confirmations the second is credited though the branch is still
	// within the horizon.
	node.tip = 991 + types.MaxReorgDepth - 1
	chain.tips[0].Height = node.tip
	chain.tips[1].Height = node.tip - 100
	node.outs[key(txA, 0)] = txout(t, a, 50, types.MaxReorgDepth, false)
	node.outs[key(txB, 0)] = txout(t, a, 60, types.MaxReorgDepth, false)
	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].TxID != txB {
		t.Fatalf("at 288 confirmations on an unsettled chain: %v %+v", err, got)
	}
}

// A branch with more work stops every credit through the Monitor, the top
// tier included, and releases them when the chain has more work again.
func TestABranchWithMoreWorkStopsEveryCreditThroughTheMonitor(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip).with("side", node.tip-20, 2, "headers-only", activeWork+1)
	node.best = "active"
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 5000, Height: 600, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 5000, 401, false)

	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("a deposit at 401 confirmations was credited beside a branch with more work: %v %+v", err, got)
	}
	if st := tiers.Status(); !strings.Contains(st.Ahead, "more work") || len(al.kinds) != 0 {
		t.Errorf("status %q, alerts %v", st.Ahead, al.kinds)
	}
	// The chain adds work and passes the branch.
	chain.tips[0].Hash, node.best = "active-2", "active-2"
	chain.work["active-2"] = activeWork + 2
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("with the chain ahead again the deposit was not credited: %v %+v", err, got)
	}
}

// ── the tip of the reading ─────────────────────────────────────────────────

// The attack the tip of the reading is for. The reading is taken once, before
// the first credit of a pass. A block arrives after it and with it the headers
// of a branch with more work, so the node reports the deposit at 288
// confirmations under a best block the reading never saw. The pass credits
// nothing, the next one reads the branch, and the deposit is credited when the
// chain has passed it.
func TestADepositReadAtATipTheReadingDidNotSeeWaitsForTheNextPass(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip)
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 5000, Height: node.tip - types.MaxReorgDepth + 1, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 5000, types.MaxReorgDepth, false)
	// The reading is of the tip "active"; gettxout is answered from the block
	// after it.
	node.best = "next"

	got, err := m.Scan(context.Background())
	if err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("a deposit the node reported at a tip the reading did not see was credited: %v %+v", err, got)
	}
	if st := tiers.Status(); st.Ahead != "" || st.Unsettled != "" || len(al.kinds) != 0 {
		t.Fatalf("the reading of the old tip holds back (%q, %q) or the wait raised %v", st.Ahead, st.Unsettled, al.kinds)
	}

	// The next pass reads the new tip, and the branch beside it.
	chain.tips[0].Hash = "next"
	chain.work["next"] = activeWork + 1
	chain.with("side", node.tip-10, 290, "headers-only", activeWork+2)
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 0 || len(led.credited) != 0 {
		t.Fatalf("the deposit was credited beside the branch with more work: %v %+v", err, got)
	}
	if st := tiers.Status(); !strings.Contains(st.Ahead, "more work") {
		t.Fatalf("the second reading does not show the branch: %+v", st)
	}

	// The chain passes the branch: the deposit is credited at the tip both
	// readings are of.
	chain.tips[0].Hash, node.best = "next-2", "next-2"
	chain.work["next-2"] = activeWork + 3
	got, err = m.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].BestBlock != "next-2" {
		t.Fatalf("with the chain ahead and one tip in both readings: %v %+v", err, got)
	}
	if len(al.kinds) != 0 {
		t.Errorf("alerts %v", al.kinds)
	}
}

// Hold passes a deposit only at the tip its reading was taken at, at every
// depth, and a deposit already counted is no exception.
func TestHoldAsksForTheTipOfTheReading(t *testing.T) {
	tiers := refreshed(t, settled(1000), &fakeBook{list: []Deposit{dep(99, 20, 995, 10)}})
	n := 0
	for _, c := range []struct {
		name, best string
		held       bool
	}{
		{"the tip of the reading", "active", false},
		{"another tip", "next", true},
		{"no best block", "", true},
	} {
		for _, confs := range []int64{SmallDepth, types.MaxReorgDepth, 400} {
			n++
			d := dep(n, 50, 1000-confs+1, confs)
			d.BestBlock = c.best
			if got := tiers.Hold(d); got != c.held {
				t.Errorf("%s, %d confirmations: held %v, want %v", c.name, confs, got, c.held)
			}
		}
		counted := dep(99, 20, 995, 10)
		counted.BestBlock = c.best
		if got := tiers.Hold(counted); got != c.held {
			t.Errorf("%s, a deposit the book lists: held %v, want %v", c.name, got, c.held)
		}
	}
	if st := tiers.Status(); st.Exposed != 20+50 {
		t.Errorf("the value below the horizon reads %d, want 70: the book's 20 and the one deposit passed below 288", st.Exposed)
	}

	// A reading whose tip has no hash binds no deposit to it.
	chain := settled(1000)
	chain.tips[0].Hash = ""
	tiers = refreshed(t, chain, &fakeBook{})
	d := dep(1, 50, 991, 10)
	d.BestBlock = ""
	if !tiers.Hold(d) {
		t.Error("a deposit with no best block was passed on a reading whose tip has no hash")
	}
}

// ── what is final ──────────────────────────────────────────────────────────

// The attack the hold on the final mark is for. A deposit is credited at 10
// confirmations. The node then takes the headers of a branch that leaves the
// chain below the deposit and carries more work, and the blocks stay away
// until the node counts the deposit past the horizon. A count alone would
// mark it final there: the ledger would stop listing it, the ceiling would
// stop counting it and no pass would ask the node about it again, so the
// reorganisation that follows would remove it with no alarm. With the hold it
// stays pending and counted, and the pass after the reorganisation alarms it.
func TestACreditedDepositIsNotMarkedFinalWhileABranchWithMoreWorkIsKnown(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip)
	node.best = "active"
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 50, Height: 991, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 50, SmallDepth, false)
	if got, err := m.Scan(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("the small deposit was not credited at 10 confirmations: %v %+v", err, got)
	}

	// The branch forks at 980, below the deposit's block, and its blocks are
	// still away when the node has the deposit 289 deep.
	node.tip = 991 + types.MaxReorgDepth
	chain.tips[0].Height = node.tip
	chain.with("side", 1100, 120, "headers-only", activeWork+1)
	node.outs[key(txA, 0)] = txout(t, a, 50, types.MaxReorgDepth+1, false)
	for pass := 1; pass <= 2; pass++ {
		if _, err := m.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if led.final[key(txA, 0)] || m.finalCount() != 0 {
			t.Fatalf("pass %d marked the deposit final at 289 confirmations beside a branch with more work", pass)
		}
	}
	if st := tiers.Status(); st.Exposed != 50 || !strings.Contains(st.Ahead, "more work") {
		t.Fatalf("the deposit left the count, or the branch is not read: %+v", st)
	}
	if len(al.kinds) != 0 {
		t.Fatalf("the wait raised %v", al.kinds)
	}

	// The blocks arrive. The node follows the branch, and the deposit is not
	// in it.
	chain.tips = []rpc.ChainTip{
		{Height: 1100, Hash: "side", Status: rpc.ChainTipActive},
		{Height: 991 + types.MaxReorgDepth, Hash: "active", BranchLen: 299, Status: "valid-fork"},
	}
	node.tip, node.best = 1100, "side"
	delete(node.outs, key(txA, 0))
	cache.utxos[a] = nil
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if al.count(AlertDepositVanished) != 1 {
		t.Fatalf("the credited deposit the reorganisation removed was alarmed %d times, want once: %v", al.count(AlertDepositVanished), al.kinds)
	}
	if st := tiers.Status(); st.Exposed != 50 {
		t.Errorf("the removed deposit left the count before the exchange settled its record: %+v", st)
	}
}

// The mark is given at the tip of a reading that shows no branch ahead, and
// not from a depth the node counted at another tip. The pass that gives it
// still counts the deposit against the ceiling, since its reading came first;
// the next one does not.
func TestTheFinalMarkWaitsForAReadingAtTheTipTheDepthWasCountedFrom(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	chain := settled(node.tip)
	tiers, _ := newTestTiers(t, chain, led)
	m.Required, m.Safeguard = tiers.Required, tiers
	led.credited[key(txA, 0)] = Deposit{TxID: txA, Vout: 0, Address: a, Value: 50, Height: 712, Confirmations: SmallDepth, BestBlock: "old"}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 50, Height: 712, Address: a}}
	node.outs[key(txA, 0)] = txout(t, a, 50, types.MaxReorgDepth+1, false)

	// The reading is of the tip "active"; gettxout is answered from the block
	// after it.
	node.best = "next"
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if led.final[key(txA, 0)] || m.finalCount() != 0 {
		t.Fatal("a depth counted at a tip the reading did not see marked the deposit final")
	}

	// Both at one tip.
	chain.tips[0].Hash = "next"
	chain.work["next"] = activeWork + 1
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !led.final[key(txA, 0)] || m.finalCount() != 1 {
		t.Fatal("with the depth and the reading at one tip and no branch ahead, the deposit was not marked final")
	}
	if st := tiers.Status(); st.Exposed != 50 {
		t.Errorf("the pass that marked the deposit read the count as %d, want 50: its reading came before the mark", st.Exposed)
	}
	asked := node.calls
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := tiers.Status(); st.Exposed != 0 || node.calls != asked {
		t.Errorf("after the mark the count reads %d and the node was asked %d times, want 0 and 0", st.Exposed, node.calls-asked)
	}
	if len(al.kinds) != 0 {
		t.Errorf("alerts %v", al.kinds)
	}
}

// Before the Monitor marks a credited deposit final it asks the safeguard,
// after the reading of that pass, with the depth and the best block the node
// reported. One the safeguard holds stays pending and is asked about on every
// pass; one it passes is marked in the ledger and asked about no more.
func TestTheMonitorAsksTheSafeguardBeforeItMarksADepositFinal(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	guard := &recorder{led: led, holdFinal: true}
	m.Safeguard = guard
	node.best = "tip-1"
	led.credited[key(txA, 0)] = Deposit{TxID: txA, Vout: 0, Address: a, Value: 700, Height: 712, Confirmations: 30, BestBlock: "old"}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 700, Height: 712, Address: a}}

	// At the horizon itself no mark is due, and the safeguard is not asked.
	node.outs[key(txA, 0)] = txout(t, a, 700, types.MaxReorgDepth, false)
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"refresh with 1 credited"}; !reflect.DeepEqual(guard.calls, want) || len(led.final) != 0 {
		t.Fatalf("at 288 confirmations the safeguard was called %v and the ledger marked %v", guard.calls, led.final)
	}

	node.outs[key(txA, 0)] = txout(t, a, 700, types.MaxReorgDepth+1, false)
	asked := []string{"refresh with 1 credited", `final a at 289 under "tip-1"`}
	for pass := 1; pass <= 2; pass++ {
		guard.calls = nil
		if _, err := m.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(guard.calls, asked) {
			t.Fatalf("pass %d: the safeguard was called %v, want %v", pass, guard.calls, asked)
		}
		if led.final[key(txA, 0)] || m.finalCount() != 0 {
			t.Fatalf("pass %d marked a deposit final that the safeguard holds", pass)
		}
	}

	guard.holdFinal, guard.calls = false, nil
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(guard.calls, asked) || !led.final[key(txA, 0)] || m.finalCount() != 1 {
		t.Fatalf("released, the deposit was not marked final (asked %v, ledger %v, set %d)", guard.calls, led.final, m.finalCount())
	}
	guard.calls = nil
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"refresh with 1 credited"}; !reflect.DeepEqual(guard.calls, want) {
		t.Errorf("a deposit marked final was put to the safeguard again: %v", guard.calls)
	}
	if len(al.kinds) != 0 {
		t.Errorf("alerts %v", al.kinds)
	}
}

// A mark the safeguard holds does not keep the next deposit from its own.
func TestAHeldFinalMarkDoesNotKeepAnotherDepositFromItsMark(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	m.Ledger = orderedLedger{led}
	m.Safeguard = &recorder{led: led, holdMarkOf: map[string]bool{txA: true}}
	for _, txid := range []string{txA, txB} {
		led.credited[key(txid, 0)] = Deposit{TxID: txid, Vout: 0, Address: a, Value: 700, Height: 712, Confirmations: 30}
		cache.utxos[a] = append(cache.utxos[a], types.UTXO{TxID: txid, Vout: 0, Value: 700, Height: 712, Address: a})
		node.outs[key(txid, 0)] = txout(t, a, 700, types.MaxReorgDepth+1, false)
	}
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if led.final[key(txA, 0)] || !led.final[key(txB, 0)] || m.finalCount() != 1 {
		t.Errorf("the ledger marked %v and the set holds %d, want the second deposit alone", led.final, m.finalCount())
	}
	if len(al.kinds) != 0 {
		t.Errorf("alerts %v", al.kinds)
	}
}

// unmarkingLedger cannot record a final mark.
type unmarkingLedger struct {
	orderedLedger
	err error
}

func (l unmarkingLedger) MarkFinal(context.Context, string, uint32) error { return l.err }

// A ledger that cannot record the final mark is alarmed and ends the pass with
// its error, and the deposit stays out of the Monitor's final set. Every
// pending deposit was rechecked before the first mark, so one that vanished,
// listed after the deposit whose mark failed, is alarmed in the same pass.
func TestALedgerThatCannotMarkFinalEndsThePassAfterTheRecheck(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	away := errors.New("the database is away")
	m.Ledger = unmarkingLedger{orderedLedger{led}, away}
	for _, txid := range []string{txA, txB} {
		led.credited[key(txid, 0)] = Deposit{TxID: txid, Vout: 0, Address: a, Value: 700, Height: 712, Confirmations: 30}
		cache.utxos[a] = append(cache.utxos[a], types.UTXO{TxID: txid, Vout: 0, Value: 700, Height: 712, Address: a})
	}
	node.outs[key(txA, 0)] = txout(t, a, 700, types.MaxReorgDepth+1, false)
	// The node no longer has txB.

	got, err := m.Scan(context.Background())
	if !errors.Is(err, away) || len(got) != 0 {
		t.Fatalf("the pass returned %v and %+v, want the ledger's error", err, got)
	}
	if al.count(AlertLedgerError) != 1 || al.count(AlertDepositVanished) != 1 {
		t.Errorf("alerts %v, want the ledger's error and the vanished deposit, once each", al.kinds)
	}
	if m.finalCount() != 0 {
		t.Error("a deposit the ledger could not mark entered the final set")
	}
}

// The same question is asked for a credited deposit the ledger leaves out of
// Pending, before it enters the Monitor's own final set. One the safeguard
// holds costs a question to the node on every pass, as it did within the
// horizon.
func TestACreditedDepositOutsidePendingEntersTheFinalSetOnlyWhenTheSafeguardPassesIt(t *testing.T) {
	m, cache, node, led, al, a := setup(t)
	m.Ledger = &sweepingLedger{fakeLedger: led, swept: map[string]bool{key(txA, 0): true}}
	guard := &recorder{led: led, holdFinal: true}
	m.Safeguard = guard
	node.best = "tip-1"
	led.credited[key(txA, 0)] = Deposit{TxID: txA, Vout: 0, Address: a, Value: 700, Height: 712, Confirmations: 30}
	cache.utxos[a] = []types.UTXO{{TxID: txA, Vout: 0, Value: 700, Height: 712, Address: a}}

	// At the horizon itself the safeguard is not asked.
	node.outs[key(txA, 0)] = txout(t, a, 700, types.MaxReorgDepth, false)
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"refresh with 1 credited"}; !reflect.DeepEqual(guard.calls, want) || m.finalCount() != 0 {
		t.Fatalf("at 288 confirmations the safeguard was called %v and the set holds %d", guard.calls, m.finalCount())
	}

	node.outs[key(txA, 0)] = txout(t, a, 700, types.MaxReorgDepth+1, false)
	asked := []string{"refresh with 1 credited", `final a at 289 under "tip-1"`}
	for pass := 1; pass <= 2; pass++ {
		guard.calls = nil
		before := node.calls
		if _, err := m.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(guard.calls, asked) {
			t.Fatalf("pass %d: the safeguard was called %v, want %v", pass, guard.calls, asked)
		}
		if m.finalCount() != 0 || node.calls-before != 1 {
			t.Fatalf("pass %d: the set holds %d and the node was asked %d times, want 0 and 1", pass, m.finalCount(), node.calls-before)
		}
	}

	guard.holdFinal = false
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := node.calls
	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.finalCount() != 1 || node.calls != before || len(led.final) != 0 {
		t.Errorf("released: the set holds %d, the node was asked %d times more and the ledger marked %v", m.finalCount(), node.calls-before, led.final)
	}
	if len(al.kinds) != 0 {
		t.Errorf("alerts %v", al.kinds)
	}
}

// HoldFinal holds the final mark in the three cases that hold a deposit at any
// depth and in no other: an unsettled chain and a full ceiling leave a deposit
// past the horizon final, and the question counts nothing.
func TestHoldFinalFollowsTheReadingAndNothingElse(t *testing.T) {
	deep := dep(1, 50, 712, types.MaxReorgDepth+1)
	other, none := deep, deep
	other.BestBlock, none.BestBlock = "next", ""

	tiers, clock := newTestTiers(t, settled(1000), &fakeBook{})
	if !tiers.HoldFinal(deep) {
		t.Fatal("a deposit was passed as final before any reading")
	}
	if err := tiers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tiers.HoldFinal(deep) {
		t.Error("a fresh reading at the deposit's tip, with no branch ahead, holds the final mark")
	}
	if !tiers.HoldFinal(other) || !tiers.HoldFinal(none) {
		t.Error("a depth counted at another tip, or at none, was passed as final")
	}
	*clock = clock.Add(DefaultTiersMaxAge)
	if tiers.HoldFinal(deep) {
		t.Error("a reading exactly MaxAge old was refused")
	}
	*clock = clock.Add(time.Nanosecond)
	if !tiers.HoldFinal(deep) {
		t.Error("a deposit was passed as final on a reading older than MaxAge")
	}

	ahead := refreshed(t, settled(1000).with("side", 990, 5, "headers-only", activeWork+1), &fakeBook{})
	if !ahead.HoldFinal(deep) {
		t.Error("a deposit was passed as final beside a branch with more work")
	}

	// Unsettled, and the ceiling taken whole by what the book lists.
	full := refreshed(t, settled(1000).with("side", 999, 3, "valid-fork", activeWork-1),
		&fakeBook{list: []Deposit{dep(9, testLines.Ceiling, 991, 10)}})
	if st := full.Status(); st.Unsettled == "" || st.Ahead != "" || st.Exposed != testLines.Ceiling {
		t.Fatalf("setup: %+v", st)
	}
	if full.HoldFinal(deep) {
		t.Error("an unsettled chain or a full ceiling held the final mark of a deposit past the horizon")
	}
	if st := full.Status(); st.Exposed != testLines.Ceiling {
		t.Errorf("the question moved the count to %d", st.Exposed)
	}
}

// A reading is as old as its first answer. Here the book takes longer than
// MaxAge to list the credited deposits, so the chain tips are that old when
// the reading is kept, and Hold does not decide on it.
func TestAReadingIsAsOldAsItsFirstAnswer(t *testing.T) {
	book := &fakeBook{}
	tiers, clock := newTestTiers(t, settled(1000), book)
	began := *clock
	book.onAsk = func() { *clock = clock.Add(DefaultTiersMaxAge + time.Nanosecond) }
	if err := tiers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tiers.Status().ReadAt; !got.Equal(began) {
		t.Errorf("the reading is stamped %v, want the moment it began, %v", got, began)
	}
	if !tiers.Hold(dep(1, 50, 991, 10)) || !tiers.Hold(dep(2, 5000, 1, 1000)) || !tiers.HoldFinal(dep(3, 50, 712, types.MaxReorgDepth+1)) {
		t.Error("a reading whose chain tips are older than MaxAge decided")
	}

	// One that took exactly MaxAge is still good at the moment it is kept.
	book.onAsk = func() { *clock = clock.Add(DefaultTiersMaxAge) }
	if err := tiers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tiers.Hold(dep(2, 5000, 1, 1000)) {
		t.Error("a reading exactly MaxAge old was refused")
	}
}
