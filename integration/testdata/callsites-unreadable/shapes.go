//go:build ignore

// Package unreadable is a fixture for readCallSites: the call sites it cannot
// resolve. Each must be REPORTED, never skipped in silence, because a site the
// reading passes over is exactly the one that lets the fake fall behind the
// client while the coverage test stays green.
package unreadable

import (
	"context"
	"encoding/json"
)

type Client struct{}

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	return nil, nil
}

// The method argument is a variable.
func (c *Client) methodFromAVariable(ctx context.Context, m string) {
	_, _ = c.call(ctx, m, nil)
}

// The method argument is a constant, which reads like a literal and is not one.
const someMethod = "unreadable.constant"

func (c *Client) methodFromAConstant(ctx context.Context) {
	_, _ = c.call(ctx, someMethod, nil)
}

// The whole argument list comes from another call, so there is no second
// argument to read.
func args(ctx context.Context) (context.Context, string, interface{}) {
	return ctx, "unreadable.spread", nil
}

func (c *Client) argumentsFromACall(ctx context.Context) {
	_, _ = c.call(args(ctx))
}

// The caller is taken as a value, so the reading cannot follow what is sent
// through it.
func (c *Client) callerAsAValue(ctx context.Context) {
	g := c.call
	_, _ = g(ctx, "unreadable.method.value", nil)
}
