// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/htlc"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// replies is a node that answers from recorded values and keeps what it was
// sent.
type replies struct {
	info         rpc.BlockchainInfo
	outs         map[string]*rpc.TxOut
	blocks       map[int64]string
	broadcastErr error
	calls        int
	sent         []string
	withMempool  []bool
	read         []int64
	// later, when set, is the tip from the second read of the chain on.
	later, reads int64
}

func (r *replies) GetBlockchainInfo(context.Context) (*rpc.BlockchainInfo, error) {
	r.calls++
	r.reads++
	info := r.info
	if r.later != 0 && r.reads > 1 {
		info.Blocks, info.Headers = r.later, r.later
	}
	return &info, nil
}

func (r *replies) GetTxOut(_ context.Context, txid string, vout uint32, includeMempool bool) (*rpc.TxOut, error) {
	r.calls++
	r.withMempool = append(r.withMempool, includeMempool)
	out, ok := r.outs[fmt.Sprintf("%s:%d", strings.ToLower(txid), vout)] // a node takes an id in either case
	if !ok {
		return nil, nil
	}
	held := *out
	return &held, nil
}

func (r *replies) Broadcast(_ context.Context, rawTxHex, txid string) (string, error) {
	r.calls++
	if r.broadcastErr != nil {
		return "", r.broadcastErr
	}
	r.sent = append(r.sent, rawTxHex)
	return txid, nil
}

func (r *replies) GetBlockHash(_ context.Context, height int64) (string, error) {
	r.calls++
	return fmt.Sprint(height), nil
}

func (r *replies) GetBlock(_ context.Context, hash string, _ int) (json.RawMessage, error) {
	r.calls++
	var height int64
	fmt.Sscan(hash, &height)
	r.read = append(r.read, height)
	block, ok := r.blocks[height]
	if !ok {
		block = `{"tx":[]}`
	}
	return json.RawMessage(fmt.Sprintf(`{"previousblockhash":"%d",`, height-1) + block[1:]), nil
}

const (
	benchTip    = 1_000
	benchLock   = benchTip + 30 + 6 // the depth of one SOQ and an inclusion of 6 above the tip
	benchAmount = types.ShorsPerSOQ
	passphrase  = "a passphrase for the bench"
)

var (
	coin     = strings.Repeat("c1", 32) + ":0" // an output of the payer's address
	contract = strings.Repeat("7a", 32) + ":1" // the funded contract
)

// bench is a keystore file with a payer's and a payee's key, a contract
// between them, and a regtest node whose tip leaves the contract claimable.
type bench struct {
	t              *testing.T
	dir, keystore  string
	manager        *keys.Manager
	payer, payee   string
	secret         [32]byte
	p              htlc.Params
	node           *replies
	environment    map[string]string
	stdout, stderr bytes.Buffer
	dialed         []string
	credentials    []string
	// closed makes every write of the result stream fail.
	closed bool
}

// unwritable is a result stream that takes nothing.
type unwritable struct{}

func (unwritable) Write([]byte) (int, error) { return 0, errors.New("the stream is closed") }

func newBench(t *testing.T) *bench {
	t.Helper()
	b := &bench{t: t, dir: t.TempDir(), environment: map[string]string{"SOQ_KEYSTORE_PASSPHRASE": passphrase}}
	b.keystore = filepath.Join(b.dir, "keys.enc")
	b.manager = keys.NewManager(b.keystore, passphrase)
	if err := b.manager.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	var hashes [2][32]byte
	for i, addr := range []*string{&b.payer, &b.payee} {
		kp, err := keys.GenerateKeyForNetwork(types.Regtest.HRP)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.manager.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
			t.Fatal(err)
		}
		*addr, hashes[i] = kp.Address, sha256.Sum256(kp.PublicKey)
	}
	if err := b.manager.Save(); err != nil {
		t.Fatal(err)
	}
	b.secret = sha256.Sum256([]byte("the bench's secret"))
	b.p = htlc.Params{Hash: sha256.Sum256(b.secret[:]), PayerKeyHash: hashes[0], PayeeKeyHash: hashes[1], LockHeight: benchLock}

	own, err := address.ScriptFor(b.payer)
	if err != nil {
		t.Fatal(err)
	}
	script, err := b.p.OutputScript()
	if err != nil {
		t.Fatal(err)
	}
	b.node = &replies{
		info: rpc.BlockchainInfo{Chain: types.Regtest.ChainID, Blocks: benchTip, Headers: benchTip, BestBlockHash: "tip",
			ActivationHeights: map[string]int64{"p2wsh_dilithium": 0, "dilithium_keyhash": 0, "v6_controlflow": 0}},
		outs: map[string]*rpc.TxOut{
			coin:     {BestBlock: "tip", Confirmations: 100, Value: 5 * benchAmount, ScriptPubKey: rpc.ScriptPubKey{Hex: hex.EncodeToString(own)}},
			contract: {BestBlock: "tip", Confirmations: 30, Value: benchAmount, ScriptPubKey: rpc.ScriptPubKey{Hex: hex.EncodeToString(script)}},
		},
	}
	return b
}

// run runs the command with the bench's environment and node.
func (b *bench) run(args ...string) error {
	b.stdout.Reset()
	b.stderr.Reset()
	var stdout io.Writer = &b.stdout
	if b.closed {
		stdout = unwritable{}
	}
	e := env{
		getenv: func(name string) string { return b.environment[name] },
		stdout: stdout, stderr: &b.stderr,
		dial: func(url, user, password string, network types.Network) htlc.Node {
			b.dialed = append(b.dialed, network.Name+" "+url)
			b.credentials = append(b.credentials, "user "+user+" password "+password)
			return b.node
		},
	}
	return e.run(context.Background(), args)
}

func (b *bench) contract() []string {
	return []string{"-network", "regtest", "-hash", hex.EncodeToString(b.p.Hash[:]),
		"-payee-key-hash", hex.EncodeToString(b.p.PayeeKeyHash[:]), "-payer-key-hash", hex.EncodeToString(b.p.PayerKeyHash[:]),
		"-lock-height", fmt.Sprint(b.p.LockHeight)}
}

func (b *bench) as(addr string) []string { return []string{"-keystore", b.keystore, "-address", addr} }

// file writes content to a new file of the bench's directory with the mode.
func (b *bench) file(name, content string, mode os.FileMode) string {
	b.t.Helper()
	path := filepath.Join(b.dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		b.t.Fatal(err)
	}
	return path
}

// pays reports whether the raw transaction holds the payment script of addr.
func (b *bench) pays(rawHex, addr string) bool {
	b.t.Helper()
	script, err := address.ScriptFor(addr)
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Contains(rawHex, hex.EncodeToString(script))
}

func args(groups ...[]string) []string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

// record reads a record file and requires that its owner alone can read it.
func (b *bench) record(path string) (step, txid, rawHex string) {
	b.t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		b.t.Fatalf("the record: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 && runtime.GOOS != "windows" {
		b.t.Errorf("the record has mode %04o, want 0600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.t.Fatal(err)
	}
	var r struct {
		Step   string `json:"step"`
		TxID   string `json:"txid"`
		RawHex string `json:"raw_hex"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		b.t.Fatalf("the record %q: %v", data, err)
	}
	return r.Step, r.TxID, r.RawHex
}

func TestScriptAndKeyHashPrintWhatTheLibraryDerives(t *testing.T) {
	b := newBench(t)
	if err := b.run(args([]string{"script"}, b.contract())...); err != nil {
		t.Fatal(err)
	}
	script, _ := b.p.Script()
	output, _ := b.p.OutputScript()
	addr, _ := b.p.Address(types.Regtest.HRP)
	want := fmt.Sprintf("witness script  %x\noutput script   %x\naddress         %s\n", script, output, addr)
	if b.stdout.String() != want {
		t.Errorf("script printed\n%s\nwant\n%s", b.stdout.String(), want)
	}
	if b.node.calls != 0 || len(b.dialed) != 0 {
		t.Errorf("script asked a node: %d calls, dialed %v", b.node.calls, b.dialed)
	}

	if err := b.run(args([]string{"key-hash"}, b.as(b.payee))...); err != nil {
		t.Fatal(err)
	}
	if got, want := b.stdout.String(), hex.EncodeToString(b.p.PayeeKeyHash[:])+"\n"; got != want {
		t.Errorf("key-hash printed %q, want %q", got, want)
	}
}

// fund writes the signed transaction to the record before the node has it,
// and a second run under the same record builds nothing and asks no node.
func TestFundRecordsThenSendsAndRefusesASecondRun(t *testing.T) {
	b := newBench(t)
	rec := filepath.Join(b.dir, "fund.json")
	fund := args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin, "-record", rec})
	if err := b.run(fund...); err != nil {
		t.Fatal(err)
	}
	step, txid, rawHex := b.record(rec)
	if step != "fund" || len(b.node.sent) != 1 || b.node.sent[0] != rawHex {
		t.Fatalf("the record holds step %q and the node was sent %d transactions; want the fund's bytes recorded and sent once", step, len(b.node.sent))
	}
	if want := fmt.Sprintf("funded %s:0 with %d shors\n", txid, benchAmount); b.stdout.String() != want {
		t.Errorf("printed %q, want %q", b.stdout.String(), want)
	}
	if len(b.dialed) != 1 || b.dialed[0] != fmt.Sprintf("regtest http://127.0.0.1:%d", types.Regtest.RPCPort) {
		t.Errorf("dialed %v, want the regtest port on this machine", b.dialed)
	}
	if len(b.node.withMempool) != 1 || !b.node.withMempool[0] {
		t.Errorf("the input was read with the mempool %v, want once with it, so that an output a transaction there spends is refused", b.node.withMempool)
	}
	if !b.pays(rawHex, b.payer) || b.pays(rawHex, b.payee) {
		t.Error("with no -change named, the change does not go to the caller's own address")
	}

	calls := b.node.calls
	err := b.run(fund...)
	if err == nil || !strings.Contains(err.Error(), rec) || len(b.node.sent) != 1 || b.node.calls != calls {
		t.Errorf("a second run under the same record: got %v, %d broadcasts and %d more calls of the node; want a refusal that names the record and nothing else", err, len(b.node.sent), b.node.calls-calls)
	}
	if _, again, _ := b.record(rec); again != txid {
		t.Errorf("the record changed from %s to %s", txid, again)
	}
}

// The flags of fund reach the library: the change address, the fee rate, the
// lock margin and the depth.
func TestFundPassesItsFlagsOn(t *testing.T) {
	fund := func(b *bench, more ...string) error {
		return b.run(args([]string{"fund"}, b.contract(), b.as(b.payer),
			[]string{"-amount", "1", "-inputs", coin, "-record", filepath.Join(b.dir, "fund.json")}, more)...)
	}
	b := newBench(t)
	if err := fund(b, "-change", b.payee); err != nil {
		t.Fatal(err)
	}
	if _, _, rawHex := b.record(filepath.Join(b.dir, "fund.json")); !b.pays(rawHex, b.payee) || b.pays(rawHex, b.payer) {
		t.Error("the change does not go to the address -change names")
	}
	for name, c := range map[string]struct {
		flag, value string
		want        error
	}{
		"a fee rate of zero":                     {"-fee-rate", "0", tx.ErrInvalidAmount},
		"a lock margin one above what is left":   {"-lock-margin", "37", htlc.ErrLockTooNear},
		"a depth below the floor":                {"-depth", "29", htlc.ErrDepth},
		"a depth the rules have not been active": {"-depth", "1001", htlc.ErrNotActive},
	} {
		b := newBench(t)
		if err := fund(b, c.flag, c.value); !errors.Is(err, c.want) || len(b.node.sent) != 0 {
			t.Errorf("%s: got %v and %d broadcasts, want %v and none", name, err, len(b.node.sent), c.want)
		}
	}
	b = newBench(t)
	if err := fund(b, "-lock-margin", "36"); err != nil {
		t.Errorf("a lock margin of what is left: %v", err)
	}
}

// The record is made exclusively: a file that appears at the path between the
// look and the write is not written over.
func TestTheRecordIsNeverWrittenOverAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	record, err := recorder(path, "fund")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("an earlier record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := record("id", "00"); err == nil {
		t.Error("the record was written over a file that appeared after the look")
	}
	if data, _ := os.ReadFile(path); string(data) != "an earlier record" {
		t.Errorf("the file now holds %q", data)
	}
}

// A path with a symbolic link followed by ".." names one file to the kernel
// and another once it is cleaned as text. The record and the secret file are
// each looked at, made and read under one spelling, so the refusal of a file
// that exists and the refusal by mode act on the file that is written or read.
func TestTheRecordAndTheSecretFileAreOneFileUnderEverySpelling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links and file modes")
	}
	dir := t.TempDir()
	for _, sub := range []string{filepath.Join(dir, "real", "sub"), filepath.Join(dir, "cwd")} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "real", "sub"), filepath.Join(dir, "cwd", "link")); err != nil {
		t.Fatal(err)
	}
	// As text the two names below are files of cwd; the kernel follows the
	// link first and reads them as files of real.
	named := func(name string) string { return filepath.Join(dir, "cwd") + "/link/../" + name }
	kernel := func(name string) string { return filepath.Join(dir, "real", name) }
	lexical := func(name string) string { return filepath.Join(dir, "cwd", name) }
	holds := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			return "absent"
		}
		return string(data)
	}

	// With a file at one reading of the name alone, the look and the create
	// agree: either the look refuses, before anything is signed, or the
	// record is made. A look that passes before a create that refuses means
	// the two acted on different files.
	for _, at := range []func(string) string{lexical, kernel} {
		for _, path := range []string{kernel("held.json"), lexical("held.json")} {
			_ = os.Remove(path)
		}
		if err := os.WriteFile(at("held.json"), []byte("held"), 0o600); err != nil {
			t.Fatal(err)
		}
		if r, err := recorder(named("held.json"), "fund"); err == nil {
			if err := r("id", "00"); err != nil {
				t.Errorf("the look passed and the record then refused: %v", err)
			}
		}
	}

	record, err := recorder(named("rec.json"), "fund")
	if err != nil {
		t.Fatal(err)
	}
	// A file appears after the look at the place the kernel reads the name as.
	// It is not written over, and no second file is left behind.
	if err := os.WriteFile(kernel("rec.json"), []byte("an earlier record"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = record("id", "00")
	if got := holds(kernel("rec.json")); got != "an earlier record" {
		t.Errorf("the file the kernel reads the name as now holds %q (record returned %v)", got, err)
	}
	if err == nil {
		if got := holds(lexical("rec.json")); !strings.Contains(got, `"txid":"id"`) {
			t.Errorf("the record returned no error and the file it made holds %q", got)
		}
	} else if got := holds(lexical("rec.json")); got != "absent" {
		t.Errorf("the record refused (%v) and left a file holding %q", err, got)
	}

	digits := strings.Repeat("ab", 32)
	for path, mode := range map[string]os.FileMode{kernel("secret"): 0o600, lexical("secret"): 0o644} {
		if err := os.WriteFile(path, []byte(strings.Repeat("cd", 32)), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(kernel("secret"), []byte(digits), 0o600); err != nil {
		t.Fatal(err)
	}
	// The mode is read at the file the bytes are read from: either the file
	// of mode 0600 is read, or the file of mode 0644 is refused.
	got, err := readSecret(named("secret"))
	if err == nil && hex.EncodeToString(got[:]) != digits {
		t.Errorf("the secret was read from a file of mode 0644 after the mode of another file was checked")
	}
	// The same with the two files exchanged: the file of mode 0600 at the name
	// cleaned as text, the file of mode 0644 where the kernel reads the name.
	for path, mode := range map[string]os.FileMode{lexical("mirrored"): 0o600, kernel("mirrored"): 0o644} {
		held := strings.Repeat("cd", 32)
		if mode == 0o600 {
			held = digits
		}
		if err := os.WriteFile(path, []byte(held), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	got, err = readSecret(named("mirrored"))
	if err == nil && hex.EncodeToString(got[:]) != digits {
		t.Errorf("with the two files exchanged, the secret was read from the file of mode 0644 after the mode of the other was checked")
	}
}

// Every text that names the record names the file the transaction is in. With
// -record given through a symbolic link followed by "..", the name as given
// and the name cleaned as text are two files: a text with one of them beside
// a record at the other sends the caller to a file that is absent, or that
// holds an earlier transaction.
func TestEveryTextNamesTheFileTheRecordIsIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links")
	}
	states := map[string]func(*bench){
		"an unknown outcome":                   func(b *bench) { b.node.broadcastErr = rpc.ErrUnknownOutcome },
		"a result line that cannot be written": func(b *bench) { b.closed = true },
		"a claim held back by the second read": func(b *bench) { b.node.later = benchTip + 1 },
	}
	for state, arrange := range states {
		for step, run := range steps {
			if state == "a claim held back by the second read" && step != "claim" {
				continue
			}
			b := newBench(t)
			for _, sub := range []string{filepath.Join(b.dir, "real", "sub"), filepath.Join(b.dir, "cwd")} {
				if err := os.MkdirAll(sub, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(b.dir, "real", "sub"), filepath.Join(b.dir, "cwd", "link")); err != nil {
				t.Fatal(err)
			}
			given := filepath.Join(b.dir, "cwd") + "/link/../" + step + ".json"
			cleaned := filepath.Join(b.dir, "cwd", step+".json")
			arrange(b)
			err := run(b, given)
			if err == nil {
				t.Errorf("%s of %s: no error", state, step)
				continue
			}
			// The text names the record under one of the two spellings. Opened
			// as printed, that file holds the transaction the text names.
			text, spelled := err.Error(), cleaned
			if strings.Contains(text, given) {
				spelled = given
			} else if !strings.Contains(text, cleaned) {
				t.Errorf("%s of %s: the text names no record: %v", state, step, err)
				continue
			}
			data, readErr := os.ReadFile(spelled)
			var held struct {
				TxID string `json:"txid"`
			}
			if readErr != nil || json.Unmarshal(data, &held) != nil || held.TxID == "" {
				t.Errorf("%s of %s: the text names %s, which holds no record (%v): %v", state, step, spelled, readErr, err)
				continue
			}
			if !strings.Contains(text, held.TxID) {
				t.Errorf("%s of %s: the file the text names holds transaction %s, which the text does not name: %v", state, step, held.TxID, err)
			}
		}
	}
}

// After the node took the transaction, a result line that cannot be written
// is no refusal of the step. The error says that the transaction was sent and
// names it and its record, so that a caller who reads the exit status as "not
// funded" does not fund a second time.
func TestAResultLineThatCannotBeWrittenNamesTheSentTransaction(t *testing.T) {
	for step, run := range steps {
		b := newBench(t)
		b.closed = true
		rec := filepath.Join(b.dir, step+".json")
		err := run(b, rec)
		if len(b.node.sent) != 1 {
			t.Fatalf("%s: the node was sent %d transactions, want one", step, len(b.node.sent))
		}
		_, txid, _ := b.record(rec)
		if err == nil {
			t.Errorf("%s: no error for a result line that could not be written", step)
			continue
		}
		for _, want := range []string{"the node took", txid, rec, "the stream is closed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the error does not hold %q: %v", step, want, err)
			}
		}
	}
}

// An input the node does not hold as a confirmed output of the caller's
// address is refused, and no record is written.
func TestFundRefusesAnInputThatIsNotTheCallersOwn(t *testing.T) {
	for name, change := range map[string]func(*bench) string{
		"an outpoint the node does not hold": func(b *bench) string { return strings.Repeat("c2", 32) + ":0" },
		"an unconfirmed output":              func(b *bench) string { b.node.outs[coin].Confirmations = 0; return coin },
		"an output of the payee's address": func(b *bench) string {
			script, _ := address.ScriptFor(b.payee)
			b.node.outs[coin].ScriptPubKey.Hex = hex.EncodeToString(script)
			return coin
		},
		"no outpoint": func(b *bench) string { return "" },
	} {
		b := newBench(t)
		rec := filepath.Join(b.dir, "fund.json")
		input := change(b)
		err := b.run(args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", input, "-record", rec})...)
		if err == nil || !strings.Contains(err.Error(), "-inputs") || len(b.node.sent) != 0 {
			t.Errorf("%s: got %v and %d broadcasts, want a refusal of -inputs and none", name, err, len(b.node.sent))
		}
		if _, statErr := os.Lstat(rec); statErr == nil {
			t.Errorf("%s: a record was written", name)
		}
	}
}

// An outpoint named twice would be counted twice and signed into a
// transaction no node takes, and a coinbase output below the network's
// maturity cannot be spent yet. Both are refused before the keystore signs
// and before the record is made.
func TestFundRefusesAnInputNamedTwiceOrACoinbaseBelowMaturity(t *testing.T) {
	id, _, _ := strings.Cut(coin, ":")
	for name, c := range map[string]struct {
		inputs string
		change func(*bench)
		want   string
	}{
		"the same outpoint twice":             {coin + "," + coin, nil, "twice"},
		"the same outpoint, once upper case":  {coin + "," + strings.ToUpper(id) + ":0", nil, "twice"},
		"the same outpoint, the index padded": {coin + ", " + id + ":00", nil, "twice"},
		"a coinbase one block below maturity": {coin, func(b *bench) {
			b.node.outs[coin].Coinbase, b.node.outs[coin].Confirmations = true, types.Regtest.CoinbaseMaturity-1
		}, "coinbase"},
	} {
		b := newBench(t)
		if c.change != nil {
			c.change(b)
		}
		rec := filepath.Join(b.dir, "fund.json")
		err := b.run(args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", c.inputs, "-record", rec})...)
		if err == nil || !strings.Contains(err.Error(), "-inputs") || !strings.Contains(err.Error(), c.want) || len(b.node.sent) != 0 {
			t.Errorf("%s: got %v and %d broadcasts, want a refusal of -inputs that says %q and none", name, err, len(b.node.sent), c.want)
		}
		if _, statErr := os.Lstat(rec); statErr == nil {
			t.Errorf("%s: a record was written", name)
		}
	}

	b := newBench(t)
	b.node.outs[coin].Coinbase, b.node.outs[coin].Confirmations = true, types.Regtest.CoinbaseMaturity
	if err := b.run(args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin, "-record", filepath.Join(b.dir, "fund.json")})...); err != nil {
		t.Errorf("a coinbase at maturity: got %v, want the funding sent", err)
	}
}

// A value passed to the wrong flag may be the secret. No refusal of the
// parser, of the amount, the depth, the outpoint, the inputs or -secret-file
// prints it back, in the error or on the error stream, and -h still prints
// the flags. The refusals of -keystore and of key-hash -address print their
// value and are left out.
func TestNoRefusalOfTheArgumentsPrintsAValue(t *testing.T) {
	b := newBench(t)
	value := hex.EncodeToString(b.secret[:])
	with := func(name string) []string {
		c := b.contract()
		for i := range c {
			if c[i] == name {
				c[i+1] = value
			}
		}
		return c
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"as the lock height":    {args([]string{"script"}, with("-lock-height")), "-lock-height"},
		"as the depth":          {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-depth", value, "-outpoint", contract}), "-depth"},
		"as the amount":         {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", value, "-outpoint", contract}), "-amount"},
		"as the outpoint":       {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", value}), "-outpoint"},
		"as an input":           {args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", value, "-record", filepath.Join(b.dir, "r.json")}), "-inputs"},
		"as an argument":        {args([]string{"script"}, b.contract(), []string{value}), "unexpected argument"},
		"as the name of a flag": {args([]string{"script"}, b.contract(), []string{"-" + value}), "flag provided but not defined"},
		"as the secret file": {args([]string{"claim"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-to", b.payer,
			"-inclusion", "6", "-secret-file", value, "-record", filepath.Join(b.dir, "r.json")}), "-secret-file"},
		"as the command":            {[]string{value}, "unknown command"},
		"where a flag has no value": {args([]string{"script"}, b.contract()[:9]), "-lock-height"},
	} {
		err := b.run(c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("the secret %s: got %v, want a refusal that names %q", name, err, c.want)
		}
		if (err != nil && strings.Contains(err.Error(), value)) || strings.Contains(b.stderr.String(), value) {
			t.Errorf("the secret %s: the refusal prints it back", name)
		}
		if b.node.calls != 0 {
			t.Fatalf("the secret %s: the node was asked", name)
		}
	}

	if err := b.run("claim", "-h"); !errors.Is(err, flag.ErrHelp) || !strings.Contains(b.stderr.String(), "-secret-file") || !strings.Contains(b.stderr.String(), "-inclusion") {
		t.Errorf("-h: got %v and %q, want flag.ErrHelp and the flags of the command", err, b.stderr.String())
	}
}

// steps runs each step that sends a transaction, under the record rec.
var steps = map[string]func(b *bench, rec string, more ...string) error{
	"fund": func(b *bench, rec string, more ...string) error {
		return b.run(args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin, "-record", rec}, more)...)
	},
	"claim": func(b *bench, rec string, more ...string) error {
		return b.run(args([]string{"claim"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-to", b.payer,
			"-inclusion", "6", "-secret-file", b.file("secret", hex.EncodeToString(b.secret[:]), 0o600), "-record", rec}, more)...)
	},
	"refund": func(b *bench, rec string, more ...string) error {
		b.node.info.Blocks, b.node.info.Headers = benchLock, benchLock
		return b.run(args([]string{"refund"}, b.contract(), b.as(b.payer), []string{"-outpoint", contract, "-to", b.payee, "-record", rec}, more)...)
	},
}

// After a broadcast whose outcome is unknown the error of each step names the
// record, which holds the bytes to send again.
func TestAnUnknownOutcomeNamesTheRecord(t *testing.T) {
	for step, run := range steps {
		b := newBench(t)
		b.node.broadcastErr = rpc.ErrUnknownOutcome
		rec := filepath.Join(b.dir, step+".json")
		err := run(b, rec)
		_, txid, rawHex := b.record(rec)
		if !errors.Is(err, rpc.ErrUnknownOutcome) || !strings.Contains(err.Error(), rec) || !strings.Contains(err.Error(), txid) || rawHex == "" {
			t.Errorf("%s: got %v; want the node's error with the record %s and the transaction %s named", step, err, rec, txid)
		}
		if err != nil && !strings.Contains(err.Error(), "send those bytes again") {
			t.Errorf("%s: the error does not say what to do with the record: %v", step, err)
		}
	}
}

// paid is one output as a raw transaction holds it: the value in eight bytes,
// the length of the script and the script.
func paid(t *testing.T, value int64, addr string) string {
	t.Helper()
	script, err := address.ScriptFor(addr)
	if err != nil {
		t.Fatal(err)
	}
	var le [8]byte
	binary.LittleEndian.PutUint64(le[:], uint64(value))
	return hex.EncodeToString(le[:]) + fmt.Sprintf("%02x", len(script)) + hex.EncodeToString(script)
}

// The values the command hands on, read from the bytes it records. The change
// of a funding is the node's value of the input less the amount and the fee,
// so the input's value is the node's; a claim and a refund pay the output's
// value less the fee at the rate of -fee-rate.
func TestTheRecordedTransactionsCarryTheNodesValueAndTheFeeRateAsked(t *testing.T) {
	id, _, _ := strings.Cut(coin, ":")
	funded, _, _ := strings.Cut(contract, ":")
	for _, rate := range []int64{types.RecommendedFeeRate, 2 * types.RecommendedFeeRate} {
		flags := []string{"-fee-rate", fmt.Sprint(rate)}

		b := newBench(t)
		rec := filepath.Join(b.dir, "fund.json")
		if err := steps["fund"](b, rec, flags...); err != nil {
			t.Fatal(err)
		}
		want, err := htlc.BuildFunding(b.p, []types.UTXO{{TxID: id, Value: 5 * benchAmount, Address: b.payer}}, benchAmount, b.payer, rate)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, rawHex := b.record(rec); !strings.Contains(rawHex, paid(t, want.Outputs[1].Value, b.payer)) {
			t.Errorf("fund at %d shors a virtual byte: the change is not %d shors, the node's value of the input less the amount and the fee", rate, want.Outputs[1].Value)
		}

		b = newBench(t)
		rec = filepath.Join(b.dir, "claim.json")
		if err := steps["claim"](b, rec, flags...); err != nil {
			t.Fatal(err)
		}
		output := types.UTXO{TxID: funded, Vout: 1, Value: benchAmount}
		claim, err := htlc.BuildClaim(b.p, output, b.secret, b.payer, rate, b.manager, b.payee)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, rawHex := b.record(rec); !strings.Contains(rawHex, paid(t, claim.Outputs[0].Value, b.payer)) {
			t.Errorf("claim at %d shors a virtual byte: the claim does not pay %d shors, the output less the fee at that rate", rate, claim.Outputs[0].Value)
		}

		b = newBench(t)
		rec = filepath.Join(b.dir, "refund.json")
		if err := steps["refund"](b, rec, flags...); err != nil {
			t.Fatal(err)
		}
		refund, err := htlc.BuildRefund(b.p, output, b.payee, rate, b.manager, b.payer)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, rawHex := b.record(rec); !strings.Contains(rawHex, paid(t, refund.Outputs[0].Value, b.payee)) {
			t.Errorf("refund at %d shors a virtual byte: the refund does not pay %d shors, the output less the fee at that rate", rate, refund.Outputs[0].Value)
		}
	}
}

// The tip moves into the margin while the claim is signed and recorded. The
// claim is in the record and was not sent, and the error says what that means:
// the record holds the secret and its bytes are not for the node.
func TestClaimHeldBackAfterTheRecordSaysNothingWasSent(t *testing.T) {
	b := newBench(t)
	b.node.later = benchTip + 1 // one block inside the depth and the inclusion
	rec := filepath.Join(b.dir, "claim.json")
	err := b.run(args([]string{"claim"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-to", b.payer,
		"-inclusion", "6", "-secret-file", b.file("secret", hex.EncodeToString(b.secret[:]), 0o600), "-record", rec})...)
	if !errors.Is(err, htlc.ErrLockTooNear) || len(b.node.sent) != 0 {
		t.Fatalf("got %v and %d broadcasts, want htlc.ErrLockTooNear and none", err, len(b.node.sent))
	}
	step, txid, rawHex := b.record(rec)
	if step != "claim" || rawHex == "" {
		t.Fatalf("the record holds step %q and %d bytes, want the claim that was held back", step, len(rawHex)/2)
	}
	text := err.Error()
	for _, want := range []string{"nothing was sent", rec, txid, "holds the secret", "do not send"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error does not hold %q: %v", want, err)
		}
	}
	if strings.Contains(text, "send those bytes again") {
		t.Errorf("the error tells the caller to send the bytes of a claim that was held back: %v", err)
	}
	if strings.Contains(text, hex.EncodeToString(b.secret[:])) {
		t.Error("the error prints the secret")
	}
}

// claim reads the secret from a file its owner alone can read, records the
// claim and sends it. The secret is no flag, and no refusal prints the file.
func TestClaimReadsTheSecretFromAFileOfItsOwner(t *testing.T) {
	b := newBench(t)
	digits := hex.EncodeToString(b.secret[:])
	rec := filepath.Join(b.dir, "claim.json")
	claim := func(secretFile, record string, more ...string) []string {
		return args([]string{"claim"}, b.contract(), b.as(b.payee),
			[]string{"-amount", "1", "-outpoint", contract, "-to", b.payer, "-inclusion", "6", "-secret-file", secretFile, "-record", record}, more)
	}
	if err := b.run(claim(b.file("secret", digits+"\n", 0o600), rec)...); err != nil {
		t.Fatal(err)
	}
	step, txid, rawHex := b.record(rec)
	if step != "claim" || len(b.node.sent) != 1 || b.node.sent[0] != rawHex || b.stdout.String() != "claim "+txid+" sent\n" {
		t.Fatalf("step %q, %d broadcasts, printed %q; want the claim recorded, sent once and named", step, len(b.node.sent), b.stdout.String())
	}
	if !strings.Contains(rawHex, digits) {
		t.Error("the claim sent does not hold the secret of the file")
	}
	if !b.pays(rawHex, b.payer) || b.pays(rawHex, b.payee) {
		t.Error("the claim does not pay the address -to names")
	}

	refusals := map[string]string{
		"a file others can read":          b.file("open", digits, 0o644),
		"a file its group can read":       b.file("group", digits, 0o640),
		"a file its group can write":      b.file("shared", digits, 0o620),
		"63 digits":                       b.file("short", digits[:63], 0o600),
		"66 digits":                       b.file("long", digits+"00", 0o600),
		"a character that is not a digit": b.file("other", "zz"+digits[2:], 0o600),
		"no file":                         filepath.Join(b.dir, "absent"),
		"a directory of its owner alone":  filepath.Join(b.dir, "a-directory"),
	}
	if err := os.Mkdir(refusals["a directory of its owner alone"], 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		delete(refusals, "a file others can read")
		delete(refusals, "a file its group can read")
		delete(refusals, "a file its group can write")
	}
	for name, path := range refusals {
		calls, sent := b.node.calls, len(b.node.sent)
		err := b.run(claim(path, filepath.Join(b.dir, "other.json"))...)
		if err == nil || !strings.Contains(err.Error(), "-secret-file") || b.node.calls != calls || len(b.node.sent) != sent {
			t.Errorf("%s: got %v with %d more calls of the node; want a refusal of -secret-file and no call", name, err, b.node.calls-calls)
		}
		if err != nil && (strings.Contains(err.Error(), digits[2:40]) || strings.Contains(err.Error(), "zz")) {
			t.Errorf("%s: the refusal prints what the file holds: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), path) {
			t.Errorf("%s: the refusal prints the value of -secret-file, which may be the secret: %v", name, err)
		}
	}
	for _, flagName := range []string{"-secret", "-preimage", "-passphrase", "-rpc-password"} {
		if err := b.run(claim(b.file("s", digits, 0o600), filepath.Join(b.dir, "other.json"), flagName, digits)...); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("%s: got %v, want no such flag", flagName, err)
		}
	}
}

func TestAuditRefundAndSecretCallTheLibrary(t *testing.T) {
	b := newBench(t)
	if err := b.run(args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-blocks-to-lock", "36"})...); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%s holds %d shors with 30 confirmations at tip %d, 36 blocks below the lock height\n", contract, benchAmount, benchTip); b.stdout.String() != want {
		t.Errorf("audit printed %q, want %q", b.stdout.String(), want)
	}
	if err := b.run(args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-blocks-to-lock", "37"})...); !errors.Is(err, htlc.ErrLockTooNear) {
		t.Errorf("audit with one block more asked than left: got %v, want htlc.ErrLockTooNear", err)
	}
	if err := b.run(args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1.00000001", "-outpoint", contract})...); !errors.Is(err, htlc.ErrFundedOutput) {
		t.Errorf("audit for one shor more than the output holds: got %v, want htlc.ErrFundedOutput", err)
	}

	if err := b.run(args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-depth", "31"})...); !errors.Is(err, htlc.ErrShallow) {
		t.Errorf("audit at a depth of 31 with 30 confirmations: got %v, want htlc.ErrShallow", err)
	}

	rec := filepath.Join(b.dir, "refund.json")
	refund := args([]string{"refund"}, b.contract(), b.as(b.payer), []string{"-outpoint", contract, "-to", b.payee, "-record", rec})
	if err := b.run(refund...); !errors.Is(err, htlc.ErrBeforeLockHeight) {
		t.Errorf("refund below the lock height: got %v, want htlc.ErrBeforeLockHeight", err)
	}
	if _, err := os.Lstat(rec); err == nil {
		t.Error("a refused refund left a record")
	}
	b.node.info.Blocks, b.node.info.Headers = benchLock, benchLock
	if err := b.run(refund...); err != nil {
		t.Fatal(err)
	}
	if step, txid, rawHex := b.record(rec); step != "refund" || len(b.node.sent) != 1 || b.node.sent[0] != rawHex || b.stdout.String() != "refund "+txid+" sent\n" {
		t.Errorf("step %q, %d broadcasts, printed %q; want the refund recorded, sent once and named", step, len(b.node.sent), b.stdout.String())
	} else if !b.pays(rawHex, b.payee) || b.pays(rawHex, b.payer) {
		t.Error("the refund does not pay the address -to names")
	}

	txid, _, _ := strings.Cut(contract, ":")
	spend := func(spender string, witness ...[]byte) string {
		items := make([]string, len(witness))
		for i, item := range witness {
			items[i] = hex.EncodeToString(item)
		}
		raw, _ := json.Marshal(map[string]any{"tx": []any{map[string]any{"txid": spender,
			"vin": []any{map[string]any{"txid": txid, "vout": 1, "txinwitness": items}}}}})
		return string(raw)
	}
	find := args([]string{"secret"}, b.contract(), []string{"-outpoint", contract, "-from-height", "990"})
	if err := b.run(find...); !errors.Is(err, htlc.ErrNoSpend) {
		t.Errorf("secret with no spend mined: got %v, want htlc.ErrNoSpend", err)
	}
	if len(b.node.read) != benchLock-990+1 || b.node.read[0] != 990 || b.node.read[len(b.node.read)-1] != benchLock {
		t.Errorf("blocks read: %v, want those from -from-height to the tip", b.node.read)
	}
	b.node.blocks = map[int64]string{995: spend("the-claim", []byte{0x01}, b.secret[:])}
	if err := b.run(find...); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("secret    %x\nspent by  the-claim\n", b.secret); b.stdout.String() != want {
		t.Errorf("secret printed %q, want %q", b.stdout.String(), want)
	}
	b.node.blocks[995] = spend("the-refund", []byte{0x01})
	if err := b.run(find...); !errors.Is(err, htlc.ErrNoSecret) || !strings.Contains(err.Error(), "the-refund") {
		t.Errorf("secret after a refund: got %v, want htlc.ErrNoSecret naming the spender", err)
	}
}

// What the command refuses before it opens a keystore or a node, and that no
// refusal prints the value of a contract flag, a lock height outside the
// range of a block height apart.
func TestArgumentsAreRefusedBeforeAnythingIsOpened(t *testing.T) {
	b := newBench(t)
	with := func(name, value string) []string {
		c := b.contract()
		for i := range c {
			if c[i] == name {
				c[i+1] = value
			}
		}
		return c
	}
	notHex := strings.Repeat("zy", 32)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no command":                   {nil, "usage"},
		"an unknown command":           {[]string{"send"}, "unknown command"},
		"an argument after flags":      {args([]string{"script"}, b.contract(), []string{"extra"}), "unexpected argument"},
		"an unknown network":           {args([]string{"script"}, with("-network", "testnet")), "-network"},
		"a hash that is not hex":       {args([]string{"script"}, with("-hash", notHex)), "-hash"},
		"a key hash of 31 bytes":       {args([]string{"script"}, with("-payee-key-hash", strings.Repeat("ab", 31))), "-payee-key-hash"},
		"a lock height of zero":        {args([]string{"script"}, with("-lock-height", "0")), "lock height"},
		"a lock height past uint32":    {args([]string{"script"}, with("-lock-height", "4294967297")), "lock height"}, // its low 32 bits are the lock height 1
		"a depth past the range":       {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-depth", "9223372036854775808", "-outpoint", contract}), "-depth: want a whole number in range"},
		"an amount that is no decimal": {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "one", "-outpoint", contract}), "-amount"},
		"an outpoint without an index": {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", "7a7a"}), "outpoint"},
		"no record for a fund":         {args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin}), "-record: name a new file"},
		"an empty record for a fund":   {args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin, "-record", ""}), "-record: name a new file"},
		"no record for a claim": {args([]string{"claim"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-to", b.payer,
			"-inclusion", "6", "-secret-file", b.file("held-secret", hex.EncodeToString(b.secret[:]), 0o600)}), "-record: name a new file"},
		"no record for a refund": {args([]string{"refund"}, b.contract(), b.as(b.payer), []string{"-outpoint", contract, "-to", b.payee}), "-record: name a new file"},
		"no secret file for a claim": {args([]string{"claim"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-to", b.payer,
			"-inclusion", "6", "-record", filepath.Join(b.dir, "r.json")}), "-secret-file: name the file"},
		"an empty secret file for a claim": {args([]string{"claim"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract, "-to", b.payer,
			"-inclusion", "6", "-secret-file", "", "-record", filepath.Join(b.dir, "r.json")}), "-secret-file: name the file"},
		"no keystore":               {args([]string{"key-hash"}, []string{"-address", b.payee}), "-keystore"},
		"a flag of another command": {args([]string{"script"}, b.contract(), []string{"-record", "r"}), "flag provided but not defined"},
	} {
		err := b.run(c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error that names %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), notHex) {
			t.Errorf("%s: the refusal prints the value of the flag", name)
		}
		if b.node.calls != 0 {
			t.Fatalf("%s: the node was asked", name)
		}
	}

	b.environment["SOQ_KEYSTORE_PASSPHRASE"] = ""
	if err := b.run(args([]string{"key-hash"}, b.as(b.payee))...); err == nil || !strings.Contains(err.Error(), "SOQ_KEYSTORE_PASSPHRASE") {
		t.Errorf("no passphrase in the environment: got %v", err)
	}
	b.environment["SOQ_KEYSTORE_PASSPHRASE"] = "another passphrase"
	if err := b.run(args([]string{"key-hash"}, b.as(b.payee))...); err == nil || !strings.Contains(err.Error(), "keystore") {
		t.Errorf("a wrong passphrase: got %v", err)
	}
	b.environment["SOQ_KEYSTORE_PASSPHRASE"] = passphrase
	b.environment["SOQ_RPC_URL"] = "http://127.0.0.1:1"
	b.environment["SOQ_RPC_USER"], b.environment["SOQ_RPC_PASSWORD"] = "the-user", "the-password"
	_ = b.run(args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract})...)
	if len(b.dialed) != 1 || b.dialed[0] != "regtest http://127.0.0.1:1" {
		t.Errorf("dialed %v, want the address of SOQ_RPC_URL", b.dialed)
	}
	if len(b.credentials) != 1 || b.credentials[0] != "user the-user password the-password" {
		t.Errorf("dialed with %v, want SOQ_RPC_USER as the user and SOQ_RPC_PASSWORD as the password", b.credentials)
	}
}

// Every command passes on the refusal of each thing it reads, and a refused
// step sends nothing and leaves no record.
func TestEveryCommandPassesOnARefusal(t *testing.T) {
	b := newBench(t)
	secretFile := b.file("secret", hex.EncodeToString(b.secret[:]), 0o600)
	held := b.file("held.json", "{}", 0o600)
	fresh := func() string { return filepath.Join(b.dir, "fresh.json") }
	bad := []string{"-network", "regtest", "-hash", "00", "-payee-key-hash", "00", "-payer-key-hash", "00", "-lock-height", "1"}
	terms := []string{"-amount", "1"}
	at := []string{"-outpoint", contract}
	nowhere := []string{"-outpoint", "7a7a"}
	pay := func(record string) []string { return []string{"-to", b.payee, "-record", record} }
	claim := []string{"-secret-file", secretFile, "-inclusion", "6"}
	for name, c := range map[string]struct {
		args []string
		want error
	}{
		"script, a contract that is none":                 {args([]string{"script"}, bad), nil},
		"key-hash, an address the keystore does not hold": {[]string{"key-hash", "-keystore", b.keystore, "-address", "sq1pnone"}, keys.ErrNoKey},
		"fund, a contract that is none":                   {args([]string{"fund"}, bad, b.as(b.payer), terms, []string{"-inputs", coin, "-record", fresh()}), nil},
		"fund, no amount":                                 {args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-inputs", coin, "-record", fresh()}), nil},
		"fund, no keystore":                               {args([]string{"fund"}, b.contract(), terms, []string{"-inputs", coin, "-record", fresh()}), nil},
		"fund, the payee asks":                            {args([]string{"fund"}, b.contract(), b.as(b.payee), terms, []string{"-inputs", coin, "-record", fresh()}), nil},
		"fund, an input that is no outpoint":              {args([]string{"fund"}, b.contract(), b.as(b.payer), terms, []string{"-inputs", "7a7a", "-record", fresh()}), nil},
		"audit, a contract that is none":                  {args([]string{"audit"}, bad, b.as(b.payee), terms, at), nil},
		"audit, no amount":                                {args([]string{"audit"}, b.contract(), b.as(b.payee), at), nil},
		"audit, no keystore":                              {args([]string{"audit"}, b.contract(), terms, at), nil},
		"claim, a contract that is none":                  {args([]string{"claim"}, bad, b.as(b.payee), terms, at, pay(fresh()), claim), nil},
		"claim, no amount":                                {args([]string{"claim"}, b.contract(), b.as(b.payee), at, pay(fresh()), claim), nil},
		"claim, an outpoint that is none":                 {args([]string{"claim"}, b.contract(), b.as(b.payee), terms, nowhere, pay(fresh()), claim), nil},
		"claim, no keystore":                              {args([]string{"claim"}, b.contract(), terms, at, pay(fresh()), claim), nil},
		"claim, a record that exists":                     {args([]string{"claim"}, b.contract(), b.as(b.payee), terms, at, pay(held), claim), nil},
		"claim, the payer asks":                           {args([]string{"claim"}, b.contract(), b.as(b.payer), terms, at, pay(fresh()), claim), htlc.ErrWrongKey},
		"claim, a depth above the confirmations":          {args([]string{"claim"}, b.contract(), b.as(b.payee), terms, at, pay(fresh()), claim, []string{"-depth", "31"}), htlc.ErrShallow},
		"claim, no block for inclusion":                   {args([]string{"claim"}, b.contract(), b.as(b.payee), terms, at, pay(fresh()), []string{"-secret-file", secretFile}), htlc.ErrLockTooNear},
		"refund, a contract that is none":                 {args([]string{"refund"}, bad, b.as(b.payer), at, pay(fresh())), nil},
		"refund, an outpoint that is none":                {args([]string{"refund"}, b.contract(), b.as(b.payer), nowhere, pay(fresh())), nil},
		"refund, no keystore":                             {args([]string{"refund"}, b.contract(), at, pay(fresh())), nil},
		"refund, a record that exists":                    {args([]string{"refund"}, b.contract(), b.as(b.payer), at, pay(held)), nil},
		"refund, the payee asks":                          {args([]string{"refund"}, b.contract(), b.as(b.payee), at, pay(fresh())), htlc.ErrWrongKey},
		"secret, a contract that is none":                 {args([]string{"secret"}, bad, at), nil},
		"secret, an outpoint that is none":                {args([]string{"secret"}, b.contract(), nowhere), nil},
		"secret, an id that is not 64 digits":             {args([]string{"secret"}, b.contract(), []string{"-outpoint", "7a7a:1"}), htlc.ErrOutpoint},
	} {
		err := b.run(c.args...)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v, want a refusal (%v)", name, err, c.want)
		}
		if len(b.node.sent) != 0 {
			t.Fatalf("%s: a refused step sent a transaction", name)
		}
		if _, statErr := os.Lstat(fresh()); statErr == nil {
			t.Fatalf("%s: a refused step left a record", name)
		}
	}
	if data, _ := os.ReadFile(held); string(data) != "{}" {
		t.Errorf("a record that existed was written over: %q", data)
	}
}

// dial returns the SDK's client, held to the network it was asked for.
func TestDialHoldsTheClientToTheNetwork(t *testing.T) {
	c, ok := dial("http://127.0.0.1:1", "user", "password", types.Stagenet).(*rpc.Client)
	if !ok || c.Network.ChainID != types.Stagenet.ChainID {
		t.Errorf("dial returned %T on %q, want *rpc.Client on stagenet", c, c.Network.ChainID)
	}
	if c.AllowRemote {
		t.Error("dial returned a client that takes a node on another machine")
	}
	remote, _ := dial("http://node.example:33389", "user", "password", types.Stagenet).(*rpc.Client)
	if _, err := remote.GetBlockchainInfo(context.Background()); !errors.Is(err, rpc.ErrRemoteNode) {
		t.Errorf("a node on another machine: got %v, want rpc.ErrRemoteNode", err)
	}
}
