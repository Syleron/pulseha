package membership

import (
	"slices"
	"testing"
)

// releaseAfterPromotionMonitor is a node in an active-passive cluster that is
// assigned the whole of group1 on eth0, with the protection in its first state:
// nothing recorded. Each test drives the member's role and the release itself.
func releaseAfterPromotionMonitor(t *testing.T, group []string) (*IPMonitor, *Member) {
	t.Helper()
	m, _, member := newExpectationsMonitor("active-passive", group, nil)
	// No real enforce pass. On Linux it re-derives the expectations in the
	// background, racing the assertions, and reaches for netlink on eth0. What is
	// under test is the release record, which the pass only reads.
	m.enforce = func() {}
	return m, member
}

func suppressedOf(m *IPMonitor, iface string, ips []string) []string {
	_, released := m.restorableIPs(iface, ips)
	return released
}

// Regression for GitHub #265, the first trigger. An explicit promotion crossed a
// consolidation demotion: the node was told to go Passive and released the group
// while it still counted as Active — MakePassive releases before it demotes — and
// was then made the agreed Active again through config sync. Nothing on that path
// clears the release record, so the Active refused its own group for the whole
// grace window: dark for 78s, with status, health checks and elections all
// reading the cluster as healthy.
//
// A release made before the node's latest move to Active is a release of a role
// the node has since been given back.
func TestAReleaseFromBeforeTheNodeWasMadeActiveAgainDoesNotSuppressItsRestore(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, member := releaseAfterPromotionMonitor(t, group)

	member.SetStatus(StatusActive)
	m.RemoveExpectedIPs("eth0", group) // the demotion's release, made while still Active
	member.SetStatus(StatusPassive)
	member.SetStatus(StatusActive) // agreed Active again, through config sync

	if got := suppressedOf(m, "eth0", group); len(got) != 0 {
		t.Errorf("an Active node refused %v it was told to release before it was made Active again", got)
	}
	if m.restoreSuppressed("eth0", "10.0.0.1/24") {
		t.Error("the watcher would leave the agreed Active's own address down")
	}
}

// The second trigger: a restarted Active comes up Passive, its startup cleanup
// releases the group through its own BringDownIP, and a second later it is the
// Active again. Dark for 61s on dev, from a release nobody asked it to make.
func TestAStartupReleaseDoesNotOutliveTheNodesPromotion(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, member := releaseAfterPromotionMonitor(t, group)

	member.SetStatus(StatusPassive)
	m.RemoveExpectedIPs("eth0", group) // the startup cleanup
	member.SetStatus(StatusActive)

	if got := suppressedOf(m, "eth0", group); len(got) != 0 {
		t.Errorf("a restarted node made Active refused %v from its own startup cleanup", got)
	}
}

// The guard. Defect #60 is a node that is Active when it is told to release and
// stays Active while its config catches up. Its release is newer than its
// promotion, so it must still be protected.
func TestAReleaseMadeWhileActiveIsStillProtected(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, member := releaseAfterPromotionMonitor(t, group)

	member.SetStatus(StatusActive)
	m.RemoveExpectedIPs("eth0", []string{"10.0.0.2/24"})

	got := suppressedOf(m, "eth0", group)
	if !slices.Equal(got, []string{"10.0.0.2/24"}) {
		t.Errorf("suppressed = %v, want the address released while Active: #60's protection must hold", got)
	}
	if !m.restoreSuppressed("eth0", "10.0.0.2/24") {
		t.Error("the watcher would restore an address released while Active (#60)")
	}
}

// Staying Active does not refresh the moment: a second Active write is not a
// promotion, and must not wipe a release made in between.
func TestRestatingActiveIsNotAPromotion(t *testing.T) {
	group := []string{"10.0.0.1/24"}
	m, member := releaseAfterPromotionMonitor(t, group)

	member.SetStatus(StatusActive)
	m.RemoveExpectedIPs("eth0", group)
	member.SetStatus(StatusActive)
	member.SetClaim(Claim{Status: StatusActive, ActiveIPs: group})

	if got := suppressedOf(m, "eth0", group); !slices.Equal(got, group) {
		t.Errorf("suppressed = %v, want %v: re-stating Active erased a release made while Active", got, group)
	}
}

// The third trigger. BringUpIP drops the additions it could not verify, and did
// it through RemoveExpectedIPs, which also records them as released: an Active
// whose addresses a NetworkManager re-apply had taken down for an instant then
// refused them for 74s. An addition that did not take is not a release.
func TestForgettingAnUnverifiedAdditionDoesNotArmTheProtection(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, member := releaseAfterPromotionMonitor(t, group)
	member.SetStatus(StatusActive)

	m.AddExpectedIPs("eth0", group)
	m.ForgetExpectedIPs("eth0", []string{"10.0.0.2/24"})

	if got := m.GetExpectedIPs("eth0"); slices.Contains(got, "10.0.0.2/24") {
		t.Errorf("expectations = %v, want the unverified addition dropped", got)
	}
	if got := suppressedOf(m, "eth0", group); len(got) != 0 {
		t.Errorf("suppressed = %v, want none: forgetting an addition is not a release", got)
	}
}
