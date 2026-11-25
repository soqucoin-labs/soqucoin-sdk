// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package rpc

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	sweepTxID  = "1111111111111111111111111111111111111111111111111111111111111111"
	sweepBlock = "2222222222222222222222222222222222222222222222222222222222222222"
	tipT       = "3333333333333333333333333333333333333333333333333333333333333333"
)

// best is a getblockchaininfo answer with the members TxConfirmations reads.
type best struct {
	hash   string
	height int64
}

func (b best) reply() string {
	return ok(fmt.Sprintf(`{"chain":"main","blocks":%d,"headers":%d,"bestblockhash":%q}`, b.height, b.height, b.hash))
}

// block is a header a scripted node holds: its height, the block before it,
// and the count the node printed with it, which is the one of the tip the node
// stood at when it was asked.
type block struct {
	height        int64
	prev          string
	confirmations int64
}

// onBranch names the block at a height on one branch of a scripted chain.
func onBranch(branch byte, height int64) string {
	return fmt.Sprintf("%c%063x", branch, height)
}

// line returns the headers of one chain from low, at height lowHeight, to
// high, at height highHeight. The blocks between are named by onBranch, and
// each block names the one below it.
func line(branch byte, low string, lowHeight int64, high string, highHeight int64) map[string]block {
	headers := map[string]block{}
	prev := onBranch(branch, lowHeight-1)
	for h := lowHeight; h <= highHeight; h++ {
		hash := onBranch(branch, h)
		switch h {
		case lowHeight:
			hash = low
		case highHeight:
			hash = high
		}
		headers[hash] = block{height: h, prev: prev, confirmations: highHeight - h + 1}
		prev = hash
	}
	return headers
}

// chainNode answers TxConfirmations from the transaction tx, the best block
// tip and the headers it holds. A hash it does not hold gets the node's error.
// asked records each call in order, a getblockheader with its hash.
func chainNode(t *testing.T, tx string, tip best, headers map[string]block) (*Client, *[]string) {
	t.Helper()
	var asked []string
	client, _ := rpcServer(t, func(method string, params []interface{}) string {
		switch method {
		case "getrawtransaction":
			asked = append(asked, method)
			if !reflect.DeepEqual(params, []interface{}{sweepTxID, true}) {
				t.Errorf("getrawtransaction asked with %v, want the txid and the verbose flag", params)
			}
			return ok(tx)
		case "getblockchaininfo":
			asked = append(asked, method)
			return tip.reply()
		case "getblockheader":
			if len(params) != 2 || params[1] != true {
				t.Errorf("getblockheader asked with %v, want a hash and the verbose flag", params)
				return ok("null")
			}
			hash, _ := params[0].(string)
			asked = append(asked, method+" "+hash)
			b, held := headers[hash]
			if !held {
				return rpcErr(-5, "Block not found")
			}
			prev := ""
			if b.prev != "" {
				prev = fmt.Sprintf(`,"previousblockhash":%q`, b.prev)
			}
			return ok(fmt.Sprintf(`{"hash":%q,"confirmations":%d,"height":%d%s}`, hash, b.confirmations, b.height, prev))
		}
		t.Errorf("method %q", method)
		return ok("null")
	})
	return client, &asked
}

const minedTx = `{"txid":"` + sweepTxID + `","blockhash":"` + sweepBlock + `","confirmations":300}`

// Between two calls the node can move its tip to a block of another branch
// and back. Here the best block is T at 1000. The header of the transaction's
// block was read while the tip stood at U, a block of the same height on a
// branch that holds the transaction's block at 999, so the node printed 2 with
// it. That is the count T's height gives for a block at 999, yet the block is
// not on T's chain and T has no count for the transaction. Only the walk back
// from T by the previous-block links, which never change, shows that.
func TestTxConfirmationsRefusesABlockOffTheBestBlocksChain(t *testing.T) {
	headers := map[string]block{
		tipT:       {height: 1000, prev: onBranch('a', 999), confirmations: 1},
		sweepBlock: {height: 999, prev: onBranch('b', 998), confirmations: 2},
	}
	client, _ := chainNode(t, minedTx, best{tipT, 1000}, headers)
	n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
	if err != nil || n != 0 || tip != "" {
		t.Fatalf("got %d confirmations at %q, %v; want 0 and no best block", n, tip, err)
	}
}

// The count a header carries is the one of the tip the node stood at when it
// printed it. The tip can be a taller block of a branch that also holds the
// transaction's block, on its way to a branch that fails validation and back.
// The count is the one of the best block's height, whatever the header says.
func TestTxConfirmationsCountsFromTheHeightOfTheBestBlock(t *testing.T) {
	// T is at 1000 and the sweep's block at 701: 300 confirmations at T. The
	// header was printed while the tip stood at 1010, so it says 310.
	headers := line('a', sweepBlock, 701, tipT, 1000)
	held := headers[sweepBlock]
	held.confirmations = 310
	headers[sweepBlock] = held
	client, _ := chainNode(t, minedTx, best{tipT, 1000}, headers)
	n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
	if err != nil || n != 300 || tip != tipT {
		t.Fatalf("got %d confirmations at %q, %v; want 300 at T", n, tip, err)
	}
}

// The count is paired with the best block only when the walk back from it
// ends at the transaction's block. In every other case the answer is 0 and no
// best block, which HoldFinal holds.
func TestTxConfirmationsPairsTheCountWithTheBestBlockWhenTheWalkEndsAtTheBlock(t *testing.T) {
	other := onBranch('b', 1000)
	for _, c := range []struct {
		name      string
		tx        string
		tip       best
		headers   map[string]block
		wantN     int64
		wantTip   string
		wantAsked []string
	}{
		{"the block is two below the best block", minedTx, best{tipT, 1000}, line('a', sweepBlock, 998, tipT, 1000), 3, tipT,
			[]string{"getrawtransaction", "getblockchaininfo", "getblockheader " + sweepBlock,
				"getblockheader " + tipT, "getblockheader " + onBranch('a', 999)}},
		{"a count one past the 288-block horizon", minedTx, best{tipT, 1000}, line('a', sweepBlock, 712, tipT, 1000), 289, tipT, nil},
		{"the block is the best block", minedTx, best{sweepBlock, 1000}, line('a', sweepBlock, 1000, sweepBlock, 1000), 1, sweepBlock,
			[]string{"getrawtransaction", "getblockchaininfo", "getblockheader " + sweepBlock}},
		{"another block at the best block's height", minedTx, best{other, 1000},
			map[string]block{sweepBlock: {height: 1000, prev: onBranch('a', 999)}}, 0, "",
			[]string{"getrawtransaction", "getblockchaininfo", "getblockheader " + sweepBlock}},
		// A branch the node left for a shorter one with more work.
		{"the block is above the best block", minedTx, best{tipT, 1000},
			map[string]block{sweepBlock: {height: 1002, prev: onBranch('b', 1001), confirmations: -1}}, 0, "",
			[]string{"getrawtransaction", "getblockchaininfo", "getblockheader " + sweepBlock}},
		{"the block left the chain 300 blocks down", minedTx, best{tipT, 1000},
			mergeHeaders(line('a', onBranch('a', 690), 690, tipT, 1000), map[string]block{sweepBlock: {height: 701, prev: onBranch('b', 700), confirmations: -1}}), 0, "", nil},
		{"the transaction is in the mempool", `{"txid":"` + sweepTxID + `"}`, best{tipT, 1000}, nil, 0, "",
			[]string{"getrawtransaction"}},
	} {
		client, asked := chainNode(t, c.tx, c.tip, c.headers)
		n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
		if err != nil || n != c.wantN || tip != c.wantTip {
			t.Errorf("%s: got %d confirmations at %q, %v; want %d at %q", c.name, n, tip, err, c.wantN, c.wantTip)
		}
		if c.wantAsked != nil && !reflect.DeepEqual(*asked, c.wantAsked) {
			t.Errorf("%s: asked %v, want %v", c.name, *asked, c.wantAsked)
		}
	}
}

func mergeHeaders(sets ...map[string]block) map[string]block {
	all := map[string]block{}
	for _, set := range sets {
		for hash, b := range set {
			all[hash] = b
		}
	}
	return all
}

// A header without the height is an error, not a zero: a missing height read
// as 0 would send the walk to the genesis block.
func TestTxConfirmationsRefusesAHeaderItCannotRead(t *testing.T) {
	for _, c := range []struct{ name, header, want string }{
		{"no height", `{"hash":"` + sweepBlock + `","confirmations":300}`, "no height"},
		{"the header as a string, the form without the verbose flag", `"00aa"`, "parse getblockheader"},
	} {
		client, _ := rpcServer(t, func(method string, _ []interface{}) string {
			switch method {
			case "getrawtransaction":
				return ok(minedTx)
			case "getblockchaininfo":
				return best{tipT, 1000}.reply()
			}
			return ok(c.header)
		})
		n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
		if err == nil || !strings.Contains(err.Error(), c.want) || n != 0 || tip != "" {
			t.Errorf("%s: got %d confirmations at %q, %v; want an error that says %q", c.name, n, tip, err, c.want)
		}
	}
}

// An error of the node on any read is passed on, with no count and no best
// block. Without txindex=1 the node does not find a transaction whose outputs
// are all spent, and says so. A header the walk asks for and the node does
// not hold is an error too.
func TestTxConfirmationsPassesOnTheNodesError(t *testing.T) {
	const notFound = `{"result":null,"error":{"code":-5,"message":"No such mempool transaction. Use -txindex to enable blockchain transaction queries"},"id":1}`
	for _, failing := range []string{"getrawtransaction", "getblockchaininfo", "getblockheader"} {
		client, _ := rpcServer(t, func(method string, _ []interface{}) string {
			switch method {
			case failing:
				return notFound
			case "getrawtransaction":
				return ok(minedTx)
			case "getblockchaininfo":
				return best{tipT, 1000}.reply()
			}
			return ok(`{"hash":"` + sweepBlock + `","confirmations":300,"height":701}`)
		})
		n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
		if err == nil || !strings.Contains(err.Error(), failing) || n != 0 || tip != "" {
			t.Errorf("%s fails: got %d confirmations at %q, %v; want its error", failing, n, tip, err)
		}
	}
	headers := line('a', sweepBlock, 701, tipT, 1000)
	delete(headers, onBranch('a', 850))
	client, _ := chainNode(t, minedTx, best{tipT, 1000}, headers)
	n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
	if err == nil || !strings.Contains(err.Error(), "getblockheader") || n != 0 || tip != "" {
		t.Errorf("a header on the walk is missing: got %d confirmations at %q, %v; want the node's error", n, tip, err)
	}
}
