//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The push scenarios believe the fake indexer. That is only safe while the
// fake answers every method electrumx.Client sends, and the two drift apart
// silently: the branch this harness came from was cut before the client grew
// blockchain.scripthash.get_history and blockchain.transaction.broadcast, and
// nothing failed — a scenario that reached either would have met "unknown
// method", taken the error path, and reported success without exercising
// what it names.
//
// So the client's outbound call sites are read out of its own source and
// compared with the handlers in indexerMethods. This test needs no node and
// runs in milliseconds.

// callSite matches the three forms the client sends a request by:
// c.call(ctx, "m", ...), c.callGen(ctx, "m", ...) and c.callLocked(ctx, "m", ...).
// The inbound notification switches in conn.go name methods too, in `case`
// labels, and are deliberately not matched: they are what the server sends.
var callSite = regexp.MustCompile(`\bc\.call(?:Gen|Locked)?\(\s*\w+\s*,\s*"([a-z_][a-z_.]*)"`)

// clientMethodFloor is how many distinct methods the scan finds today. It is
// written down so that a rename of call/callGen/callLocked, which would make
// the regexp match nothing, fails here instead of turning this test into one
// that asserts nothing. Lowering it is a deliberate act.
const clientMethodFloor = 8

func TestTheFakeIndexerAnswersEveryMethodTheClientSends(t *testing.T) {
	sent := methodsTheClientSends(t)
	if len(sent) < clientMethodFloor {
		t.Fatalf("found %d methods in the electrumx client (%v), want at least %d: the call sites are no longer matched, so this test is checking nothing",
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
// double and a reader will take it for a path under test.
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

// methodsTheClientSends reads the non-test source of the electrumx package and
// returns the distinct JSON-RPC methods it calls, sorted.
func methodsTheClientSends(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "electrumx")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := map[string]bool{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		files++
		for _, m := range callSite.FindAllSubmatch(b, -1) {
			found[string(m[1])] = true
		}
	}
	if files == 0 {
		t.Fatalf("no non-test Go files under %s", dir)
	}
	out := make([]string, 0, len(found))
	for m := range found {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
