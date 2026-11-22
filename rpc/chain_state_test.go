// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package rpc

import (
	"context"
	"math/big"
	"reflect"
	"testing"
)

// getnetworkhashps is a number. Null read into a float would be zero, and zero
// is an answer the node gives, so a reply that is no number is an error and
// never a rate.
func TestGetNetworkHashPSRefusesAReplyThatIsNoRate(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		want        float64
		fails       bool
	}{
		{"a rate", `12345.5`, 12345.5, false},
		{"exponent form, as the node prints a large rate", `1.5e+12`, 1.5e12, false},
		{"zero", `0`, 0, false},
		{"null", `null`, 0, true},
		{"below zero", `-1`, 0, true},
		{"a string", `"12345"`, 0, true},
		{"an object", `{"rate":1}`, 0, true},
	} {
		var asked []interface{}
		client, _ := rpcServer(t, func(method string, params []interface{}) string {
			if method != "getnetworkhashps" {
				t.Errorf("%s: method %q", c.name, method)
			}
			asked = params
			return ok(c.reply)
		})
		got, err := client.GetNetworkHashPS(context.Background(), 120)
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("%s: %v, %v", c.name, got, err)
		}
		if !reflect.DeepEqual(asked, []interface{}{float64(120)}) {
			t.Errorf("%s: asked with %v, want the number of blocks", c.name, asked)
		}
	}
}

// getchaintips is read whole or not at all: an entry without a member would
// read as height 0 or a branch of no length, and a reply without exactly one
// active tip names no chain to measure the others against.
func TestGetChainTipsRefusesAReplyItCannotReadWhole(t *testing.T) {
	const active = `{"height":1000,"hash":"aa","branchlen":0,"status":"active"}`
	for _, c := range []struct {
		name, reply string
		want        []ChainTip
	}{
		{"the active tip alone", `[` + active + `]`, []ChainTip{{Height: 1000, Hash: "aa", Status: ChainTipActive}}},
		{"a branch beside it", `[` + active + `,{"height":998,"hash":"bb","branchlen":3,"status":"valid-fork"}]`,
			[]ChainTip{{Height: 1000, Hash: "aa", Status: ChainTipActive}, {Height: 998, Hash: "bb", BranchLen: 3, Status: "valid-fork"}}},
		{"a status this client does not name", `[` + active + `,{"height":998,"hash":"bb","branchlen":3,"status":"later"}]`,
			[]ChainTip{{Height: 1000, Hash: "aa", Status: ChainTipActive}, {Height: 998, Hash: "bb", BranchLen: 3, Status: "later"}}},
		{"null", `null`, nil},
		{"no tips", `[]`, nil},
		{"an object", `{"height":1000}`, nil},
		{"no active tip", `[{"height":998,"hash":"bb","branchlen":3,"status":"valid-fork"}]`, nil},
		{"two active tips", `[` + active + `,` + active + `]`, nil},
		{"an entry without a height", `[` + active + `,{"hash":"bb","branchlen":3,"status":"valid-fork"}]`, nil},
		{"an entry without a hash", `[` + active + `,{"height":998,"branchlen":3,"status":"valid-fork"}]`, nil},
		{"an entry without a length", `[` + active + `,{"height":998,"hash":"bb","status":"valid-fork"}]`, nil},
		{"an entry without a status", `[` + active + `,{"height":998,"hash":"bb","branchlen":3}]`, nil},
		{"a null height", `[` + active + `,{"height":null,"hash":"bb","branchlen":3,"status":"valid-fork"}]`, nil},
		{"a height below zero", `[` + active + `,{"height":-1,"hash":"bb","branchlen":3,"status":"valid-fork"}]`, nil},
		{"a length below zero", `[` + active + `,{"height":998,"hash":"bb","branchlen":-3,"status":"valid-fork"}]`, nil},
		{"a height as a string", `[` + active + `,{"height":"998","hash":"bb","branchlen":3,"status":"valid-fork"}]`, nil},
	} {
		client, _ := rpcServer(t, func(method string, params []interface{}) string {
			if method != "getchaintips" || len(params) != 0 {
				t.Errorf("%s: asked %q with %v", c.name, method, params)
			}
			return ok(c.reply)
		})
		got, err := client.GetChainTips(context.Background())
		if (err != nil) != (c.want == nil) || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, %v", c.name, got, err)
		}
	}
}

// The chainwork of getblockheader is 64 hexadecimal digits. A reply without
// it, or with one that is no positive number, is an error: a work of zero
// would make every other chain the heavier one, and one that is no number
// would make none.
func TestGetChainWorkRefusesAReplyWithoutAPositiveWork(t *testing.T) {
	const hash = "00000000000000000000000000000000000000000000000000000000000000aa"
	big64, _ := new(big.Int).SetString("00000000000000000000000000000000000000000000001a2b3c4d5e6f708192", 16)
	for _, c := range []struct {
		name, reply string
		want        *big.Int
	}{
		{"a work as the node prints it", `{"hash":"aa","height":7,"chainwork":"00000000000000000000000000000000000000000000001a2b3c4d5e6f708192"}`, big64},
		{"a short work", `{"chainwork":"ff"}`, big.NewInt(255)},
		{"no chainwork", `{"hash":"aa","height":7}`, nil},
		{"a null chainwork", `{"chainwork":null}`, nil},
		{"an empty chainwork", `{"chainwork":""}`, nil},
		{"a chainwork that is no hexadecimal", `{"chainwork":"xyz"}`, nil},
		{"a chainwork of zero", `{"chainwork":"0000000000000000000000000000000000000000000000000000000000000000"}`, nil},
		{"a chainwork below zero", `{"chainwork":"-ff"}`, nil},
		{"a chainwork as a number", `{"chainwork":255}`, nil},
		{"the header as a string, the form without the verbose flag", `"00aa"`, nil},
		{"null", `null`, nil},
	} {
		var asked []interface{}
		client, _ := rpcServer(t, func(method string, params []interface{}) string {
			if method != "getblockheader" {
				t.Errorf("%s: method %q", c.name, method)
			}
			asked = params
			return ok(c.reply)
		})
		got, err := client.GetChainWork(context.Background(), hash)
		switch {
		case c.want == nil && (err == nil || got != nil):
			t.Errorf("%s: %v, %v, want an error", c.name, got, err)
		case c.want != nil && (err != nil || got == nil || got.Cmp(c.want) != 0):
			t.Errorf("%s: %v, %v, want %v", c.name, got, err, c.want)
		}
		if !reflect.DeepEqual(asked, []interface{}{hash, true}) {
			t.Errorf("%s: asked with %v, want the hash and the verbose flag", c.name, asked)
		}
	}
}

// A node that answers with an error of its own, such as a hash it does not
// hold, is passed on by all three.
func TestTheChainReadersPassOnTheNodesError(t *testing.T) {
	client, _ := rpcServer(t, func(string, []interface{}) string {
		return `{"result":null,"error":{"code":-5,"message":"Block not found"},"id":1}`
	})
	ctx := context.Background()
	if _, err := client.GetNetworkHashPS(ctx, 120); err == nil {
		t.Error("GetNetworkHashPS returned no error")
	}
	if _, err := client.GetChainTips(ctx); err == nil {
		t.Error("GetChainTips returned no error")
	}
	if _, err := client.GetChainWork(ctx, "aa"); err == nil {
		t.Error("GetChainWork returned no error")
	}
}
