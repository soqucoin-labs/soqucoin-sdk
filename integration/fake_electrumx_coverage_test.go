//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"
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
const clientMethodFloor = 8

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
