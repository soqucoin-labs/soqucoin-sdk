// Copyright (c) 2026 Soqucoin Labs Inc.
// Distributed under the MIT software license, see LICENSE.

// Command soq-htlc runs the Soqucoin side of a cross-chain swap from a
// terminal, one step a call, against your own node:
//
//	soq-htlc key-hash -keystore F -address A
//	soq-htlc script   CONTRACT
//	soq-htlc fund     CONTRACT PARTY -amount SOQ -inputs txid:vout,... -record FILE
//	soq-htlc audit    CONTRACT PARTY -amount SOQ -outpoint txid:vout
//	soq-htlc claim    CONTRACT PARTY -amount SOQ -outpoint txid:vout -secret-file FILE -to A -inclusion N -record FILE
//	soq-htlc refund   CONTRACT PARTY -outpoint txid:vout -to A -record FILE
//	soq-htlc secret   CONTRACT -outpoint txid:vout -from-height H
//
// CONTRACT is -network, -hash, -payee-key-hash, -payer-key-hash and
// -lock-height, the values both parties agreed. PARTY is -keystore and
// -address, the caller's own key. Run a command with -h for the rest.
//
// Each step is one function of package htlc, which makes every check of the
// contract and refuses before anything is signed. The command checks what it
// handles itself: the inputs of a funding (an outpoint named twice, spent,
// unconfirmed, paying another script, or a coinbase output below maturity),
// the mode of the secret file, and a record file that exists. It reads the
// flags, opens the keystore and the node, and writes the record. No flag
// skips a check.
//
// The keystore passphrase is read from SOQ_KEYSTORE_PASSPHRASE and the node
// from SOQ_RPC_URL (default: the network's port on 127.0.0.1), SOQ_RPC_USER
// and SOQ_RPC_PASSWORD. The secret is read from a file that its owner alone
// can read. None of them is a flag, so none is in the process list or the
// shell's history.
//
// fund, claim and refund write the signed transaction to the -record file
// before the node receives it, and refuse a record that exists. After an
// error that leaves the outcome unknown, send the recorded bytes again with
// the node's sendrawtransaction; running the step again under another record
// builds a second transaction, and a second funding locks a second amount
// until the lock height. The record of a claim holds the secret. A claim that
// ends with "nothing was sent" was held back because the tip moved too near
// the lock height while it was signed and recorded: its bytes are never sent.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/htlc"
	"github.com/soqucoin-labs/soqucoin-sdk/internal/atomicfile"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/tx"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

const usage = `usage: soq-htlc <command> [flags]

  key-hash  print the hash of the caller's own key, as a contract names it
  script    print the contract's witnessScript, output script and address
  fund      fund the contract (the payer)
  audit     check the funded output against the terms (either party)
  claim     take the output with the secret (the payee)
  refund    take the output back from the lock height on (the payer)
  secret    read the secret from the mined spend of the output

Run a command with -h for its flags`

// env is what the command reads from outside its arguments.
type env struct {
	getenv func(string) string
	stdout io.Writer
	stderr io.Writer
	dial   func(url, user, password string, network types.Network) htlc.Node
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	e := env{getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr, dial: dial}
	if err := e.run(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "soq-htlc:", err)
		}
		os.Exit(1)
	}
}

// dial returns the client of a node on this machine, held to the network.
func dial(url, user, password string, network types.Network) htlc.Node {
	c := rpc.NewClient(url, user, password, nil)
	c.Network = network
	return c
}

// options are the values of the flags.
type options struct {
	network, hash, payee, payer, keystore, address string
	amount, outpoint, inputs, change, to, record   string
	secretFile                                     string
	lockHeight                                     uint64
	depth, feeRate, lockMargin, blocksToLock       int64
	inclusion, fromHeight                          int64
}

// The flags by group, as the commands share them.
var (
	contractFlags = []string{"network", "hash", "payee-key-hash", "payer-key-hash", "lock-height"}
	partyFlags    = []string{"keystore", "address"}
	termsFlags    = []string{"amount", "depth"}
	spendFlags    = []string{"outpoint", "to", "fee-rate", "record"}
)

// register declares the named flags on fs.
func (o *options) register(fs *flag.FlagSet, groups ...[]string) {
	for _, group := range groups {
		for _, name := range group {
			switch name {
			case "network":
				fs.StringVar(&o.network, name, "", "mainnet, stagenet or regtest")
			case "hash":
				fs.StringVar(&o.hash, name, "", "SHA-256 of the secret, 64 hexadecimal digits")
			case "payee-key-hash":
				fs.StringVar(&o.payee, name, "", "key hash of the party that claims, 64 hexadecimal digits")
			case "payer-key-hash":
				fs.StringVar(&o.payer, name, "", "key hash of the party that funds and refunds, 64 hexadecimal digits")
			case "lock-height":
				fs.Uint64Var(&o.lockHeight, name, 0, "height from which the refund is valid")
			case "keystore":
				fs.StringVar(&o.keystore, name, "", "the caller's keystore file")
			case "address":
				fs.StringVar(&o.address, name, "", "payment address of the caller's own key in the keystore")
			case "amount":
				fs.StringVar(&o.amount, name, "", "value of the contract's output in SOQ, a decimal")
			case "depth":
				fs.Int64Var(&o.depth, name, 0, "confirmations to wait for; 0 is the floor for the amount, and less is refused")
			case "outpoint":
				fs.StringVar(&o.outpoint, name, "", "the contract's output, txid:vout")
			case "to":
				fs.StringVar(&o.to, name, "", "payment address the spend pays")
			case "fee-rate":
				fs.Int64Var(&o.feeRate, name, types.RecommendedFeeRate, "fee rate in shors per virtual byte")
			case "record":
				fs.StringVar(&o.record, name, "", "new file that receives the signed transaction before the node does")
			case "inputs":
				fs.StringVar(&o.inputs, name, "", "confirmed outputs of -address to fund from, txid:vout separated by commas")
			case "change":
				fs.StringVar(&o.change, name, "", "address for the change (default: -address)")
			case "lock-margin":
				fs.Int64Var(&o.lockMargin, name, 0, "least number of blocks between the tip and the lock height")
			case "blocks-to-lock":
				fs.Int64Var(&o.blocksToLock, name, 0, "least number of blocks the tip must leave before the lock height")
			case "secret-file":
				fs.StringVar(&o.secretFile, name, "", "file, readable by its owner alone, with the secret as 64 hexadecimal digits")
			case "inclusion":
				fs.Int64Var(&o.inclusion, name, 0, "blocks allowed for the claim to get into a block, 1 or more")
			case "from-height":
				fs.Int64Var(&o.fromHeight, name, 0, "first block to read, the funding's or below")
			}
		}
	}
}

// run parses the arguments of one command and runs it.
func (e env) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	var (
		o    options
		step func(context.Context, *options) error
		fs   = flag.NewFlagSet("soq-htlc "+args[0], flag.ContinueOnError)
	)
	fs.SetOutput(io.Discard) // the parser prints the value it refuses
	switch args[0] {
	case "key-hash":
		o.register(fs, partyFlags)
		step = e.keyHash
	case "script":
		o.register(fs, contractFlags)
		step = e.script
	case "fund":
		o.register(fs, contractFlags, partyFlags, termsFlags, []string{"inputs", "change", "fee-rate", "lock-margin", "record"})
		step = e.fund
	case "audit":
		o.register(fs, contractFlags, partyFlags, termsFlags, []string{"outpoint", "blocks-to-lock"})
		step = e.audit
	case "claim":
		o.register(fs, contractFlags, partyFlags, termsFlags, spendFlags, []string{"secret-file", "inclusion"})
		step = e.claim
	case "refund":
		o.register(fs, contractFlags, partyFlags, spendFlags)
		step = e.refund
	case "secret":
		o.register(fs, contractFlags, []string{"outpoint", "from-height"})
		step = e.secret
	default:
		return fmt.Errorf("unknown command\n%s", usage)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(e.stderr)
			_, _ = fmt.Fprintf(e.stderr, "flags of %s:\n", fs.Name())
			fs.PrintDefaults()
			return err
		}
		return flagError(fs, err)
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected argument after the flags")
	}
	return step(ctx, &o)
}

// flagError names the flag the parser refused and never its value: one
// passed to the wrong flag may be the secret. The same holds for the
// refusals of contract, terms, parseOutpoint and readSecret, except a lock
// height outside the range of a block height, which is printed. The refusals
// of -keystore and of key-hash -address print their value.
func flagError(fs *flag.FlagSet, err error) error {
	named := errors.New("flag provided but not defined, or malformed; run the command with -h")
	msg := err.Error()
	fs.VisitAll(func(f *flag.Flag) {
		refused := " for flag -" + f.Name + ": "
		switch {
		case msg == "flag needs an argument: -"+f.Name:
			named = fmt.Errorf("-%s: needs a value", f.Name)
		case strings.HasSuffix(msg, refused+"parse error"), strings.HasSuffix(msg, refused+"value out of range"):
			named = fmt.Errorf("-%s: want a whole number in range", f.Name)
		}
	})
	return named
}

// contract returns the network and the contract's parameters as the flags
// name them. No error holds the value of -network, -hash or a key hash: a
// secret passed as -hash by mistake is not printed back (flagError). A lock
// height outside the range of a block height is printed by Params.Validate.
func (o *options) contract() (types.Network, htlc.Params, error) {
	var (
		network types.Network
		p       htlc.Params
	)
	for _, n := range []types.Network{types.Mainnet, types.Stagenet, types.Regtest} {
		if n.Name == o.network {
			network = n
		}
	}
	if network.Name == "" {
		return network, p, errors.New("-network: want mainnet, stagenet or regtest")
	}
	for _, f := range []struct {
		name, value string
		into        *[32]byte
	}{{"hash", o.hash, &p.Hash}, {"payee-key-hash", o.payee, &p.PayeeKeyHash}, {"payer-key-hash", o.payer, &p.PayerKeyHash}} {
		b, err := hex.DecodeString(f.value)
		if err != nil || len(b) != len(f.into) {
			return network, p, fmt.Errorf("-%s: want 64 hexadecimal digits", f.name)
		}
		copy(f.into[:], b)
	}
	if o.lockHeight > math.MaxUint32 {
		return network, p, fmt.Errorf("-lock-height: %w", htlc.ErrLockHeight)
	}
	p.LockHeight = htlc.Height(o.lockHeight)
	return network, p, p.Validate()
}

// terms returns the contract with the amount and the depth of the flags.
func (o *options) terms(p htlc.Params) (htlc.Terms, error) {
	amount, err := types.ParseSOQ(o.amount)
	if err != nil {
		return htlc.Terms{}, fmt.Errorf("-amount: want a decimal number of SOQ: %w", types.ErrAmountFormat)
	}
	return htlc.Terms{Params: p, Amount: amount, Depth: o.depth}, nil
}

// party opens the keystore and returns the caller as a party on network.
func (e env) party(o *options, network types.Network) (htlc.Party, error) {
	passphrase := e.getenv("SOQ_KEYSTORE_PASSPHRASE")
	if o.keystore == "" || o.address == "" || passphrase == "" {
		return htlc.Party{}, errors.New("the caller's key needs -keystore, -address and SOQ_KEYSTORE_PASSPHRASE")
	}
	m := keys.NewManager(o.keystore, passphrase)
	if err := m.Load(); err != nil {
		return htlc.Party{}, fmt.Errorf("keystore: %w", err)
	}
	return htlc.Party{Network: network, Signer: m, Address: o.address}, nil
}

// node returns the node the environment names for network.
func (e env) node(network types.Network) htlc.Node {
	url := e.getenv("SOQ_RPC_URL")
	if url == "" {
		url = fmt.Sprintf("http://127.0.0.1:%d", network.RPCPort)
	}
	return e.dial(url, e.getenv("SOQ_RPC_USER"), e.getenv("SOQ_RPC_PASSWORD"), network)
}

// parseOutpoint reads txid:vout, the value of the flag name.
func parseOutpoint(name, s string) (string, uint32, error) {
	txid, index, ok := strings.Cut(s, ":")
	vout, err := strconv.ParseUint(index, 10, 32)
	if !ok || err != nil {
		return "", 0, fmt.Errorf("-%s: want txid:vout", name)
	}
	return txid, uint32(vout), nil
}

// recorder returns the record of one step: a new file at path, readable by
// its owner alone, that holds the step, the transaction id and the raw bytes.
// A path that exists is refused here, before the keystore signs anything, and
// again when the file is made: the file of an earlier run may be the one copy
// of a transaction the node already has.
func recorder(path, step string) (htlc.Record, error) {
	if path == "" {
		return nil, errors.New("-record: name a new file for the signed transaction")
	}
	path = filepath.Clean(path) // one spelling for the look, the create and the write
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("-record: %s exists; a transaction was already built under it", path)
	}
	return func(txid, rawHex string) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		data, err := json.Marshal(struct {
			Step   string `json:"step"`
			TxID   string `json:"txid"`
			RawHex string `json:"raw_hex"`
		}{step, txid, rawHex})
		if err != nil {
			return err
		}
		return atomicfile.WriteFile(path, append(data, '\n'), 0o600)
	}, nil
}

// recorded adds, to an error that came after a transaction was recorded,
// where its bytes are and what to do with them. htlc.ErrLockTooNear beside a
// transaction is the second read of htlc.Claim: the claim was not sent, and
// its bytes show the secret at a tip from which the refund can win.
func recorded(err error, t *tx.Transaction, path string) error {
	if t == nil || errors.Is(err, htlc.ErrRecord) {
		return err
	}
	if errors.Is(err, htlc.ErrLockTooNear) {
		return fmt.Errorf("%w\nnothing was sent. transaction %s is in %s, which holds the secret: do not send those bytes, now or later. Run the step again under a new record; it refuses before it signs once the margin is gone", err, t.TxID(), path)
	}
	return fmt.Errorf("%w\ntransaction %s is in %s: if the node may hold it, send those bytes again and build no second one", err, t.TxID(), path)
}

// result prints the line of a step whose transaction the node took. A line
// that cannot be written is no refusal of the step, and the error says so:
// the transaction was sent, and the record holds it.
func (e env) result(t *tx.Transaction, path, format string, values ...any) error {
	if _, err := fmt.Fprintf(e.stdout, format, values...); err != nil {
		return fmt.Errorf("the node took transaction %s, recorded in %s, and the result line could not be written: %w; build no second one", t.TxID(), path, err)
	}
	return nil
}

func (e env) keyHash(_ context.Context, o *options) error {
	party, err := e.party(o, types.Network{})
	if err != nil {
		return err
	}
	publicKey, err := party.Signer.PublicKeyFor(o.address)
	if err != nil {
		return err
	}
	hash, err := htlc.KeyHash(publicKey)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "%x\n", hash)
	return err
}

func (e env) script(_ context.Context, o *options) error {
	network, p, err := o.contract()
	if err != nil {
		return err
	}
	script, err := p.Script()
	if err != nil {
		return err
	}
	output, err := p.OutputScript()
	if err != nil {
		return err
	}
	addr, err := p.Address(network.HRP)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "witness script  %x\noutput script   %x\naddress         %s\n", script, output, addr)
	return err
}

// ownInputs reads each outpoint of the list from the node and returns it as
// an input of the caller's address. An outpoint that is named twice, is
// spent, unconfirmed or pays another script, or is a coinbase output below
// the network's maturity is refused. The node would refuse a transaction
// that spends any of them but the unconfirmed one, after it was signed and
// recorded; a funding that spends an unconfirmed output is undone with it.
func ownInputs(ctx context.Context, node htlc.Node, network types.Network, addr, list string) ([]types.UTXO, error) {
	script, err := address.ScriptFor(addr)
	if err != nil {
		return nil, err
	}
	maturity := network.CoinbaseMaturity
	if maturity <= 0 {
		maturity = types.Mainnet.CoinbaseMaturity
	}
	var inputs []types.UTXO
	named := map[string]bool{}
	for _, item := range strings.Split(list, ",") {
		txid, vout, err := parseOutpoint("inputs", strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%d", strings.ToLower(txid), vout)
		if named[key] {
			return nil, fmt.Errorf("-inputs: %s is named twice", key)
		}
		named[key] = true
		out, err := node.GetTxOut(ctx, txid, vout, true)
		if err != nil {
			return nil, err
		}
		if out == nil || out.Confirmations < 1 || out.ScriptPubKey.Hex != hex.EncodeToString(script) {
			return nil, fmt.Errorf("-inputs: %s is not a confirmed unspent output of %s", item, addr)
		}
		if out.Coinbase && out.Confirmations < maturity {
			return nil, fmt.Errorf("-inputs: %s is a coinbase output with %d confirmations, spendable at %d", item, out.Confirmations, maturity)
		}
		inputs = append(inputs, types.UTXO{TxID: txid, Vout: vout, Value: out.Value, Address: addr})
	}
	return inputs, nil
}

func (e env) fund(ctx context.Context, o *options) error {
	network, p, err := o.contract()
	if err != nil {
		return err
	}
	terms, err := o.terms(p)
	if err != nil {
		return err
	}
	party, err := e.party(o, network)
	if err != nil {
		return err
	}
	record, err := recorder(o.record, "fund")
	if err != nil {
		return err
	}
	node := e.node(network)
	inputs, err := ownInputs(ctx, node, network, o.address, o.inputs)
	if err != nil {
		return err
	}
	change := o.change
	if change == "" {
		change = o.address
	}
	funded, t, err := htlc.Fund(ctx, node, party, terms,
		htlc.Funding{Inputs: inputs, ChangeAddress: change, FeeRate: o.feeRate, LockMargin: o.lockMargin}, record)
	if err != nil {
		return recorded(err, t, o.record)
	}
	return e.result(t, o.record, "funded %s:%d with %d shors\n", funded.TxID, funded.Vout, funded.Value)
}

func (e env) audit(ctx context.Context, o *options) error {
	network, p, err := o.contract()
	if err != nil {
		return err
	}
	terms, err := o.terms(p)
	if err != nil {
		return err
	}
	txid, vout, err := parseOutpoint("outpoint", o.outpoint)
	if err != nil {
		return err
	}
	party, err := e.party(o, network)
	if err != nil {
		return err
	}
	funded, err := htlc.Audit(ctx, e.node(network), party, terms, txid, vout, o.blocksToLock)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "%s:%d holds %d shors with %d confirmations at tip %d, %d blocks below the lock height\n",
		txid, vout, funded.Output.Value, funded.Confirmations, funded.Tip, int64(p.LockHeight)-funded.Tip)
	return err
}

// readSecret reads the secret, 64 hexadecimal digits, from a file that its
// owner alone can read. No error holds a byte of the file, and none holds
// the path: the secret itself, passed in place of its file's name, is a path
// that does not exist.
func readSecret(path string) ([32]byte, error) {
	var secret [32]byte
	path = filepath.Clean(path) // one spelling for the look and the read
	info, err := os.Stat(path)
	if err != nil {
		return secret, fmt.Errorf("-secret-file: %w", withoutPath(err))
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 && runtime.GOOS != "windows" {
		return secret, fmt.Errorf("-secret-file: the file has mode %04o, open to others; want 0600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return secret, fmt.Errorf("-secret-file: %w", withoutPath(err))
	}
	digits := bytes.TrimSpace(data)
	if len(digits) != hex.EncodedLen(len(secret)) {
		return secret, errors.New("-secret-file: want 64 hexadecimal digits")
	}
	if _, err := hex.Decode(secret[:], digits); err != nil {
		return [32]byte{}, errors.New("-secret-file: want 64 hexadecimal digits")
	}
	return secret, nil
}

// withoutPath returns the cause of a file error without the path it names.
func withoutPath(err error) error {
	var named *fs.PathError
	if errors.As(err, &named) {
		return named.Err
	}
	return err
}

func (e env) claim(ctx context.Context, o *options) error {
	network, p, err := o.contract()
	if err != nil {
		return err
	}
	terms, err := o.terms(p)
	if err != nil {
		return err
	}
	txid, vout, err := parseOutpoint("outpoint", o.outpoint)
	if err != nil {
		return err
	}
	secret, err := readSecret(o.secretFile)
	if err != nil {
		return err
	}
	party, err := e.party(o, network)
	if err != nil {
		return err
	}
	record, err := recorder(o.record, "claim")
	if err != nil {
		return err
	}
	t, err := htlc.Claim(ctx, e.node(network), party, terms, txid, vout, secret,
		htlc.Spend{PayToAddress: o.to, FeeRate: o.feeRate}, o.inclusion, record)
	if err != nil {
		return recorded(err, t, o.record)
	}
	return e.result(t, o.record, "claim %s sent\n", t.TxID())
}

func (e env) refund(ctx context.Context, o *options) error {
	network, p, err := o.contract()
	if err != nil {
		return err
	}
	txid, vout, err := parseOutpoint("outpoint", o.outpoint)
	if err != nil {
		return err
	}
	party, err := e.party(o, network)
	if err != nil {
		return err
	}
	record, err := recorder(o.record, "refund")
	if err != nil {
		return err
	}
	t, err := htlc.Refund(ctx, e.node(network), party, p, txid, vout,
		htlc.Spend{PayToAddress: o.to, FeeRate: o.feeRate}, record)
	if err != nil {
		return recorded(err, t, o.record)
	}
	return e.result(t, o.record, "refund %s sent\n", t.TxID())
}

func (e env) secret(ctx context.Context, o *options) error {
	network, p, err := o.contract()
	if err != nil {
		return err
	}
	txid, vout, err := parseOutpoint("outpoint", o.outpoint)
	if err != nil {
		return err
	}
	secret, spender, err := htlc.FindSecret(ctx, e.node(network), p, txid, vout, o.fromHeight)
	if err != nil {
		if spender != "" {
			return fmt.Errorf("%w (the output is spent by %s)", err, spender)
		}
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "secret    %x\nspent by  %s\n", secret, spender)
	return err
}
