// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/htlc"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
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
}

func (r *replies) GetBlockchainInfo(context.Context) (*rpc.BlockchainInfo, error) {
	r.calls++
	info := r.info
	return &info, nil
}

func (r *replies) GetTxOut(_ context.Context, txid string, vout uint32, _ bool) (*rpc.TxOut, error) {
	r.calls++
	out, ok := r.outs[fmt.Sprintf("%s:%d", txid, vout)]
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
	if block, ok := r.blocks[height]; ok {
		return json.RawMessage(block), nil
	}
	return json.RawMessage(`{"tx":[]}`), nil
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
}

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
	e := env{
		getenv: func(name string) string { return b.environment[name] },
		stdout: &b.stdout, stderr: &b.stderr,
		dial: func(url, _, _ string, network types.Network) htlc.Node {
			b.dialed = append(b.dialed, network.Name+" "+url)
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

	calls := b.node.calls
	err := b.run(fund...)
	if err == nil || !strings.Contains(err.Error(), rec) || len(b.node.sent) != 1 || b.node.calls != calls {
		t.Errorf("a second run under the same record: got %v, %d broadcasts and %d more calls of the node; want a refusal that names the record and nothing else", err, len(b.node.sent), b.node.calls-calls)
	}
	if _, again, _ := b.record(rec); again != txid {
		t.Errorf("the record changed from %s to %s", txid, again)
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

// After a broadcast whose outcome is unknown the error names the record,
// which holds the bytes to send again.
func TestAnUnknownOutcomeNamesTheRecord(t *testing.T) {
	b := newBench(t)
	b.node.broadcastErr = rpc.ErrUnknownOutcome
	rec := filepath.Join(b.dir, "fund.json")
	err := b.run(args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin, "-record", rec})...)
	_, txid, rawHex := b.record(rec)
	if !errors.Is(err, rpc.ErrUnknownOutcome) || !strings.Contains(err.Error(), rec) || !strings.Contains(err.Error(), txid) || rawHex == "" {
		t.Errorf("got %v; want the node's error with the record %s and the transaction %s named", err, rec, txid)
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
			[]string{"-amount", "1", "-outpoint", contract, "-to", b.payee, "-inclusion", "6", "-secret-file", secretFile, "-record", record}, more)
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

	refusals := map[string]string{
		"a file others can read":          b.file("open", digits, 0o644),
		"63 digits":                       b.file("short", digits[:63], 0o600),
		"66 digits":                       b.file("long", digits+"00", 0o600),
		"a character that is not a digit": b.file("other", "zz"+digits[2:], 0o600),
		"no file":                         filepath.Join(b.dir, "absent"),
	}
	if runtime.GOOS == "windows" {
		delete(refusals, "a file others can read")
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

	rec := filepath.Join(b.dir, "refund.json")
	refund := args([]string{"refund"}, b.contract(), b.as(b.payer), []string{"-outpoint", contract, "-to", b.payer, "-record", rec})
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
// refusal prints the value of a contract flag.
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
		"a lock height past uint32":    {args([]string{"script"}, with("-lock-height", "4294967296")), "lock height"},
		"an amount that is no decimal": {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "one", "-outpoint", contract}), "-amount"},
		"an outpoint without an index": {args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", "7a7a"}), "outpoint"},
		"no record for a fund":         {args([]string{"fund"}, b.contract(), b.as(b.payer), []string{"-amount", "1", "-inputs", coin}), "-record"},
		"no keystore":                  {args([]string{"key-hash"}, []string{"-address", b.payee}), "-keystore"},
		"a flag of another command":    {args([]string{"script"}, b.contract(), []string{"-record", "r"}), "flag provided but not defined"},
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
	_ = b.run(args([]string{"audit"}, b.contract(), b.as(b.payee), []string{"-amount", "1", "-outpoint", contract})...)
	if len(b.dialed) != 1 || b.dialed[0] != "regtest http://127.0.0.1:1" {
		t.Errorf("dialed %v, want the address of SOQ_RPC_URL", b.dialed)
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
}
