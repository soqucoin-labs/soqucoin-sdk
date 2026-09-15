package electrumx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
)

// The push model. The client subscribes to every tracked address
// (blockchain.scripthash.subscribe); the server sends a notification when an
// address's history changes; the refresher makes one listunspent for that
// address. A full pass over every address (the reconcile) runs on a long
// interval as the safety net for a notification the server never sent. A
// server.ping on a short interval keeps the session alive on the server's
// side and, on ours, advances the freshness of every subscribed address that
// has no change pending: with an acknowledged subscription and a server that
// has just answered, an address the server has not reported changed is
// current as of that answer.
//
// A notification writes nothing to the cache. Only a listunspent reply does,
// through the same merge a poll used, and credit still needs the node's
// gettxout in deposit.Monitor. What a lying or faulty indexer can do with a
// notification is therefore what it could do with a poll reply: omit or
// invent an output the node will not confirm. A notification it never sends
// delays a deposit by at most the reconcile interval.

const (
	defaultReconcileInterval = 10 * time.Minute
	defaultPingInterval      = 60 * time.Second
	maxReconnectBackoff      = time.Minute
)

// kick wakes the refresher. Non-blocking: one pending wake is enough, and the
// reader goroutine must never wait on the refresher.
func (c *Client) kick() {
	select {
	case c.kickCh <- struct{}{}:
	default:
	}
}

// noteChange records a scripthash notification received on connection gen.
// The address is found through the client's own map from TrackAddresses; an
// unknown scripthash is ignored. A status equal to the one last seen is a
// duplicate and changes nothing. Otherwise the address is marked changed for
// the refresher and its record marked dirty, so no ping advances it until the
// listunspent has landed.
func (c *Client) noteChange(gen uint64, sh, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr, ok := c.byScriptHash[sh]
	if !ok {
		c.log.Debug("ignoring a notification for an untracked scripthash", "scripthash", sh)
		return
	}
	if gen != c.liveGen.Load() {
		return // from a connection already replaced; the new one re-subscribes
	}
	if prev, seen := c.status[addr]; seen && prev == status {
		return
	}
	c.status[addr] = status
	c.changed[addr] = true
	rec := c.refreshed[addr]
	rec.dirty = true
	rec.seq++
	c.refreshed[addr] = rec
	c.kick()
}

// isSubscribed reports whether addr's subscription was acknowledged on the
// live connection.
func (c *Client) isSubscribed(addr string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	g := c.subscribed[addr]
	return g != 0 && g == c.liveGen.Load()
}

// subscribe asks the server to notify on addr and records the acknowledgement
// against the connection it went over. The reply carries the address's status,
// a hash over its history. When it equals the status last seen and the address's
// set was known current with no change pending, the set is current now and no
// listunspent follows: a reconnect over an unchanged address costs one call.
// Otherwise the address is marked changed and refreshed by the same pass.
func (c *Client) subscribe(ctx context.Context, addr string) error {
	sh, err := address.AddressToScriptHash(c.hrp(), addr)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", addr, err)
	}
	raw, gen, err := c.callGen(ctx, "blockchain.scripthash.subscribe", []interface{}{sh})
	var status string
	if err == nil {
		var ok bool
		if status, ok = parseStatus(raw); !ok {
			err = fmt.Errorf("status %s is neither a string nor null", raw)
		}
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.trackedSet[addr] {
		return nil // dropped by TrackAddresses during the call
	}
	rec := c.refreshed[addr]
	if err != nil {
		if !endedErr(err) {
			rec.err = err
			c.refreshed[addr] = rec
		}
		return fmt.Errorf("subscribe %s: %w", addr, err)
	}
	c.subscribed[addr] = gen
	prev, seen := c.status[addr]
	c.status[addr] = status
	if seen && prev == status && !rec.at.IsZero() && rec.err == nil && !rec.dirty {
		rec.at, rec.gen = now, gen
	} else {
		c.changed[addr] = true
		rec.dirty = true
		rec.seq++
	}
	c.refreshed[addr] = rec
	return nil
}

// ping proves the session alive to both sides and advances the freshness of
// every address subscribed on the connection the reply came over whose record
// is clean: known current, no error, no change pending.
func (c *Client) ping(ctx context.Context) error {
	_, gen, err := c.callGen(ctx, "server.ping", []interface{}{})
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	c.touch(gen, time.Now())
	return nil
}

// touch advances the clean records of the addresses subscribed on gen to now.
func (c *Client) touch(gen uint64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for addr, g := range c.subscribed {
		if g != gen {
			continue
		}
		rec := c.refreshed[addr]
		if rec.dirty || rec.err != nil || rec.at.IsZero() || rec.gen != gen {
			continue
		}
		rec.at = now
		c.refreshed[addr] = rec
	}
}

// pass is one wake of the refresher: subscribe every tracked address not yet
// subscribed on the live connection, then refresh the addresses with a change
// pending, or every address when full (the reconcile, which also records
// LastRefresh). A lost connection or an ended context stops the pass where
// it is; the next pass picks up what is left.
func (c *Client) pass(ctx context.Context, full bool) error {
	if c.liveGen.Load() == 0 {
		return ErrNotConnected // nothing to subscribe on; the refresher reconnects
	}
	c.mu.RLock()
	addrs := append([]string(nil), c.addresses...)
	c.mu.RUnlock()

	var errs []error
	for _, addr := range addrs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if c.isSubscribed(addr) {
			continue
		}
		if err := c.subscribe(ctx, addr); err != nil {
			errs = append(errs, err)
			if errIsConnection(err) || ctx.Err() != nil {
				return errors.Join(errs...)
			}
		}
	}

	var todo []string
	c.mu.Lock()
	if full {
		todo = addrs
	} else {
		for a := range c.changed {
			todo = append(todo, a)
		}
	}
	c.changed = make(map[string]bool)
	c.mu.Unlock()
	if full || len(todo) > 0 {
		failed, err := c.refresh(ctx, todo, full)
		errs = append(errs, err)
		// An address whose listunspent failed, or was never attempted because
		// the pass stopped, keeps its change pending: the retry wake or the
		// next pass refreshes it, rather than the next reconcile.
		if len(failed) > 0 {
			c.mu.Lock()
			for _, a := range failed {
				if c.trackedSet[a] {
					c.changed[a] = true
				}
			}
			c.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

func (c *Client) pingInterval() time.Duration {
	if c.PingInterval > 0 {
		return c.PingInterval
	}
	return defaultPingInterval
}

// Start launches the refresher: it subscribes every tracked address, refreshes
// an address when the server reports it changed, makes a full pass on the
// reconcile interval, and reconnects when the connection is lost (at once) or
// calls keep failing (after two in a row). Every failure is followed by a
// retry after a backoff that doubles from one second to a minute and resets
// on the first clean pass, so a connection that dies after every handshake
// is dialled a few times a minute, not thousands. The ping runs on its own
// goroutine on PingInterval, beside any pass in progress, so a long reconcile
// does not age the addresses it has not reached. Both goroutines end when ctx
// ends or Stop is called; every call they make runs under ctx.
//
// Production lesson: the goroutine includes panic recovery and auto-reconnect.
// Without this, a bufio panic kills the entire process. With recovery, the
// goroutine logs the panic, reconnects, and resumes.
func (c *Client) Start(ctx context.Context) {
	pingErr := make(chan error, 1)
	go c.pingLoop(ctx, pingErr)
	go c.run(ctx, pingErr)
}

// pingLoop pings on PingInterval and hands each failure to the refresher,
// which decides on the reconnect; a success resets nothing there, since only
// a clean pass shows the connection is doing its work.
func (c *Client) pingLoop(ctx context.Context, pingErr chan<- error) {
	ticker := time.NewTicker(c.pingInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			if err := c.ping(ctx); err != nil {
				select {
				case pingErr <- err:
				default: // one pending failure is enough
				}
			}
		}
	}
}

func (c *Client) run(ctx context.Context, pingErr <-chan error) {
	// PF-018 FIX: Recover from panics in the refresher.
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("panic in the refresher, reconnecting", "panic", r)
			if ctx.Err() != nil || c.stopped() {
				return
			}
			if err := c.Reconnect(ctx); err != nil {
				c.log.Error("reconnect after panic failed", "err", err)
			}
			go c.run(ctx, pingErr)
		}
	}()

	reconcile := time.NewTicker(c.reconcileInterval)
	defer reconcile.Stop()
	var retry <-chan time.Time
	backoff := time.Second
	consecutive := 0

	// after takes a pass or ping result. Every failure schedules the next
	// pass after the backoff, whatever else is done about it, so an address
	// whose listunspent failed once is refreshed again well before the
	// reconcile and a connection that keeps dying is redialled at the ladder's
	// pace. A lost connection is reconnected at once, other failures after
	// two in a row; the pass that re-subscribes waits for the retry timer.
	// Only a clean pass resets the ladder.
	after := func(err error) {
		if err == nil {
			consecutive, backoff, retry = 0, time.Second, nil
			return
		}
		if ctx.Err() != nil || c.stopped() {
			return
		}
		consecutive++
		c.log.Warn("refresh failed", "consecutive", consecutive, "in", backoff, "err", err)
		retry = time.After(backoff)
		if backoff *= 2; backoff > maxReconnectBackoff {
			backoff = maxReconnectBackoff
		}
		if !errIsConnection(err) && consecutive < 2 {
			return
		}
		if rerr := c.Reconnect(ctx); rerr != nil {
			c.log.Warn("reconnect failed", "err", rerr)
		}
	}

	after(c.pass(ctx, true))
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-c.kickCh:
			if ctx.Err() != nil {
				return
			}
			if retry != nil {
				// A failure is being backed off; the retry timer runs the
				// next pass. A wake from a lost connection, a notification
				// or TrackAddresses waits for it, so a connection that dies
				// after every handshake is redialled at the ladder's pace.
				continue
			}
			after(c.pass(ctx, false))
		case <-reconcile.C:
			if ctx.Err() != nil {
				return
			}
			after(c.pass(ctx, true))
		case err := <-pingErr:
			if ctx.Err() != nil {
				return
			}
			after(err)
		case <-retry:
			if ctx.Err() != nil {
				return
			}
			retry = nil
			after(c.pass(ctx, false))
		}
	}
}
