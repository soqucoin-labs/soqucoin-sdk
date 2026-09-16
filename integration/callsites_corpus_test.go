//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

// The corpus for the call-site reading. The reading is the instrument behind
// the fake indexer's coverage test, and it has been wrong twice: a regexp
// missed a call site whose first argument was a call, and the parser that
// replaced it skipped whole function bodies by name, missing three more
// shapes. Both times the replacement was checked against the previous failure
// and not against its own blind spots. These two fixtures are that check, and
// every shape in them was a silent miss in some version.

// Every shape the reading must see. Each fixture method is named after its
// shape; two entries are the forwarder bodies, which must NOT be collected.
func TestTheCallSiteReadingSeesEveryShape(t *testing.T) {
	got, err := readCallSites(filepath.Join("testdata", "callsites-found"), "Client")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	if len(got.problems) != 0 {
		t.Errorf("the corpus of readable shapes reported problems:\n  %s", strings.Join(got.problems, "\n  "))
	}

	want := []string{
		"shape.first.arg.has.a.comma",
		"shape.first.arg.is.a.call",
		"shape.in.a.method.named.Call.on.another.type",
		"shape.in.a.method.named.call.on.another.type",
		"shape.inside.callgen",
		"shape.inside.calllocked",
		"shape.nested.func.literal",
		"shape.package.level.func.literal",
		"shape.parenthesised.callee",
		"shape.plain",
		"shape.plain.function",
	}
	have := map[string]bool{}
	for _, m := range got.methods {
		have[m] = true
	}
	for _, m := range want {
		if !have[m] {
			t.Errorf("the reading missed %s: a call site of that shape would leave the fake behind the client with the coverage test green", m)
		}
	}

	// The two forwarder bodies name methods that must not be collected: a
	// forwarder sends on behalf of its caller, so a literal in its body is
	// not a method the client itself sends.
	for _, m := range []string{"inside.the.call.forwarder", "inside.the.exported.call.forwarder"} {
		if have[m] {
			t.Errorf("the reading collected %s from inside a forwarder body", m)
		}
	}
	if len(got.methods) != len(want) {
		t.Errorf("the reading found %d methods, want exactly %d: %v", len(got.methods), len(want), got.methods)
	}
}

// A call site the reading cannot resolve must be reported. Skipping one in
// silence is the failure mode: the method goes unchecked and nothing says so.
func TestTheCallSiteReadingReportsWhatItCannotRead(t *testing.T) {
	got, err := readCallSites(filepath.Join("testdata", "callsites-unreadable"), "Client")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	if len(got.methods) != 0 {
		t.Errorf("the reading resolved %v from the unreadable corpus; none of those sites names a literal", got.methods)
	}
	// One per unreadable shape: a variable, a constant, a spread argument
	// list, and the caller taken as a value.
	if len(got.problems) != 4 {
		t.Errorf("the reading reported %d problems, want 4, one per shape:\n  %s",
			len(got.problems), strings.Join(got.problems, "\n  "))
	}
	for _, want := range []string{
		"is given a method argument that is not a string literal",
		"is called with 1 arguments",
		"is used as a value",
	} {
		found := false
		for _, p := range got.problems {
			if strings.Contains(p, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no problem reported matching %q; reported:\n  %s", want, strings.Join(got.problems, "\n  "))
		}
	}
}
