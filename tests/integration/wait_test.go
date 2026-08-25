package integration

import (
	"testing"
	"time"

	"github.com/syleron/pulseha/rpc"
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

// requireReportedStatus waits for the status observer *publishes* for target to
// reach want, and fails with the last value it actually saw.
//
// The sibling above reads the member list directly, which is a different
// question: it sees what the daemon believes, not what it tells an operator.
// Only this one crosses deriveMemberStatus, and END-2289 was a defect that lived
// entirely on the far side of it.
func requireReportedStatus(
	t *testing.T,
	observer *testutils.TestNode,
	targetHostname string,
	want rpc.MemberStatusEnum,
	msg string,
) {
	t.Helper()

	var (
		got rpc.MemberStatusEnum
		err error
	)
	deadline := time.Now().Add(statusSettleTimeout)
	for time.Now().Before(deadline) {
		got, err = observer.ReportedStatus(targetHostname)
		if err == nil && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err != nil {
		t.Fatalf("%s: %s could not report a status for %s within %s: %v",
			msg, observer.Hostname, targetHostname, statusSettleTimeout, err)
	}
	t.Fatalf("%s: %s reports %s as %v after %s, want %v",
		msg, observer.Hostname, targetHostname, got, statusSettleTimeout, want)
}

// requireAgreedStatus waits for two observers to publish the same status for target,
// and fails with what each of them last said.
//
// The same lesson as requireMemberStatus above, applied to the one assertion in this
// package that still asserted at an arbitrary instant. Comparing two ReportedStatus
// calls directly is two sequential RPCs against a cluster whose statuses are still
// moving -- the tests drive a 1ms health-check interval, so an election can land
// between them and the reads straddle it. That is a race in the assertion, not
// disagreement between the nodes: what the test means is that the two converge on one
// answer, which is a property with a deadline rather than an instant.
//
// Measured before this existed: the containerised run failed 6 of 15 on the branch that
// added it and 15 of 15 on origin/dev, so it is the assertion that is wrong rather than
// any one change under it.
func requireAgreedStatus(
	t *testing.T,
	a, b *testutils.TestNode,
	targetHostname string,
	msg string,
) {
	t.Helper()

	var (
		fromA, fromB rpc.MemberStatusEnum
		errA, errB   error
	)
	deadline := time.Now().Add(statusSettleTimeout)
	for time.Now().Before(deadline) {
		fromA, errA = a.ReportedStatus(targetHostname)
		fromB, errB = b.ReportedStatus(targetHostname)
		if errA == nil && errB == nil && fromA == fromB {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	if errA != nil {
		t.Fatalf("%s should report a status for %s: %v", a.Hostname, targetHostname, errA)
	}
	if errB != nil {
		t.Fatalf("%s should report a status for %s: %v", b.Hostname, targetHostname, errB)
	}
	t.Fatalf("%s: %s says %v, %s says %v", msg, a.Hostname, fromA, b.Hostname, fromB)
}
