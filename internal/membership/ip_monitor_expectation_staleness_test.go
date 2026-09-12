package membership

import (
	"slices"
	"testing"
)

// Regression for docs/TEST-PLAN.md defect #105's second half — the half that
// turned a one-second race into a permanent strand.
//
// On a steady active-passive Active nothing recomputes the expectation set:
// RefreshLocalMonitorExpectedIPs fires only on a role transition, and
// Add/RemoveExpectedIPs are incremental. The enforce pass re-derived the set from
// the config every tick, but only in active-active. So a single stale writer left
// `10.20.70.78/24` expected for the life of the daemon on MC-LB-3-node-1 — eight
// hours after the address had left the config, every tick still logged
// `expectations=map[enp1s0:[10.20.70.67/24 10.20.70.78/24]]`, and the netlink
// watcher restored the address each time it was deleted by hand.
//
// The config is the record of intent. An Active node's expectation set must be
// derived from it every tick, in either mode.
func TestAnActivePassiveNodeRederivesItsExpectationsFromTheConfig(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, nodeID, member := newExpectationsMonitor("active-passive", group, nil)

	// The cached set has an address the config does not: whatever wrote it, the
	// node is now expecting something nothing configures.
	m.UpdateExpectedIPs("eth0", append(slices.Clone(group), "10.0.0.9/24"))

	got := reDeriveExpectations(m, nodeID, member)
	if slices.Contains(got, "10.0.0.9/24") {
		t.Errorf("expectations = %v, want the unconfigured address gone: it is "+
			"restored by the netlink watcher for as long as it is expected", got)
	}
	if !slices.Equal(got, group) {
		t.Errorf("expectations = %v, want the configured group %v", got, group)
	}
}

// The recompute must not undo a release that is still in flight. An address is
// released before it leaves the config — that ordering is what stops a failed
// release stranding it — so for that window the config still names an address the
// node has just been told to give up.
func TestRederivingExpectationsStillHonoursAReleaseInFlight(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, nodeID, member := newExpectationsMonitor("active-passive", group, nil)
	m.UpdateExpectedIPs("eth0", group)

	released := []string{"10.0.0.2/24"}
	m.RemoveExpectedIPs("eth0", released)

	// The config still lists it, so the recompute puts the expectation back...
	expected := reDeriveExpectations(m, nodeID, member)
	if !slices.Contains(expected, "10.0.0.2/24") {
		t.Fatalf("expectations = %v; the fixture no longer reproduces the window", expected)
	}

	// ...but the release record still keeps it down (defect #60).
	restore, suppressed := m.restorableIPs("eth0", expected)
	if slices.Contains(restore, "10.0.0.2/24") {
		t.Errorf("restorable = %v, want the released address left down", restore)
	}
	if !slices.Contains(suppressed, "10.0.0.2/24") {
		t.Errorf("suppressed = %v, want the released address named", suppressed)
	}
}

// The safety-net half of docs/TEST-PLAN.md defect #107.
//
// surplusFloatingIPs scans every *configured* group rather than only the ones
// still assigned to the node — that is #40's fix, and the whole reason
// configured-but-unassigned is a recoverable state. But releaseUnassignedIPs was
// gated on active-active, so in active-passive nothing ever called it and an
// unassigned group's addresses stayed up on the Active node with no pass able to
// compute them.
//
// This pins the computation the ungated call now reaches: the expectation set is
// derived from the assignments, the group is no longer among them, and its
// addresses are therefore surplus wherever the node is still holding them.
func TestAnUnassignedGroupsAddressesAreSurplusInActivePassive(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	m, nodeID, member := newExpectationsMonitor("active-passive", group, nil)

	// Still assigned: the node expects the whole group, so nothing is surplus.
	expected := reDeriveExpectations(m, nodeID, member)
	if !slices.Equal(expected, group) {
		t.Fatalf("expectations = %v, want the whole group while it is assigned", expected)
	}
	held := func(ip string) (string, bool) { return "eth0", true }
	if surplus := surplusFloatingIPs(m.members.Config().Groups,
		map[string][]string{"eth0": expected}, held); len(surplus) != 0 {
		t.Fatalf("surplus = %v while the group is assigned, want none", surplus)
	}

	// Unassign it. The group stays configured -- that is the state the handler
	// leaves behind, and the state this pass has to be able to read.
	cfg := m.members.Config()
	delete(cfg.Nodes[nodeID].IPGroups, "eth0")

	expected = reDeriveExpectations(m, nodeID, member)
	if len(expected) != 0 {
		t.Fatalf("expectations = %v, want none once the group is unassigned", expected)
	}

	surplus := surplusFloatingIPs(cfg.Groups, m.deriveExpectedIPs(nodeID, member), held)
	if !slices.Equal(surplus["eth0"], group) {
		t.Errorf("surplus = %v, want the whole unassigned group %v: with nothing "+
			"computing it, those addresses stay up forever", surplus["eth0"], group)
	}
}
