package electrumx

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// A model test over the refresher's scheduling policy.
//
// The policy is pure, so every ordering of events and outcomes can be
// enumerated instead of read. Three properties, each one a sentence the code
// already claimed; every sequence of four events and outcomes (65,536 of
// them) plus four thousand random traces of length 120 are checked against
// all three.
//
// Each mutant below breaks exactly one behaviour the properties are meant to
// hold, and the test requires that a property catch it. A property no mutant
// can violate is not pinning anything.

// state lets the harness read the scheduling state of a policy or a mutant.
func (p *refreshPolicy) state() *refreshPolicy { return p }

type policyUnderTest interface {
	onEvent(refreshEvent) refreshAction
	onResult(refreshOutcome) refreshResult
	state() *refreshPolicy
}

// mutantWakeIgnoresBackoff: a wake runs a pass whatever the backoff says.
// Against a server that hangs up after every handshake this dials thousands
// of times a second.
type mutantWakeIgnoresBackoff struct{ *refreshPolicy }

func (p mutantWakeIgnoresBackoff) onEvent(ev refreshEvent) refreshAction {
	if ev == evKick {
		p.retrying = false
	}
	return p.refreshPolicy.onEvent(ev)
}

// mutantEveryFailureCounts: the reconnect trigger counts every failed pass,
// so one address the indexer refuses rebuilds the connection on every other
// backoff tick.
type mutantEveryFailureCounts struct{ *refreshPolicy }

func (p mutantEveryFailureCounts) onResult(out refreshOutcome) refreshResult {
	if out == outAppErr || out == outNoReply {
		return p.refreshPolicy.onResult(outNoReply)
	}
	return p.refreshPolicy.onResult(out)
}

// mutantDropsReconcile: a reconcile tick arriving during a backoff is
// discarded and never made up, so one permanently refused address starves the
// full pass for the life of the process.
type mutantDropsReconcile struct{ *refreshPolicy }

func (p mutantDropsReconcile) onEvent(ev refreshEvent) refreshAction {
	if ev == evReconcile && p.retrying {
		return refreshAction{}
	}
	return p.refreshPolicy.onEvent(ev)
}

// mutantAppErrKeepsCount: an application error does not break a run of reply
// timeouts, so a timeout, one refused address and another timeout rebuild a
// healthy connection.
type mutantAppErrKeepsCount struct{ *refreshPolicy }

func (p mutantAppErrKeepsCount) onResult(out refreshOutcome) refreshResult {
	saved := p.consecutive
	r := p.refreshPolicy.onResult(out)
	if out == outAppErr {
		p.consecutive = saved
	}
	return r
}

// ---------------------------------------------------------------------------

type modelStep struct {
	ev  refreshEvent
	out refreshOutcome
}

var evName = map[refreshEvent]string{evStart: "Start", evKick: "Kick", evReconcile: "Reconcile", evPingErr: "PingErr", evRetry: "Retry"}
var outName = map[refreshOutcome]string{outOK: "OK", outConnErr: "ConnErr", outNoReply: "NoReply", outAppErr: "AppErr"}

func (s modelStep) String() string { return evName[s.ev] + "/" + outName[s.out] }

func modelSeqString(seq []modelStep) string {
	parts := make([]string, len(seq))
	for i, s := range seq {
		parts[i] = s.String()
	}
	return strings.Join(parts, " -> ")
}

type modelTrace struct {
	fullPasses     int
	reconnects     int
	reported       []refreshOutcome
	fullAfterRec   bool
	passesAfterRec int
	pacingBreak    string
	firstRecAt     int
}

// runModel drives a policy through a sequence. The environment supplies each
// pass's outcome; the policy decides everything else.
func runModel(p policyUnderTest, seq []modelStep) modelTrace {
	tr := modelTrace{firstRecAt: -1}
	for i, s := range seq {
		if s.ev == evReconcile && tr.firstRecAt < 0 {
			tr.firstRecAt = i
		}
		wasRetrying := p.state().retrying
		act := p.onEvent(s.ev)
		if !act.runPass && !act.report {
			continue
		}
		if act.runPass {
			if tr.firstRecAt >= 0 {
				tr.passesAfterRec++
			}
			if act.full {
				tr.fullPasses++
				if tr.firstRecAt >= 0 {
					tr.fullAfterRec = true
				}
			}
		}
		tr.reported = append(tr.reported, s.out)
		if res := p.onResult(s.out); res.reconnect {
			tr.reconnects++
			if wasRetrying && s.ev != evRetry && tr.pacingBreak == "" {
				tr.pacingBreak = fmt.Sprintf("step %d (%s) rebuilt the connection while a backoff was pending", i, s)
			}
		}
	}
	return tr
}

// P1: a wake arriving during a backoff never rebuilds the connection; only
// the retry timer's own pass may. This bounds the dial rate.
func propPacing(tr modelTrace) string { return tr.pacingBreak }

// P2: with no lost connection and no two reply timeouts in a row, nothing
// rebuilds the connection. This is errNoReply's own sentence in conn.go.
func propReconnectTrigger(tr modelTrace) string {
	for _, o := range tr.reported {
		if o == outConnErr {
			return ""
		}
	}
	for i := 1; i < len(tr.reported); i++ {
		if tr.reported[i] == outNoReply && tr.reported[i-1] == outNoReply {
			return ""
		}
	}
	if tr.reconnects > 0 {
		names := make([]string, len(tr.reported))
		for i, o := range tr.reported {
			names[i] = outName[o]
		}
		return fmt.Sprintf("%d reconnect(s) with no lost connection and no two timeouts in a row (outcomes %s)", tr.reconnects, strings.Join(names, ","))
	}
	return ""
}

// P3: a reconcile tick delivered during a backoff is made up by the next pass
// that runs. A wake the policy deliberately discards is not a missed
// opportunity, since discarding it is what P1 requires; only a pass that ran
// and was not full counts against liveness.
func propReconcileLiveness(tr modelTrace) string {
	if tr.firstRecAt < 0 || tr.fullAfterRec || tr.passesAfterRec == 0 {
		return ""
	}
	return fmt.Sprintf("a reconcile tick at step %d never produced a full pass, though %d later passes ran", tr.firstRecAt, tr.passesAfterRec)
}

var modelProperties = []struct {
	name  string
	check func(modelTrace) string
}{
	{"pacing", propPacing},
	{"reconnect trigger", propReconnectTrigger},
	{"reconcile liveness", propReconcileLiveness},
}

var modelEvents = []refreshEvent{evKick, evReconcile, evPingErr, evRetry}
var modelOutcomes = []refreshOutcome{outOK, outConnErr, outNoReply, outAppErr}

// checkAll enumerates every sequence of (event, outcome) to depth and returns
// the first counterexample per property.
func checkAll(depth int, mk func() policyUnderTest) map[string]string {
	found := make(map[string]string)
	seq := make([]modelStep, depth)
	var walk func(int)
	walk = func(i int) {
		if len(found) == len(modelProperties) {
			return
		}
		if i == depth {
			for _, pr := range modelProperties {
				if _, done := found[pr.name]; done {
					continue
				}
				if msg := pr.check(runModel(mk(), seq)); msg != "" {
					found[pr.name] = modelSeqString(seq) + "  ||  " + msg
				}
			}
			return
		}
		for _, ev := range modelEvents {
			for _, out := range modelOutcomes {
				seq[i] = modelStep{ev, out}
				walk(i + 1)
			}
		}
	}
	walk(0)
	return found
}

// TestRefreshPolicyHoldsItsProperties enumerates every sequence of four
// events and outcomes against the live policy.
func TestRefreshPolicyHoldsItsProperties(t *testing.T) {
	if bad := checkAll(4, func() policyUnderTest { return newRefreshPolicy() }); len(bad) != 0 {
		for name, ce := range bad {
			t.Errorf("the policy broke %s:\n  %s", name, ce)
		}
	}
}

// TestRefreshPolicyPropertiesCatchKnownDefects is the sensitivity gate on the
// properties themselves. A property set that cannot fail against a policy
// carrying a known defect proves nothing about the policy that holds them.
func TestRefreshPolicyPropertiesCatchKnownDefects(t *testing.T) {
	base := func() *refreshPolicy { return newRefreshPolicy() }
	for _, m := range []struct {
		head string
		want string
		mk   func() policyUnderTest
	}{
		{"a wake ignores the backoff", "pacing", func() policyUnderTest { return mutantWakeIgnoresBackoff{base()} }},
		{"every failure counts toward the reconnect", "reconnect trigger", func() policyUnderTest { return mutantEveryFailureCounts{base()} }},
		{"a reconcile tick during a backoff is dropped", "reconcile liveness", func() policyUnderTest { return mutantDropsReconcile{base()} }},
		{"an application error does not break a run of timeouts", "reconnect trigger", func() policyUnderTest { return mutantAppErrKeepsCount{base()} }},
	} {
		bad := checkAll(4, m.mk)
		if _, caught := bad[m.want]; !caught {
			t.Errorf("%s: the %q property did not catch it, so it pins nothing (caught instead: %v)", m.head, m.want, keysOf(bad))
		} else {
			t.Logf("%s\n    caught by %s: %s", m.head, m.want, bad[m.want])
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRefreshPolicyDoesNotStarveTheReconcile states the case the way an
// operator would: one address the indexer refuses on every pass, reconcile
// ticking throughout, for long enough that no reader would call it a
// transient. The policy must keep reconciling and must never rebuild the
// connection over an application error.
func TestRefreshPolicyDoesNotStarveTheReconcile(t *testing.T) {
	seq := make([]modelStep, 3000)
	for i := range seq {
		ev := evRetry
		switch {
		case i%7 == 0:
			ev = evReconcile
		case i%5 == 0:
			ev = evKick
		}
		seq[i] = modelStep{ev, outAppErr}
	}
	tr := runModel(newRefreshPolicy(), seq)
	if tr.fullPasses < 100 {
		t.Errorf("the reconcile was starved: %d full passes over %d events", tr.fullPasses, len(seq))
	}
	if tr.reconnects != 0 {
		t.Errorf("an application error rebuilt the connection %d times", tr.reconnects)
	}
	if starved := runModel(mutantDropsReconcile{newRefreshPolicy()}, seq); starved.fullPasses > 1 {
		t.Errorf("the mutant was expected to starve, got %d full passes", starved.fullPasses)
	}
}

// TestRefreshPolicyOverRandomTraces reaches interleavings the depth-4
// enumeration does not.
func TestRefreshPolicyOverRandomTraces(t *testing.T) {
	const runs, length = 4000, 120
	rng := rand.New(rand.NewSource(20260915))
	for r := 0; r < runs; r++ {
		seq := make([]modelStep, length)
		for i := range seq {
			seq[i] = modelStep{modelEvents[rng.Intn(len(modelEvents))], modelOutcomes[rng.Intn(len(modelOutcomes))]}
		}
		tr := runModel(newRefreshPolicy(), seq)
		for _, pr := range modelProperties {
			if msg := pr.check(tr); msg != "" {
				t.Fatalf("the policy broke %s: %s\nsequence: %s", pr.name, msg, modelSeqString(seq))
			}
		}
	}
}

// A pass that made no call is not an outcome. The first ping timeout sets a
// retry, and the retry's pass has nothing to refresh whenever no address has
// a change pending, which is the ordinary state of a quiet wallet. Reported
// as a clean pass it reset the reply-timeout count, so the second ping
// timeout counted as the first and a server that had stopped answering while
// holding the connection open was never replaced.
func TestAPassThatMadeNoCallIsNotAnOutcome(t *testing.T) {
	timeout := fmt.Errorf("ping: %w", errNoReply)
	p := newRefreshPolicy()

	if act := p.onEvent(evPingErr); !act.report {
		t.Fatal("the first ping failure was not reported to the policy")
	}
	out, report := classifyPass(true, timeout)
	if !report {
		t.Fatal("a ping failure is an outcome")
	}
	if res := p.onResult(out); res.reconnect {
		t.Fatal("one reply timeout rebuilt the connection")
	}

	act := p.onEvent(evRetry)
	if !act.runPass {
		t.Fatal("the retry timer did not run a pass")
	}
	if _, report := classifyPass(false, nil); report {
		t.Fatal("a pass that made no call was reported as an outcome")
	}

	if act := p.onEvent(evPingErr); !act.report {
		t.Fatal("the second ping failure was not reported to the policy")
	}
	out, _ = classifyPass(true, timeout)
	if res := p.onResult(out); !res.reconnect {
		t.Error("two reply timeouts in a row did not rebuild the connection")
	}
}
