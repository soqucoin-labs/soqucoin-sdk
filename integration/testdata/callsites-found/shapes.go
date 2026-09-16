//go:build ignore

// Package found is a fixture for readCallSites: every shape of call site the
// reading has to see. It is under testdata and carries an ignore constraint,
// so it is never built; the reading parses it directly.
//
// Each shape names a method after the shape, and callsites_corpus_test.go
// asserts that all of them are found. Every one of these was a silent miss in
// some version of the reading.
package found

import (
	"context"
	"encoding/json"
	"time"
)

// Client stands in for electrumx.Client: the type whose call and Call forward.
type Client struct{ probe bool }

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	// A forwarder: names no method of its own. A literal here must NOT be
	// collected, and "inside.the.call.forwarder" is asserted absent.
	if c.probe {
		_, _ = c.callGen(ctx, "inside.the.call.forwarder", params)
	}
	return c.callGen(ctx, method, params)
}

// Call is the other forwarder, for the same reason.
func (c *Client) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.probe {
		_, _ = c.call(ctx, "inside.the.exported.call.forwarder", params)
	}
	return c.call(ctx, method, params)
}

// callGen is a caller and not a forwarder: it sends nothing on behalf of
// another caller, so a literal in its body is a real call site.
func (c *Client) callGen(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.probe {
		_, _ = c.callLocked(ctx, "shape.inside.callgen", params)
	}
	return nil, nil
}

// callLocked is a caller and not a forwarder, for the same reason.
func (c *Client) callLocked(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.probe {
		_, _ = c.callGen(ctx, "shape.inside.calllocked", params)
	}
	return nil, nil
}

// The ordinary shape: a literal in a method that is not a forwarder.
func (c *Client) plain(ctx context.Context) {
	_, _ = c.call(ctx, "shape.plain", nil)
}

// The first argument is itself a call, which no pattern over the text can
// step over reliably.
func (c *Client) firstArgumentIsACall() {
	_, _ = c.call(context.Background(), "shape.first.arg.is.a.call", nil)
}

// The first argument is a call containing a comma.
func (c *Client) firstArgumentHasAComma() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = c.call(ctx, "shape.first.arg.has.a.comma", nil)
}

// A method named call on another type must not be treated as a forwarder:
// its body holds a real call site.
type helper struct{ c *Client }

func (h *helper) call(ctx context.Context) {
	_, _ = h.c.call(ctx, "shape.in.a.method.named.call.on.another.type", nil)
}

// Same for an exported Call on another type.
type wrapper struct{ c *Client }

func (w *wrapper) Call(ctx context.Context) {
	_, _ = w.c.call(ctx, "shape.in.a.method.named.Call.on.another.type", nil)
}

// A package-level function literal lives in a GenDecl, not a FuncDecl.
var packageLevelLiteral = func(c *Client, ctx context.Context) {
	_, _ = c.call(ctx, "shape.package.level.func.literal", nil)
}

// A function literal nested inside a function body.
func (c *Client) nestedLiteral(ctx context.Context) func() {
	return func() {
		_, _ = c.call(ctx, "shape.nested.func.literal", nil)
	}
}

// A plain function with no receiver.
func plainFunction(c *Client, ctx context.Context) {
	_, _ = c.callGen(ctx, "shape.plain.function", nil)
}

// A parenthesised callee is still a call site.
func (c *Client) parenthesisedCallee(ctx context.Context) {
	_, _ = (c.call)(ctx, "shape.parenthesised.callee", nil)
}
