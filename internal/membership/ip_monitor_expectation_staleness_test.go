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
