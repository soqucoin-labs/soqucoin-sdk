// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package rpc

import (
	"context"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// The member activation_heights is read by the swap functions alone, and
// GetBlockchainInfo is read by every function that asks whether the node is
// synchronised and on its chain. So no form of the member is an error: a
// height the client can read is kept, and anything else reads as a height the
// node does not report. GetBlockchainInfo, RequireSynced and RequireChain
// answer for each form as they do for a reply without the member.
func TestNoFormOfTheActivationHeightsFailsTheReply(t *testing.T) {
	for _, c := range []struct {
		name, member string
		want         map[string]int64
	}{
		{"the member absent", ``, nil},
		{"whole numbers", `,"activation_heights":{"p2wsh_dilithium":0,"dilithium_keyhash":100,"v6_controlflow":2147483647}`,
			map[string]int64{"p2wsh_dilithium": 0, "dilithium_keyhash": 100, "v6_controlflow": 2147483647}},
		{"an empty object", `,"activation_heights":{}`, nil},
		{"the member null", `,"activation_heights":null`, nil},
		{"one height null", `,"activation_heights":{"p2wsh_dilithium":null}`, nil},
		{"a height with a fraction", `,"activation_heights":{"p2wsh_dilithium":100.5}`, nil},
		{"a height in exponent form", `,"activation_heights":{"p2wsh_dilithium":1e3}`, nil},
		{"a height as a string", `,"activation_heights":{"p2wsh_dilithium":"100"}`, nil},
		{"a height above int64", `,"activation_heights":{"p2wsh_dilithium":9223372036854775808}`, nil},
		{"a height as an object", `,"activation_heights":{"p2wsh_dilithium":{"height":100,"status":"active"}}`, nil},
		{"the member as an array", `,"activation_heights":[{"name":"p2wsh_dilithium","height":100}]`, nil},
		{"the member as a string", `,"activation_heights":"none"`, nil},

		// A height the client cannot read leaves the other names as printed.
		{"one height null beside two numbers", `,"activation_heights":{"p2wsh_dilithium":7,"dilithium_keyhash":null,"v6_controlflow":9}`,
			map[string]int64{"p2wsh_dilithium": 7, "v6_controlflow": 9}},
		{"one height with a fraction beside two numbers", `,"activation_heights":{"p2wsh_dilithium":7,"dilithium_keyhash":8.5,"v6_controlflow":9}`,
			map[string]int64{"p2wsh_dilithium": 7, "v6_controlflow": 9}},
		{"the largest height an int64 holds and one below zero", `,"activation_heights":{"p2wsh_dilithium":9223372036854775807,"v6_controlflow":-1}`,
			map[string]int64{"p2wsh_dilithium": 9223372036854775807, "v6_controlflow": -1}},
	} {
		reply := `{"chain":"main","blocks":500,"headers":500,"bestblockhash":"00","initialblockdownload":false` + c.member + `}`
		client, _ := rpcServer(t, func(string, []interface{}) string { return ok(reply) })
		client.Network = types.Mainnet
		ctx := context.Background()

		info, err := client.GetBlockchainInfo(ctx)
		if err != nil {
			t.Errorf("%s: GetBlockchainInfo: %v, want no error", c.name, err)
			continue
		}
		if info.Chain != "main" || info.Blocks != 500 || info.Headers != 500 || info.InitialSync {
			t.Errorf("%s: the reply was read as %+v", c.name, info)
		}
		if len(info.ActivationHeights) != len(c.want) {
			t.Errorf("%s: heights %v, want %v", c.name, info.ActivationHeights, c.want)
		}
		for name, want := range c.want {
			if got, held := info.ActivationHeights[name]; !held || got != want {
				t.Errorf("%s: %s is %d (held: %v), want %d", c.name, name, got, held, want)
			}
		}
		if err := client.RequireSynced(ctx); err != nil {
			t.Errorf("%s: RequireSynced: %v, want no error", c.name, err)
		}
		if err := client.RequireChain(ctx, types.Mainnet.ChainID); err != nil {
			t.Errorf("%s: RequireChain: %v, want no error", c.name, err)
		}
	}
}
