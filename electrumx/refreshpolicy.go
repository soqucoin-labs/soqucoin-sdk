package electrumx

import "time"

// The refresher's scheduling policy, separated from its I/O.
//
// Everything about when a pass runs, how full it is, how long the next one
// waits and whether the connection is rebuilt lives here, as a state machine
// over events and outcomes with no timers, goroutines or sockets in it. run()
// in subscribe.go is the only caller and does nothing but supply events and
// carry out the decisions.
//
// The separation is not tidiness. Three consecutive readings of this logic
// while it was inline in the select loop each found a defect introduced by
// the previous reading's fix: the dial storm, the reconnect trigger counting
// application errors, and a reconcile tick discarded during a backoff and
// never made up. A reader cannot enumerate the orderings; a test can.
// refreshpolicy_model_test.go enumerates every sequence of four events and
// outcomes against the three properties stated below, and each of those three
// heads fails at least one of them.

// refreshEvent is what wakes the refresher.
type refreshEvent int

const (
	evStart     refreshEvent = iota // the first pass, before the loop
	evKick                          // a notification, a lost connection, or TrackAddresses
	evReconcile                     // the long-interval safety-net tick
	evPingErr                       // the ping goroutine reported a failure
	evRetry                         // the backoff timer fired
)

// refreshOutcome is how a pass, or a ping, reported.
type refreshOutcome int

const (
	outOK      refreshOutcome = iota
	outConnErr                // the connection was lost
	outNoReply                // a call timed out waiting for its reply
	outAppErr                 // the server answered with an error: one address it refuses
)

// classifyOutcome maps a pass's error onto what the policy reasons about. A
// pass joins the errors of every address it touched, so a lost connection
// anywhere in the pass is a lost connection, and a timeout anywhere in it is
// a timeout; only a pass whose every failure was the server answering with an
// error is an application error, which says nothing about the connection.
func classifyOutcome(err error) refreshOutcome {
	switch {
	case err == nil:
		return outOK
	case errIsConnection(err):
		return outConnErr
	case errIsNoReply(err):
		return outNoReply
	default:
		return outAppErr
	}
}

// refreshAction is what the policy decides for one event.
type refreshAction struct {
	runPass bool
	full    bool
	report  bool // no pass: report an error the caller already has (the ping's)
}

// refreshResult is what the policy decides once a pass or ping has reported.
type refreshResult struct {
	reconnect bool
	setRetry  bool
	backoff   time.Duration
}

// refreshPolicy holds the whole of the refresher's scheduling state.
//
// The three properties it is built to hold, each of them a sentence the code
// around it already claimed:
//
//   - Pacing: a wake arriving during a backoff never rebuilds the connection.
//     Only the retry timer's own pass may. This bounds the dial rate against a
//     server that hangs up after every handshake.
//   - Reconnect trigger: with no lost connection and no two reply timeouts in
//     a row, nothing rebuilds the connection (conn.go, errNoReply).
//   - Reconcile liveness: a reconcile tick delivered during a backoff is made
//     up by the next pass that runs. The reconcile is the only safety net
//     against a notification the server never sent, so it may not be starved
//     by an address the indexer permanently refuses.
type refreshPolicy struct {
	backoff     time.Duration
	consecutive int  // reply timeouts in a row
	retrying    bool // a backoff timer is pending
	pendingFull bool // a reconcile tick arrived during the backoff
}

func newRefreshPolicy() *refreshPolicy {
	return &refreshPolicy{backoff: time.Second}
}

// takeFull consumes a reconcile tick deferred by a backoff.
func (p *refreshPolicy) takeFull() bool {
	full := p.pendingFull
	p.pendingFull = false
	return full
}

func (p *refreshPolicy) onEvent(ev refreshEvent) refreshAction {
	switch ev {
	case evStart:
		return refreshAction{runPass: true, full: true}
	case evKick:
		if p.retrying {
			return refreshAction{}
		}
		return refreshAction{runPass: true, full: p.takeFull()}
	case evReconcile:
		if p.retrying {
			// Deferred, not dropped: the pass after the backoff runs full.
			// Dropping it starved the reconcile for the life of the process
			// whenever one address failed on every pass.
			p.pendingFull = true
			return refreshAction{}
		}
		return refreshAction{runPass: true, full: true}
	case evPingErr:
		if p.retrying {
			return refreshAction{}
		}
		return refreshAction{report: true}
	case evRetry:
		p.retrying = false
		return refreshAction{runPass: true, full: p.takeFull()}
	}
	return refreshAction{}
}

func (p *refreshPolicy) onResult(out refreshOutcome) refreshResult {
	if out == outOK {
		p.consecutive, p.backoff, p.retrying = 0, time.Second, false
		return refreshResult{}
	}
	r := refreshResult{setRetry: true, backoff: p.backoff}
	p.retrying = true
	if p.backoff *= 2; p.backoff > maxReconnectBackoff {
		p.backoff = maxReconnectBackoff
	}
	switch out {
	case outConnErr:
	case outNoReply:
		p.consecutive++
		if p.consecutive < 2 {
			return r
		}
	default:
		// A reply that arrived is not a timeout, so it breaks the run. Without
		// this, a timeout, one refused address and another timeout rebuilt a
		// healthy connection, which errNoReply's own comment forbids.
		p.consecutive = 0
		return r
	}
	// The count is spent on the decision, not on the dial succeeding: a
	// reconnect that fails leaves the connection down, so the next pass
	// returns a connection error and rebuilds at once whatever the count.
	p.consecutive = 0
	r.reconnect = true
	return r
}
