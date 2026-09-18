package resilience

import (
	"fmt"
	"testing"
	"time"

	"github.com/soqucoin-labs/soqucoin-sdk/rpc"
	"github.com/soqucoin-labs/soqucoin-sdk/withdraw"
)

// A node refusing the credentials, and an intent held after a rejection, are
// both conditions that stop the run: nothing sent under them can succeed, and
// an operator resolves them. The hold carries the rejection as text only, so
// the rpc.ErrPermanent inside it does not read as one bad request.
func TestUnauthorizedNodeAndAHeldRejectionCountAsSystemic(t *testing.T) {
	for _, e := range []error{
		fmt.Errorf("broadcast: %w", rpc.ErrUnauthorized),
		fmt.Errorf("%w: w1: %s: %v", withdraw.ErrHeld, withdraw.HoldRejectedAfterUnknown, rpc.ErrPermanent),
	} {
		if !NewCircuitBreaker(1, time.Hour, nil).RecordResult(e) {
			t.Errorf("not counted, so nothing stops the run: %v", e)
		}
	}
}
