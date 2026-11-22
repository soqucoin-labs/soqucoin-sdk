package deposit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The confirmations of the two lower tiers. The third tier is
// types.MaxReorgDepth.
const (
	SmallDepth    = 10
	StandardDepth = 30
)

const (
	// competingBranch is the length of a branch beside the active chain from
	// which the chain is read as unsettled.
	competingBranch = 3
	// recentBlocks and dayBlocks are the two windows whose hashrates are
	// compared: the chain is unsettled while the first is below half of the
	// second.
	recentBlocks = 120
	dayBlocks    = 1440
	// DefaultTiersMaxAge is how old a reading may be when Hold is asked.
	DefaultTiersMaxAge = 5 * time.Minute
)

// TierLines are the three figures an exchange sets, in shors. Set the two
// value lines to the amounts worth about 100 USD and about 1,000 USD at your
// own price and the ceiling to about 10,000 USD, and move them with SetLines
// when the price moves.
type TierLines struct {
	// SmallBelow: a deposit below it is credited at SmallDepth confirmations.
	SmallBelow int64
	// LargeFrom: a deposit at or above it waits for types.MaxReorgDepth. One
	// between the two lines is credited at StandardDepth.
	LargeFrom int64
	// Ceiling is the most value that may be credited below
	// types.MaxReorgDepth confirmations and not yet be past the horizon, at
	// one time.
	Ceiling int64
}

func (l TierLines) check() error {
	switch {
	case l.SmallBelow <= 0 || l.LargeFrom <= 0 || l.Ceiling <= 0:
		return fmt.Errorf("deposit: the tier lines must be positive: %+v", l)
	case l.SmallBelow > l.LargeFrom:
		return fmt.Errorf("deposit: SmallBelow %d is above LargeFrom %d", l.SmallBelow, l.LargeFrom)
	}
	return nil
}

// ChainReader is the part of the exchange's own node Tiers reads.
// *rpc.Client satisfies it.
type ChainReader interface {
	GetNetworkHashPS(ctx context.Context, blocks int) (float64, error)
	GetChainTips(ctx context.Context) ([]rpc.ChainTip, error)
	GetChainWork(ctx context.Context, hash string) (*big.Int, error)
}

// Book is the part of the exchange's own records the ceiling reads.
type Book interface {
	// CreditedSince returns every deposit credited in a block at or above
	// height, as it was credited (the Deposit that Ledger.Credit received),
	// whether or not the exchange has spent the output since. A deposit swept
	// to the hot wallet is still one a reorganisation can remove, so this
	// list keeps what Ledger.Pending leaves out.
	CreditedSince(ctx context.Context, height int64) ([]Deposit, error)
}

// Tiers applies the three confirmation tiers by value, and the safeguards that
// go with them and read state: a ceiling on the value credited below
// types.MaxReorgDepth at one time, types.MaxReorgDepth for every deposit while
// the chain is unsettled, and no credit at all while the node holds a branch
// with more work than the chain it follows.
//
// Required is the Monitor's policy and the Tiers itself its Safeguard:
//
//	tiers, err := deposit.NewTiers(node, book, lines)
//	m := &deposit.Monitor{Required: tiers.Required, Safeguard: tiers, ...}
//
// Required is the tier of a value and does not move with the chain. The
// safeguards hold a deposit that has reached its tier:
//
//   - every deposit, at any depth, while the node holds the headers of a
//     branch that leaves its active chain below the tip and carries more work
//     than that chain. The node follows such a branch once it has the blocks,
//     and a deposit the branch leaves out is then gone. A new header is
//     refused on a branch that forks types.MaxReorgDepth or more blocks down,
//     but a branch whose headers the node took earlier is not judged again, so
//     this is the case a count of confirmations does not cover.
//   - while the chain is unsettled, a deposit below types.MaxReorgDepth
//     confirmations. Unsettled is while the node's chain tips show a branch of
//     three or more blocks beside the active chain that ends within the last
//     types.MaxReorgDepth blocks, and any time the hashrate of the last 120
//     blocks is below half that of the last 1,440 or the node reports none.
//     Such a branch is one the node left in a reorganisation or one it never
//     followed: a header added on top of a branch that was left makes the two
//     read alike, so both count.
//   - a deposit below types.MaxReorgDepth confirmations that would take the
//     value credited below that depth, and not yet past the horizon, over
//     Ceiling. It is credited when it reaches types.MaxReorgDepth.
//   - every deposit while there is no reading, or the reading is older than
//     MaxAge.
//
// The value credited below the horizon is read from the Book on every
// Refresh: each deposit it lists that was credited below types.MaxReorgDepth
// confirmations counts until it has more than that, the depth at which the
// Monitor marks a deposit final. A restart loses nothing, since the count is
// the Book's.
//
// Status says what the last reading holds back. A branch with more work that
// stays, or an unsettled chain, is a condition a human should see: read Status
// after a pass and page on a change. A branch can stay for as long as the
// chain carries less work than it, for example the headers of a chain the node
// took and then found invalid at its first block. That is a chain split, and
// whether to credit through one is the exchange's decision: a Monitor without
// this Safeguard credits again.
type Tiers struct {
	// MaxAge bounds the age of the reading Hold decides on (default
	// DefaultTiersMaxAge). It must exceed the time one pass of the Monitor
	// takes, or the deposits a pass reaches last are held on every pass.
	MaxAge time.Duration

	node ChainReader
	book Book
	now  func() time.Time

	// refreshMu makes Refresh one at a time; work is read and written under
	// it. work holds the proof of work of the chain tips by block hash, which
	// never changes for a hash, so a tip is asked about once.
	refreshMu sync.Mutex
	work      map[string]*big.Int

	mu        sync.Mutex
	lines     TierLines
	readAt    time.Time // zero: no reading
	tip       int64
	ahead     string // why every deposit waits, "" when none does for this
	unsettled string // why the lower tiers wait for the horizon, "" when they do not
	// counted holds the outpoints in exposed: those the Book listed at the
	// last Refresh and those Hold has passed since.
	counted map[string]bool
	exposed int64
}

// NewTiers returns a Tiers that reads node and book. The lines must be
// positive, with SmallBelow no higher than LargeFrom.
func NewTiers(node ChainReader, book Book, lines TierLines) (*Tiers, error) {
	if node == nil || book == nil {
		return nil, errors.New("deposit: Tiers needs a node and a book")
	}
	t := &Tiers{node: node, book: book}
	if err := t.SetLines(lines); err != nil {
		return nil, err
	}
	return t, nil
}

// SetLines replaces the three figures, for when the price has moved. Lines
// that NewTiers would refuse are refused and the old ones stay. A deposit
// credited under the old lines that the new ones ask more confirmations of is
// reported by the Monitor as regressed until it is that deep; the credit
// stands.
func (t *Tiers) SetLines(lines TierLines) error {
	if err := lines.check(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = lines
	return nil
}

// Required returns the confirmations of the tier a deposit of this value is
// in. It is a Policy. A Tiers that NewTiers did not make answers
// types.MaxReorgDepth for every value.
func (t *Tiers) Required(value int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case value < 0 || value >= t.lines.LargeFrom:
		return types.MaxReorgDepth
	case value < t.lines.SmallBelow:
		return SmallDepth
	}
	return StandardDepth
}

// TiersStatus is what the last Refresh read, for a log line, a dashboard or a
// page.
type TiersStatus struct {
	ReadAt    time.Time // zero when there is no reading: every deposit waits
	Tip       int64     // the height of the node's active tip
	Ahead     string    // why every deposit waits, "" when none does for this
	Unsettled string    // why the lower tiers wait for types.MaxReorgDepth, "" when they do not
	Exposed   int64     // shors credited below types.MaxReorgDepth and not yet past the horizon
}

// Status returns the state Hold decides on.
func (t *Tiers) Status() TiersStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TiersStatus{ReadAt: t.readAt, Tip: t.tip, Ahead: t.ahead, Unsettled: t.unsettled, Exposed: t.exposed}
}

// Refresh reads the node's chain tips, their proof of work and two hashrates,
// and the Book's credited deposits. When any of them cannot be read the
// reading is dropped and Hold holds every deposit until a Refresh succeeds.
func (t *Tiers) Refresh(ctx context.Context) error {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	err := t.refresh(ctx)
	if err != nil {
		t.mu.Lock()
		t.readAt = time.Time{}
		t.mu.Unlock()
	}
	return err
}

func (t *Tiers) refresh(ctx context.Context) error {
	if t.node == nil || t.book == nil {
		return errors.New("deposit: this Tiers was not made by NewTiers")
	}
	recent, err := t.node.GetNetworkHashPS(ctx, recentBlocks)
	if err != nil {
		return err
	}
	day, err := t.node.GetNetworkHashPS(ctx, dayBlocks)
	if err != nil {
		return err
	}
	tips, err := t.node.GetChainTips(ctx)
	if err != nil {
		return err
	}
	active, unsettled, branches, err := readTips(tips)
	if err != nil {
		return err
	}
	ahead, err := t.heavier(ctx, active, branches)
	if err != nil {
		return err
	}
	if unsettled == "" {
		unsettled = readHashrate(recent, day)
	}
	// Everything credited in a block that is not yet past the horizon.
	credited, err := t.book.CreditedSince(ctx, active.Height-types.MaxReorgDepth+1)
	if err != nil {
		return fmt.Errorf("deposit: Tiers could not read the credited deposits: %w", err)
	}
	counted := make(map[string]bool, len(credited))
	var exposed int64
	for _, d := range credited {
		key := outpointKey(d.TxID, d.Vout)
		switch {
		case d.Confirmations >= types.MaxReorgDepth: // credited at the top tier
		case d.Height > 0 && active.Height-d.Height+1 > types.MaxReorgDepth: // past the horizon
		case counted[key]: // listed twice, counted once
		default:
			counted[key] = true
			exposed = addCapped(exposed, d.Value)
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.tip, t.ahead, t.unsettled = active.Height, ahead, unsettled
	t.counted, t.exposed = counted, exposed
	t.readAt = t.clock()
	return nil
}

// readTips returns the active tip, the reason the tips make the chain
// unsettled ("" when they do not) and every tip of a branch that leaves the
// active chain below its tip. A tip's status is not read: a header added on
// top of a branch changes the status the branch is listed under, and a branch
// listed as invalid for its last block still holds the valid blocks below it.
func readTips(tips []rpc.ChainTip) (active rpc.ChainTip, unsettled string, branches []rpc.ChainTip, err error) {
	found := 0
	for _, c := range tips {
		if c.Status == rpc.ChainTipActive {
			found++
			active = c
		}
	}
	if found != 1 {
		return active, "", nil, fmt.Errorf("deposit: %d active tips among %d chain tips, want one", found, len(tips))
	}
	for _, c := range tips {
		if c.Status == rpc.ChainTipActive {
			continue
		}
		fork := c.Height - c.BranchLen
		if c.BranchLen <= 0 || fork < 0 || fork > active.Height {
			return active, "", nil, fmt.Errorf("deposit: a chain tip at height %d with branch length %d beside an active tip at %d",
				c.Height, c.BranchLen, active.Height)
		}
		if fork == active.Height {
			continue // headers on top of the active tip: nothing beside the chain
		}
		branches = append(branches, c)
		if c.BranchLen >= competingBranch && active.Height-c.Height < types.MaxReorgDepth {
			unsettled = fmt.Sprintf("a branch of %d blocks beside the active chain ends at height %d, within the last %d blocks",
				c.BranchLen, c.Height, types.MaxReorgDepth)
		}
	}
	return active, unsettled, branches, nil
}

// heavier returns why every deposit waits: one of the branches carries more
// work than the active chain. "" when none does. Each answer of the node is
// kept as it arrives, so a pass that runs out of time over many tips leaves
// fewer to the next one.
func (t *Tiers) heavier(ctx context.Context, active rpc.ChainTip, branches []rpc.ChainTip) (string, error) {
	if len(branches) == 0 {
		t.work = nil
		return "", nil
	}
	if t.work == nil {
		t.work = make(map[string]*big.Int, len(branches)+1)
	}
	workOf := func(hash string) (*big.Int, error) {
		if work, ok := t.work[hash]; ok {
			return work, nil
		}
		work, err := t.node.GetChainWork(ctx, hash)
		if err != nil {
			return nil, err
		}
		if work == nil || work.Sign() <= 0 {
			return nil, fmt.Errorf("deposit: the node reports no work for block %s", hash)
		}
		t.work[hash] = work
		return work, nil
	}
	own, err := workOf(active.Hash)
	if err != nil {
		return "", err
	}
	ahead := ""
	listed := map[string]bool{active.Hash: true}
	for _, b := range branches {
		listed[b.Hash] = true
		work, err := workOf(b.Hash)
		if err != nil {
			return "", err
		}
		if ahead == "" && work.Cmp(own) > 0 {
			ahead = fmt.Sprintf("the node holds a branch with more work than its active chain: %d blocks from height %d to %d (%s, tip %s)",
				b.BranchLen, b.Height-b.BranchLen, b.Height, b.Status, b.Hash)
		}
	}
	for hash := range t.work {
		if !listed[hash] {
			delete(t.work, hash) // no longer a tip
		}
	}
	return ahead, nil
}

// readHashrate returns the reason the two hashrates make the chain unsettled,
// "" when they do not. A rate that is not a positive, finite number is no
// reading of the hashrate.
func readHashrate(recent, day float64) string {
	usable := func(rate float64) bool { return rate > 0 && !math.IsInf(rate, 0) }
	switch {
	case !usable(recent) || !usable(day):
		return fmt.Sprintf("the node reports no usable hashrate (%v over %d blocks, %v over %d)", recent, recentBlocks, day, dayBlocks)
	case recent < day/2:
		return fmt.Sprintf("the hashrate of the last %d blocks is below half that of the last %d", recentBlocks, dayBlocks)
	}
	return ""
}

// addCapped is a + b for two amounts that are not negative, held at the
// largest int64. A negative b is counted as the largest: it is no amount.
func addCapped(a, b int64) int64 {
	if b < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Hold reports whether a deposit that has reached its tier waits on this
// pass. A deposit it passes below types.MaxReorgDepth confirmations is counted
// against the ceiling from that moment, so the next one asked in the same pass
// meets it.
func (t *Tiers) Hold(d Deposit) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readAt.IsZero() || t.clock().Sub(t.readAt) > t.maxAge() {
		return true
	}
	if t.ahead != "" {
		return true
	}
	if d.Confirmations >= types.MaxReorgDepth {
		return false
	}
	if t.unsettled != "" {
		return true
	}
	key := outpointKey(d.TxID, d.Vout)
	if t.counted[key] {
		return false
	}
	if d.Value < 0 || d.Value > t.lines.Ceiling-t.exposed {
		return true
	}
	t.exposed += d.Value
	t.counted[key] = true
	return false
}

func (t *Tiers) maxAge() time.Duration {
	if t.MaxAge > 0 {
		return t.MaxAge
	}
	return DefaultTiersMaxAge
}

func (t *Tiers) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}
