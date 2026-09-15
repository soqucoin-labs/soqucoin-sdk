package electrumx

import (
	"testing"

	"go.uber.org/goleak"
)

// Every test in this package is checked for leaked goroutines.
//
// This client runs a reader per connection generation, a refresher and a ping
// loop, and each of them must end when the context ends or Stop is called. A
// goroutine that outlives its client is not a tidiness problem: it holds the
// connection lock, it keeps writing to a socket the caller believes is closed,
// and in a long-running exchange process the leak compounds one connection at
// a time until the indexer refuses new ones. Nothing in an ordinary assertion
// notices that, which is why it is checked here rather than test by test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
