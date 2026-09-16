//go:build integration

package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
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
// So the client's outbound call sites are read out of its own source and
// compared with the handlers in indexerMethods. The reading is done with
// go/parser rather than a pattern over the text: a regexp has to guess where
// the method argument begins, and a call whose first argument is itself a call
// with a comma in it defeats any such guess, which leaves the new call site
// invisible and this test passing. That is the failure this file exists to
// prevent, so the instrument may not have it.
//
// This test needs no node and runs in milliseconds.

// callers are the client's own request methods. Every JSON-RPC method the
// client sends goes through one of them, with the method name as the second
// argument. The inbound notification switches in conn.go name methods too, in
// case labels, and are correctly invisible here: those are what the server
// sends.
var callers = map[string]bool{"call": true, "callGen": true, "callLocked": true}

// forwarders are the request plumbing: Call, call and callGen pass a method
// parameter down to the next one rather than naming a method themselves, so a
// call to a caller from inside one of these is not a call site. Call is
// exported, so an SDK consumer can send any method it likes through it; what
// the fake has to cover is what the client itself sends, which is what the
// reading below collects.
var forwarders = map[string]bool{"Call": true, "call": true, "callGen": true, "callLocked": true}

// clientMethodFloor is how many distinct methods the reading finds today. It is
// written down so that a rename of the request methods, which would make the
// reading match nothing, fails here instead of turning this test into one that
// asserts nothing. Lowering it is a deliberate act.
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

// methodsTheClientSends parses the non-test source of the electrumx package
// and returns the distinct JSON-RPC methods it calls, sorted.
//
// A call site whose method argument is not a string literal cannot be resolved
// here and is reported as a failure. Such a site is precisely the one this
// test would otherwise skip in silence, so it has to be made a literal, or the
// set stated some other way; it may not simply go unread.
func methodsTheClientSends(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "electrumx")
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, path := range nonTestGoFiles(t, dir) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || forwarders[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !callers[sel.Sel.Name] || len(call.Args) < 2 {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: %s in %s is given a method argument that is not a string literal, so this test cannot read which method it sends; make it a literal, or the fake's coverage of that method goes unchecked",
						fset.Position(call.Pos()), sel.Sel.Name, fn.Name.Name)
					return true
				}
				m, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("%s: cannot read the method literal %s: %v", fset.Position(lit.Pos()), lit.Value, err)
					return true
				}
				found[m] = true
				return true
			})
		}
	}
	out := make([]string, 0, len(found))
	for m := range found {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// nonTestGoFiles is the package's own source: the .go files that are not tests.
func nonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []string
	for _, p := range all {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		t.Fatalf("no non-test Go files under %s", dir)
	}
	return out
}
