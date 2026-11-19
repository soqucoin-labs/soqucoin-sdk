//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/soqucoin-labs/soqucoin-sdk/keys"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// The command a pool copies, examples/pool_payout, built and run as a process
// against the node and the indexer. A dry run sends and writes nothing. A run
// of five payees in batches of two is three transactions. The same command
// line again sends nothing new, and each payee holds one output. The same run
// id over a changed file is refused and sends nothing.
func TestPoolPayoutCommandPaysARunOnce(t *testing.T) {
	n := startNode(t)
	ctx := context.Background()
	dir := t.TempDir()

	keystore := filepath.Join(dir, "keystore.enc")
	m := keys.NewManager(keystore, "harness")
	if err := m.LoadOrCreate(); err != nil {
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
	pool := kp.Address
	_, other := newKey(t)
	n.mine(pool, 6) // one block reward for each batch, and spares
	n.mine(other, int(types.Regtest.CoinbaseMaturity)+5)

	scan := newScanner(n, pool)
	if err := scan.RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	idx := newFakeIndexer(t, scan, pool)

	payees := batchPayees(t, 5)
	writeRun := func(name string, ps []withdraw.Payee) string {
		t.Helper()
		data, err := json.Marshal(ps)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	runFile := writeRun("run.json", payees)

	bin := filepath.Join(dir, "pool_payout")
	build := exec.Command("go", "build", "-o", bin, "./examples/pool_payout")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	runID := "run-1"
	command := func(file string, extra ...string) (string, error) {
		t.Helper()
		args := append([]string{
			"-rpc-url", "http://127.0.0.1:" + strconv.Itoa(n.port), "-rpc-user", "it",
			"-electrumx", idx.addr(), "-keystore", keystore, "-pool-address", pool,
			"-state-dir", state, "-payouts", file, "-run-id", runID, "-batch-size", "2",
			"-min-confirmations", strconv.FormatInt(types.Regtest.CoinbaseMaturity, 10), "-regtest",
		}, extra...)
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "SOQ_KEYSTORE_PASSPHRASE=harness", "SOQ_RPC_PASSWORD=it")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mempool := func() []string {
		t.Helper()
		raw, err := n.rpc.Call(ctx, "getrawmempool")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	requireOneOutputEach := func(when string) {
		t.Helper()
		addrs := make([]string, len(payees))
		for i, p := range payees {
			addrs[i] = p.Address
		}
		rs := newScanner(n, addrs...)
		if err := rs.RefreshAll(ctx); err != nil {
			t.Fatal(err)
		}
		for _, p := range payees {
			got := rs.GetUTXOs(p.Address)
			if len(got) != 1 || got[0].Value != p.Amount {
				t.Fatalf("%s: payee %s holds %+v, want one output of %d", when, p.Address, got, p.Amount)
			}
		}
	}

	out, err := command(runFile, "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if got := strings.Count(out, "would broadcast"); got != 3 {
		t.Fatalf("the dry run reported %d batches, want 3\n%s", got, out)
	}
	if left, _ := os.ReadDir(state); len(left) != 0 || len(mempool()) != 0 {
		t.Fatalf("the dry run wrote %d files and left %d transactions in the mempool", len(left), len(mempool()))
	}

	out, err = command(runFile)
	if err != nil {
		t.Fatalf("the run: %v\n%s", err, out)
	}
	if got := len(mempool()); got != 3 {
		t.Fatalf("the run left %d transactions in the mempool, want 3\n%s", got, out)
	}
	n.mine(other, 1)
	idx.advance()
	requireOneOutputEach("after the run")

	out, err = command(runFile)
	if err != nil {
		t.Fatalf("the same command again: %v\n%s", err, out)
	}
	if got := len(mempool()); got != 0 {
		t.Fatalf("the second run sent %d transactions\n%s", got, out)
	}
	n.mine(other, 1)
	idx.advance()
	requireOneOutputEach("after the second run")

	changed := append([]withdraw.Payee(nil), payees...)
	changed[3].Amount++
	out, err = command(writeRun("changed.json", changed))
	if err == nil || !strings.Contains(out, withdraw.ErrConflict.Error()) {
		t.Fatalf("a changed file under the same run id: err %v, want the conflict\n%s", err, out)
	}
	if got := len(mempool()); got != 0 {
		t.Fatalf("the refused run sent %d transactions\n%s", got, out)
	}

	// Two payees added at the end are a batch no id of the run holds yet.
	// The run's record refuses them.
	longer := append(append([]withdraw.Payee(nil), payees...), batchPayees(t, 3)[1:]...)
	out, err = command(writeRun("longer.json", longer))
	if err == nil || !strings.Contains(out, withdraw.ErrConflict.Error()) || len(mempool()) != 0 {
		t.Fatalf("payees added under the same run id: err %v, %d in the mempool, want the conflict and nothing sent\n%s", err, len(mempool()), out)
	}

	// A second process on the same state directory is refused.
	lock := filepath.Join(state, "payout.lock")
	if err := os.WriteFile(lock, []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = command(runFile)
	if err == nil || !strings.Contains(out, "payout.lock") {
		t.Fatalf("a run while the lock is held: err %v, want a refusal that names the lock\n%s", err, out)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	// A new run whose last batch holds a payee under the relay floor stops
	// before its first batch is sent.
	runID = "run-2"
	bad := batchPayees(t, 5)
	bad[4].Amount = 1
	out, err = command(writeRun("bad.json", bad))
	if err == nil || !strings.Contains(out, withdraw.ErrInvalidIntent.Error()) || len(mempool()) != 0 {
		t.Fatalf("a bad payee in the last batch: err %v, %d in the mempool, want the refusal and nothing sent\n%s", err, len(mempool()), out)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("a run that stopped left the lock behind")
	}
}
