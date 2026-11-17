package membership

import "testing"

// twoNodeAfterMakePassive is node-a just after the MakePassive of a handoff to
// node-b: both nodes read Passive from here — node-b's new status is not
// broadcast until its transfer finishes — and node-a holds the handoff guard.
// node-a has the lower ID, so it is the election coordinator, as the demoted
// node was on the lab pair; a non-coordinator never promotes anyway, and would
// pass these tests without the guard being read at all.
func twoNodeAfterMakePassive(t *testing.T, handoff string) (*HealthChecker, *stubServer, *Member) {
	t.Helper()
	a := newAATestMember("node-a", "host-a", StatusPassive, nil)
	b := newAATestMember("node-b", "host-b", StatusPassive, nil)
	h, stub := newAPTestChecker("node-a", a, b)
	stub.handoff = handoff
	return h, stub, a
}

// Regression for GitHub #286. #266's handoff guard is enforced by voters, which
// probe the other members and refuse while any reports a handoff — but a
// two-node cluster takes no vote (ADR-0002), so nothing read the guard. On the
// lab pair the demoted node elected itself ~0.3s after its MakePassive in 2 of 3
// handoffs and took the group back while the target was still acquiring it:
// 16s of every address on both nodes before #283 bounded it, ~0.5s after.
func TestATwoNodeClusterDoesNotElectDuringAHandoffThisNodeIsPartOf(t *testing.T) {
	h, stub, a := twoNodeAfterMakePassive(t, "node-b")

	if h.tryAutomaticPromotion(a) {
		t.Error("automatic promotion accepted on a node part of a handoff in progress")
	}
	if len(stub.promotions) != 0 {
		t.Errorf("sent %d Promote requests mid-handoff, want 0", len(stub.promotions))
	}
}

// The whole election path, as the health check runs it.
func TestTheElectionRespectsTheHandoffGuard(t *testing.T) {
	h, stub, _ := twoNodeAfterMakePassive(t, "node-b")

	h.electNewActiveNode()

	if len(stub.promotions) != 0 {
		t.Errorf("the election promoted %v mid-handoff, want nothing", stub.promotions)
	}
}

// The guard is the only thing in the way: with no handoff, the same two-node
// election still promotes, as ADR-0002 intends.
func TestATwoNodeClusterStillElectsWithNoHandoff(t *testing.T) {
	h, stub, a := twoNodeAfterMakePassive(t, "")

	if !h.tryAutomaticPromotion(a) || len(stub.promotions) != 1 {
		t.Errorf("no promotion with no handoff in progress (promotions=%d); a two-node cluster must still elect", len(stub.promotions))
	}
}
