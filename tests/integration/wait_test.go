package integration

import (
	"testing"
	"time"

	"github.com/syleron/pulseha/tests/testutils"
)

// statusSettleTimeout bounds how long a member status is given to converge.
//
// Generous on purpose. These assertions used to be a bare time.Sleep sized to
// what a dev machine happened to need, which is why the same three tests failed
// with different values on consecutive CI runs -- "unknown" when the health
// checks had not converged yet, "passive" once they had, "active" once an
// election had run. A deadline that is too long only costs time on a genuine
// failure; one that is too short fails a cluster that was going to converge.
const statusSettleTimeout = 15 * time.Second

// requireMemberStatus waits for observer's view of target to reach want, and
// fails with the last status it actually saw.
//
// Polling rather than sleeping is the point: the previous fixed sleeps asserted
// at one arbitrary instant, so a loaded runner decided the result. This asserts
// that the cluster *reaches* the expected state within a bound, which is the
// property these tests were always trying to check.
func requireMemberStatus(t *testing.T, observer *testutils.TestNode, targetHostname, want, msg string) {
	t.Helper()

	var got string
	deadline := time.Now().Add(statusSettleTimeout)
	for time.Now().Before(deadline) {
		got = observer.GetMemberStatus(targetHostname)
		if got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("%s: %s's view of %s = %q after %s, want %q",
		msg, observer.Hostname, targetHostname, got, statusSettleTimeout, want)
}
