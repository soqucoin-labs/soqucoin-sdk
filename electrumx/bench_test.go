package electrumx

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/address"
	"github.com/soqucoin-labs/soqucoin-sdk/types"
)

// benchAddrs returns n distinct stagenet v1 addresses.
func benchAddrs(b *testing.B, n int) []string {
	b.Helper()
	out := make([]string, n)
	prog := make([]byte, 32)
	for i := range out {
		binary.BigEndian.PutUint32(prog[28:], uint32(i+1))
		a, err := address.Encode(types.Stagenet.HRP, 1, prog)
		if err != nil {
			b.Fatal(err)
		}
		out[i] = a
	}
	return out
}

// BenchmarkSubscribeAndRefreshPerAddress measures the client's own cost of
// bringing one address up on a fresh connection: one subscribe call and one
// listunspent, over loopback against a server that answers at once. This is
// the figure the exchange guide cites for the reconnect ramp and the
// reconcile pass; the round trip to a real indexer is added to it.
//
//	go test ./electrumx -run '^$' -bench SubscribeAndRefresh -benchtime 3x
func BenchmarkSubscribeAndRefreshPerAddress(b *testing.B) {
	const n = 1000
	stub := newScriptedStub(b, types.Stagenet.GenesisHash, func(req request) []string {
		switch req.Method {
		case "blockchain.scripthash.subscribe":
			return []string{reply(req.ID, `null`)}
		case "blockchain.scripthash.listunspent":
			return []string{reply(req.ID, `[]`)}
		}
		return []string{reply(req.ID, `null`)}
	})
	addrs := benchAddrs(b, n)
	c := NewClient(stub.addr(), time.Hour, nil)
	setHRP(b, c, types.Stagenet.HRP)
	if err := c.Connect(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer c.Stop()
	if err := c.TrackAddresses(addrs); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A new connection: every subscription is made again.
		if err := c.Reconnect(context.Background()); err != nil {
			b.Fatal(err)
		}
		if _, err := c.pass(context.Background(), true); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	perAddr := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(n)
	b.ReportMetric(perAddr/1000, "µs/address")
	b.ReportMetric(1e9/perAddr, "addresses/s")
}

// BenchmarkNotificationToRefresh measures the client's own cost from a
// scripthash notification to the listunspent that answers it, over loopback.
//
//	go test ./electrumx -run '^$' -bench NotificationToRefresh -benchtime 1000x
func BenchmarkNotificationToRefresh(b *testing.B) {
	done := make(chan struct{}, 1)
	stub := newScriptedStub(b, types.Stagenet.GenesisHash, func(req request) []string {
		switch req.Method {
		case "blockchain.scripthash.subscribe":
			return []string{reply(req.ID, `null`)}
		case "blockchain.scripthash.listunspent":
			select {
			case done <- struct{}{}:
			default:
			}
			return []string{reply(req.ID, `[]`)}
		}
		return []string{reply(req.ID, `null`)}
	})
	a := benchAddrs(b, 1)[0]
	sh, _ := address.AddressToScriptHash(types.Stagenet.HRP, a)
	c := NewClient(stub.addr(), time.Hour, nil)
	setHRP(b, c, types.Stagenet.HRP)
	c.PingInterval = time.Hour
	if err := c.Connect(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer c.Stop()
	if err := c.TrackAddresses([]string{a}); err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	<-done // the first refresh
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stub.push(scripthashNotification(sh, fmt.Sprintf("s%d", i)))
		<-done
	}
}
