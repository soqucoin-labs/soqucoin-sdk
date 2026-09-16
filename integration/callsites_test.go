//go:build integration

package integration

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Reading the methods a client sends out of its own source.
//
// This is the instrument behind fake_electrumx_coverage_test.go. It is
// separate, and parameterised by directory, so that it can be run against a
// fixture corpus (callsites_corpus_test.go) as well as against the electrumx
// package. A guard with no corpus is a reminder: the first version of this
// reading was a regexp that missed a call site whose first argument was itself
// a call, and the replacement missed three more shapes, because each was
// checked against the previous failure rather than against its own blind
// spots.
//
// It reports problems rather than failing, so a test can assert that an
// unreadable call site is reported and not skipped.

// callers are the request methods. Every JSON-RPC method a client sends goes
// through one of them, with the method name as the second argument.
var callers = map[string]bool{"call": true, "callGen": true, "callLocked": true}

// forwarders are the two request methods that pass a method parameter down to
// the next one instead of naming a method: Call calls call, and call calls
// callGen. A call to a caller from inside one of these names no method, so the
// body is skipped.
//
// callGen and callLocked are deliberately NOT here. Neither calls a caller —
// both go to sendLocked and await — so skipping them would hide any real call
// site added to their bodies, which is the defect this whole file exists to
// prevent.
//
// The skip also requires the receiver to be the client type. Keyed on the name
// alone, any method called call or Call on any type in the package would hide
// every call site in its body.
var forwarders = map[string]bool{"Call": true, "call": true}

// callSites is what one reading found: the distinct methods sent, sorted, and
// the call sites it could not read, each as a message naming its position.
type callSites struct {
	methods  []string
	problems []string
}

// readCallSites parses the non-test Go files directly under dir and collects
// the methods sent through callers from bodies that are not forwarders.
//
// recvType is the receiver type whose call and Call forward, without a leading
// star: "Client" for the electrumx package.
func readCallSites(dir, recvType string) (callSites, error) {
	paths, err := nonTestGoFiles(dir)
	if err != nil {
		return callSites{}, err
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	var problems []string
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return callSites{}, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range f.Decls {
			// The forwarder skip applies to a whole function body, so it is
			// decided per declaration. Every other declaration is walked,
			// which is what a package-level function literal lives in: a
			// GenDecl, invisible to a walk that looks only at FuncDecls.
			if fn, ok := decl.(*ast.FuncDecl); ok && isForwarder(fn, recvType) {
				continue
			}
			problems = append(problems, collectFrom(fset, decl, found)...)
		}
	}
	out := make([]string, 0, len(found))
	for m := range found {
		out = append(out, m)
	}
	sort.Strings(out)
	sort.Strings(problems)
	return callSites{methods: out, problems: problems}, nil
}

// isForwarder reports a method named call or Call on the client type.
func isForwarder(fn *ast.FuncDecl, recvType string) bool {
	if !forwarders[fn.Name.Name] {
		return false
	}
	return receiverTypeName(fn) == recvType
}

// receiverTypeName is the receiver's type without a leading star, empty for a
// plain function.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	// A generic receiver is Name[T]; the name is what matters.
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// collectFrom walks one declaration, records every method named at a caller
// call site, and returns a message for each site it could not read.
func collectFrom(fset *token.FileSet, node ast.Node, found map[string]bool) []string {
	var problems []string

	// The selectors that are the callee of a call. Anything else that names a
	// caller is the method taken as a value, which this reading cannot follow.
	callee := map[*ast.SelectorExpr]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				callee[sel] = true
			}
			// A parenthesised callee, (c.call)(...), is still a callee.
			if par, ok := call.Fun.(*ast.ParenExpr); ok {
				if sel, ok := par.X.(*ast.SelectorExpr); ok {
					callee[sel] = true
				}
			}
		}
		return true
	})

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel := calleeSelector(x.Fun)
			if sel == nil || !callers[sel.Sel.Name] {
				return true
			}
			if len(x.Args) < 2 {
				problems = append(problems, fmt.Sprintf("%s: %s is called with %d arguments, so this reading cannot tell which method it sends",
					fset.Position(x.Pos()), sel.Sel.Name, len(x.Args)))
				return true
			}
			lit, ok := x.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				problems = append(problems, fmt.Sprintf("%s: %s is given a method argument that is not a string literal, so this reading cannot tell which method it sends",
					fset.Position(x.Pos()), sel.Sel.Name))
				return true
			}
			m, err := strconv.Unquote(lit.Value)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: cannot read the method literal %s: %v", fset.Position(lit.Pos()), lit.Value, err))
				return true
			}
			found[m] = true
		case *ast.SelectorExpr:
			if callers[x.Sel.Name] && !callee[x] {
				problems = append(problems, fmt.Sprintf("%s: %s is used as a value, so this reading cannot follow what it sends",
					fset.Position(x.Pos()), x.Sel.Name))
			}
		}
		return true
	})
	return problems
}

// calleeSelector is the selector a call expression calls, through one layer of
// parentheses, or nil when the callee is not a selector.
func calleeSelector(fun ast.Expr) *ast.SelectorExpr {
	if par, ok := fun.(*ast.ParenExpr); ok {
		fun = par.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	return sel
}

// nonTestGoFiles is a package's own source: the .go files directly under dir
// that are not tests.
func nonTestGoFiles(dir string) ([]string, error) {
	all, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	var out []string
	for _, p := range all {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no non-test Go files under %s", dir)
	}
	return out, nil
}
