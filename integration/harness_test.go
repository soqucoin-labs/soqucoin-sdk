//go:build integration

// Package integration is the self-serve harness an integrator runs before
// writing a line of their own code:
//
//	make integration                      # needs a soqucoind + soqucoin-cli build
//	go test -tags integration ./integration -soqucoind /path/to/soqucoind
//
// It starts a throwaway regtest node (wallet enabled, which is fine on a
// developer machine and never on a fleet VPS), mines to SDK-generated
// addresses, and drives the real deposit and withdraw packages against the
// real node: deposit credit with node cross-check, a withdrawal built, signed,
// broadcast and confirmed, a lost broadcast reply survived without a second
// transaction, two withdrawals that cannot share an input, refused inputs, and
// a reorganisation that removes a credited deposit. The indexer role is played
// by a small in-test block scanner so the harness needs no ElectrumX; the
// ElectrumX fork is exercised by the electrumx package's protocol tests.
package integration

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

var (
	soqucoind = flag.String("soqucoind", os.Getenv("SOQUCOIND"), "path to the soqucoind binary (regtest)")
)

// node is a managed regtest soqucoind.
type node struct {
	t    *testing.T
	dir  string
	cmd  *exec.Cmd
	rpc  *rpc.Client
	port int
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startNode(t *testing.T) *node {
	t.Helper()
	if *soqucoind == "" {
		t.Skip("set -soqucoind or SOQUCOIND to a regtest-capable soqucoind to run the integration harness")
	}
	dir := t.TempDir()
	p2p, rpcPort := freePort(t), freePort(t)
	args := []string{
		"-regtest", "-server", "-listen=0", "-dnsseed=0", "-upnp=0", "-discover=0",
		"-disablewallet=0", "-enablemining=1", "-txindex=1", "-printtoconsole=0",
		"-rpcuser=it", "-rpcpassword=it", "-rpcbind=127.0.0.1", "-rpcallowip=127.0.0.1",
		"-maxreorgdepth=" + strconv.FormatInt(types.MaxReorgDepth, 10), // mirror mainnet's horizon
		"-port=" + strconv.Itoa(p2p), "-rpcport=" + strconv.Itoa(rpcPort), "-datadir=" + dir, "-daemon=0",
	}
	cmd := exec.Command(*soqucoind, args...)
	logf, err := os.Create(filepath.Join(dir, "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start soqucoind: %v", err)
	}
	n := &node{t: t, dir: dir, cmd: cmd, port: rpcPort,
		rpc: rpc.NewClient(fmt.Sprintf("http://127.0.0.1:%d", rpcPort), "it", "it")}
	t.Cleanup(n.stop)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := n.rpc.GetBlockCount(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node did not come up; see %s", logf.Name())
		}
		time.Sleep(250 * time.Millisecond)
	}
	if g, _ := n.rpc.GetBlockHash(0); g != types.Regtest.GenesisHash {
		t.Fatalf("node genesis %s is not the regtest genesis %s", g, types.Regtest.GenesisHash)
	}
	return n
}

func (n *node) stop() {
	n.rpc.Call("stop")
	done := make(chan struct{})
	go func() { n.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		n.cmd.Process.Kill()
	}
}

func (n *node) mine(to string, blocks int) []string {
	n.t.Helper()
	raw, err := n.rpc.Call("generatetoaddress", blocks, to)
	if err != nil {
		n.t.Fatalf("generatetoaddress: %v", err)
	}
	var hashes []string
	json.Unmarshal(raw, &hashes)
	return hashes
}

func (n *node) height() int64 {
	h, err := n.rpc.GetBlockCount()
	if err != nil {
		n.t.Fatal(err)
	}
	return h
}

// scanner is the harness's stand-in for ElectrumX: it walks blocks and keeps
// the UTXO set of the tracked scripts. It satisfies deposit.Cache and
// resilience.UTXOSource, and feeds the coin selector.
type scanner struct {
	mu       sync.Mutex
	node     *node
	tracked  map[string]string // scriptPubKey hex -> address
	utxos    map[string]types.UTXO
	scanned  int64
	lastAt   time.Time
	lastErr  error
	chainTip map[int64]string // height -> hash, to notice reorgs
}

func newScanner(n *node, addrs ...string) *scanner {
	s := &scanner{node: n, tracked: map[string]string{}, utxos: map[string]types.UTXO{}, chainTip: map[int64]string{}}
	for _, a := range addrs {
		spk, err := address.ScriptFor(a)
		if err != nil {
			n.t.Fatal(err)
		}
		s.tracked[hex.EncodeToString(spk)] = a
	}
	return s
}

func okey(txid string, vout uint32) string { return fmt.Sprintf("%s:%d", txid, vout) }

// RefreshAll rescans from the first block whose hash changed (a reorg) or from
// where it left off.
func (s *scanner) RefreshAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tip, err := s.node.rpc.GetBlockCount()
	if err != nil {
		s.lastErr = err
		return err
	}
	// Detect a reorg: walk back until a scanned hash still matches.
	from := s.scanned + 1
	for h := s.scanned; h >= 1; h-- {
		hash, err := s.node.rpc.GetBlockHash(h)
		if err != nil {
			s.lastErr = err
			return err
		}
		if s.chainTip[h] == hash {
			break
		}
		from = h
	}
	if from <= s.scanned {
		// Rebuild from scratch below the fork point: simplest correct behaviour.
		s.utxos = map[string]types.UTXO{}
		s.chainTip = map[int64]string{}
		from = 1
	}
	for h := from; h <= tip; h++ {
		hash, err := s.node.rpc.GetBlockHash(h)
		if err != nil {
			s.lastErr = err
			return err
		}
		raw, err := s.node.rpc.GetBlock(hash, 2)
		if err != nil {
			s.lastErr = err
			return err
		}
		var blk struct {
			Tx []struct {
				TxID string `json:"txid"`
				Vin  []struct {
					TxID string `json:"txid"`
					Vout uint32 `json:"vout"`
				} `json:"vin"`
				Vout []struct {
					Value        float64 `json:"value"`
					N            uint32  `json:"n"`
					ScriptPubKey struct {
						Hex string `json:"hex"`
					} `json:"scriptPubKey"`
				} `json:"vout"`
			} `json:"tx"`
		}
		if err := json.Unmarshal(raw, &blk); err != nil {
			s.lastErr = err
			return err
		}
		for _, tx := range blk.Tx {
			for _, in := range tx.Vin {
				delete(s.utxos, okey(in.TxID, in.Vout))
			}
			for _, out := range tx.Vout {
				if addr, ok := s.tracked[out.ScriptPubKey.Hex]; ok {
					s.utxos[okey(tx.TxID, out.N)] = types.UTXO{TxID: tx.TxID, Vout: out.N,
						Value: (&rpc.TxOut{Value: out.Value}).ValueShors(), Height: h, Address: addr}
				}
			}
		}
		s.chainTip[h] = hash
		s.scanned = h
	}
	s.lastAt, s.lastErr = time.Now(), nil
	return nil
}

func (s *scanner) LastRefresh() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAt, s.lastErr
}

func (s *scanner) GetUTXOs(addr string) []types.UTXO {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.UTXO
	for _, u := range s.utxos {
		if u.Address == addr {
			out = append(out, u)
		}
	}
	return out
}

func (s *scanner) GetAllUTXOs() []types.UTXO {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.UTXO, 0, len(s.utxos))
	for _, u := range s.utxos {
		out = append(out, u)
	}
	return out
}

// newKey returns a manager holding one fresh regtest key and its address.
func newKey(t *testing.T) (*keys.Manager, string) {
	t.Helper()
	m := keys.NewManager(filepath.Join(t.TempDir(), "keys.enc"), "harness")
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	kp, err := keys.GenerateKeyForNetwork(types.Regtest.HRP)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ImportPrivateKey(kp.PrivateKey, kp.PublicKey, kp.Address); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	return m, kp.Address
}
