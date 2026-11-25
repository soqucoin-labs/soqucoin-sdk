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
	tipU       = "4444444444444444444444444444444444444444444444444444444444444444"
)

// best is a getblockchaininfo answer with the members TxConfirmations reads.
type best struct {
	hash   string
	height int64
}

func (b best) reply() string {
	return ok(fmt.Sprintf(`{"chain":"main","blocks":%d,"headers":%d,"bestblockhash":%q}`, b.height, b.height, b.hash))
}

// sweepNode answers the four reads of TxConfirmations: the transaction, the
// best block, the header of the transaction's block, the best block again.
// header is the raw result of getblockheader; asked records every call in order.
func sweepNode(t *testing.T, tx string, first best, header string, second best) (*Client, *[]string) {
	t.Helper()
	var asked []string
	bestReads := 0
	client, _ := rpcServer(t, func(method string, params []interface{}) string {
		asked = append(asked, method)
		switch method {
		case "getrawtransaction":
			if !reflect.DeepEqual(params, []interface{}{sweepTxID, true}) {
				t.Errorf("getrawtransaction asked with %v, want the txid and the verbose flag", params)
			}
			return ok(tx)
		case "getblockchaininfo":
			bestReads++
			if bestReads == 1 {
				return first.reply()
			}
			return second.reply()
		case "getblockheader":
			if !reflect.DeepEqual(params, []interface{}{sweepBlock, true}) {
				t.Errorf("getblockheader asked with %v, want the block hash and the verbose flag", params)
			}
			return ok(header)
		}
		t.Errorf("method %q", method)
		return ok("null")
	})
	return client, &asked
}

const minedTx = `{"txid":"` + sweepTxID + `","blockhash":"` + sweepBlock + `","confirmations":300}`

func headerAt(confirmations, height int64) string {
	return fmt.Sprintf(`{"hash":%q,"confirmations":%d,"height":%d}`, sweepBlock, confirmations, height)
}

// Between two calls the node can move its tip to the valid part of a heavier
// branch and back: a block of that branch fails validation, and the node
// returns to the old tip (ActivateBestChain releases its lock between steps).
// That part can be taller than the old tip while it carries less work. Both
// reads of the best block then say T while the count was taken at the taller
// tip, so it is more than the count at T. Equal best blocks alone would pass
// that count to HoldFinal with T. The count must also be the one T's height
// gives.
func TestTxConfirmationsRefusesACountTakenAtATallerTipBetweenTwoEqualReads(t *testing.T) {
	// T is at 1000 and the sweep's block at 701: 300 confirmations at T. The
	// header was read while the tip stood at 1010, so it says 310.
	client, _ := sweepNode(t, minedTx, best{tipT, 1000}, headerAt(310, 701), best{tipT, 1000})
	n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
	if err != nil || n != 0 || tip != "" {
		t.Fatalf("got %d confirmations at %q, %v; want 0 and no best block", n, tip, err)
	}
}

// The count is paired with the best block only when the two reads of it are
// the same block and the count is the one that block's height gives; in every
// other case the answer is 0 and no best block, which HoldFinal holds.
func TestTxConfirmationsPairsTheCountWithTheBestBlockOnlyWhenTheReadsAgree(t *testing.T) {
	for _, c := range []struct {
		name          string
		tx            string
		first, second best
		header        string
		wantN         int64
		wantTip       string
		wantAsked     []string
	}{
		{"the reads agree", minedTx, best{tipT, 1000}, best{tipT, 1000}, headerAt(300, 701), 300, tipT,
			[]string{"getrawtransaction", "getblockchaininfo", "getblockheader", "getblockchaininfo"}},
		{"a count one past the 288-block horizon", minedTx, best{tipT, 1000}, best{tipT, 1000}, headerAt(289, 712), 289, tipT, nil},
		{"the tip moved forward between the reads", minedTx, best{tipT, 1000}, best{tipU, 1001}, headerAt(301, 701), 0, "", nil},
		{"the tip changed at the same height", minedTx, best{tipT, 1000}, best{tipU, 1000}, headerAt(300, 701), 0, "", nil},
		{"the block is off the active chain", minedTx, best{tipT, 1000}, best{tipT, 1000}, headerAt(-1, 701), 0, "", nil},
		// -1 is the count the height gives for a block two above the tip: a
		// branch the node left for a shorter one with more work.
		{"the block is off the active chain two above the tip", minedTx, best{tipT, 1000}, best{tipT, 1000}, headerAt(-1, 1002), 0, "", nil},
		{"the node names no best block", minedTx, best{"", 1000}, best{"", 1000}, headerAt(300, 701), 0, "", nil},
		{"the transaction is in the mempool", `{"txid":"` + sweepTxID + `"}`, best{tipT, 1000}, best{tipT, 1000}, headerAt(300, 701), 0, "",
			[]string{"getrawtransaction"}},
	} {
		client, asked := sweepNode(t, c.tx, c.first, c.header, c.second)
		n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
		if err != nil || n != c.wantN || tip != c.wantTip {
			t.Errorf("%s: got %d confirmations at %q, %v; want %d at %q", c.name, n, tip, err, c.wantN, c.wantTip)
		}
		if c.wantAsked != nil && !reflect.DeepEqual(*asked, c.wantAsked) {
			t.Errorf("%s: asked %v, want %v", c.name, *asked, c.wantAsked)
		}
	}
}

// A header without the count or the height is an error, not a zero: a missing
// height read as 0 would make the count the one of the genesis block.
func TestTxConfirmationsRefusesAHeaderItCannotRead(t *testing.T) {
	for _, c := range []struct{ name, header string }{
		{"no confirmations", `{"hash":"` + sweepBlock + `","height":701}`},
		{"no height", `{"hash":"` + sweepBlock + `","confirmations":300}`},
		{"the header as a string, the form without the verbose flag", `"00aa"`},
	} {
		client, _ := sweepNode(t, minedTx, best{tipT, 1000}, c.header, best{tipT, 1000})
		n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
		if err == nil || n != 0 || tip != "" {
			t.Errorf("%s: got %d confirmations at %q, %v; want an error", c.name, n, tip, err)
		}
	}
}

// An error of the node on any of the four reads is passed on, with no count
// and no best block. Without txindex=1 the node does not find a transaction
// whose outputs are all spent, and says so.
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
			return ok(headerAt(300, 701))
		})
		n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
		if err == nil || !strings.Contains(err.Error(), failing) || n != 0 || tip != "" {
			t.Errorf("%s fails: got %d confirmations at %q, %v; want its error", failing, n, tip, err)
		}
	}
}

// The second read of the best block is the one that can fail alone.
func TestTxConfirmationsPassesOnAFailureOfTheSecondBestBlockRead(t *testing.T) {
	bestReads := 0
	client, _ := rpcServer(t, func(method string, _ []interface{}) string {
		switch method {
		case "getrawtransaction":
			return ok(minedTx)
		case "getblockchaininfo":
			bestReads++
			if bestReads == 2 {
				return `{"result":null,"error":{"code":-28,"message":"Loading block index..."},"id":1}`
			}
			return best{tipT, 1000}.reply()
		}
		return ok(headerAt(300, 701))
	})
	n, tip, err := client.TxConfirmations(context.Background(), sweepTxID)
	if err == nil || n != 0 || tip != "" {
		t.Errorf("got %d confirmations at %q, %v; want an error", n, tip, err)
	}
}
