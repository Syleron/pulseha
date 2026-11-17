package membership

import (
	"net"
	"testing"
)

// Regression for GitHub #276, link 1. A daemon that had just restarted ran its
// first health check one second in, found its peer reachable (2.46ms) but with
// no role yet — in active-passive only a deep check recovers an Unknown peer's
// role, and the deep check runs every fifth cycle — and so saw no Active, elected
// itself as the only Passive coordinator, and demoted the healthy Active that was
// holding all 118 addresses.
//
// A reachable peer whose role is unknown must be asked for it straight away.
func TestAReachablePeerWithAnUnknownRoleIsAskedForItOnTheFirstCycle(t *testing.T) {
	h, peer, _ := newLatchTestChecker(t)
	h.members.GetMemberByID("node-local").SetStatus(StatusPassive)
	peer.SetStatus(StatusUnknown)

	calls := 0
	h.deepCheck = func(*Member) (membershipVerdict, MemberStatus) {
		calls++
		return membershipConfirmed, StatusActive
	}

	// deepCheckCounter is 0, so this is a cheap cycle on the five-cycle cadence:
	// exactly the first cycle after a start.
	h.performHealthChecks()

	if calls != 1 {
		t.Errorf("deep checks on the first cycle = %d, want 1: the peer answered, so its role can be asked for now", calls)
	}
	if got := peer.GetStatus(); got != StatusActive {
		t.Errorf("peer status after the first cycle = %v, want Active: an election would run against a healthy Active", got)
	}
}

// The cost half. A peer that does not answer TCP must stay on the cheap path:
// the deep check carries a 2s gRPC deadline under the health checker's lock, so
// deep-checking every down peer every cycle would stall the whole pass.
func TestAnUnreachablePeerWithAnUnknownRoleIsNotDeepCheckedEveryCycle(t *testing.T) {
	h, peer, _ := newLatchTestChecker(t)
	h.members.GetMemberByID("node-local").SetStatus(StatusPassive)
	peer.SetStatus(StatusUnknown)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	peer.mu.Lock()
	peer.IP, peer.Port = "127.0.0.1", port
	peer.mu.Unlock()

	calls := 0
	h.deepCheck = func(*Member) (membershipVerdict, MemberStatus) {
		calls++
		return membershipConfirmed, StatusActive
	}

	runCycles(h, 4) // cycles 1-4: all cheap on the five-cycle cadence

	if calls != 0 {
		t.Errorf("deep checks for a peer refusing TCP = %d over four cheap cycles, want 0", calls)
	}
	if got := peer.GetStatus(); got != StatusUnknown {
		t.Errorf("peer status = %v, want Unknown", got)
	}
}

// And a peer whose role is already known keeps the cheap cadence.
func TestAPeerWithAKnownRoleKeepsTheCheapCadence(t *testing.T) {
	h, peer, _ := newLatchTestChecker(t)
	h.members.GetMemberByID("node-local").SetStatus(StatusPassive)
	peer.SetStatus(StatusActive)

	calls := 0
	h.deepCheck = func(*Member) (membershipVerdict, MemberStatus) {
		calls++
		return membershipConfirmed, StatusActive
	}

	runCycles(h, 4)

	if calls != 0 {
		t.Errorf("deep checks for a peer with a known role = %d over four cheap cycles, want 0", calls)
	}
}
