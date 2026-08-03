package membership

import (
	"slices"
	"testing"
	"time"
)

// releaseProtectionMonitor builds an active-active monitor whose node is assigned
// the whole of group1 on eth0 and is recorded as holding all of it — the state a
// peer is in when a release lands on it before the config write that says the
// addresses stopped being its share.
func releaseProtectionMonitor(t *testing.T, group []string) (*IPMonitor, string, *Member) {
	t.Helper()

	m, nodeID, member := newExpectationsMonitor("active-active", group, group)
	m.UpdateExpectedIPs("eth0", group)
	return m, nodeID, member
}

// reDeriveExpectations is the enforce pass's first move on an Active
// active-active node: recompute the expectation set from the config and the
// node's own assignment list, and install it. On a node whose config has not yet
// dropped the assignment this puts back the expectation the release just removed,
// which is the resurrection at the heart of defect #60.
func reDeriveExpectations(m *IPMonitor, nodeID string, member *Member) []string {
	expectations := m.deriveExpectedIPs(nodeID, member)
	m.UpdateExpectedIPsAll(expectations)
	return expectations["eth0"]
}

// Regression for docs/TEST-PLAN.md defect #60: a deleted group's release RPC
// races the peer's IP monitor, which restores the addresses it just removed.
//
// Observed on whitecrane in 2 of 6 `group delete --force` runs: the peer logged
// `RPC BringDownIP`, then `Removed IPs from interface`, then `IP monitor:
// expected IP removed from Active node; restoring` for an address it had just
// been told to give up. Dropping the expectation is not enough, because the
// expectation is re-derived from a config that still gives the node the address —
// so the node hands it straight back, and the delete reports success over it.
func TestAReleasedAddressIsNotRestoredWhileTheConfigStillClaimsIt(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}
	m, nodeID, member := releaseProtectionMonitor(t, group)

	released := []string{"10.0.0.2/24"}
	m.RemoveExpectedIPs("eth0", released)

	// The config still assigns the group and the node is still recorded as
	// holding its share, so the expectation comes back.
	expected := reDeriveExpectations(m, nodeID, member)
	if !slices.Contains(expected, "10.0.0.2/24") {
		t.Fatalf("expectations = %v; the fixture no longer reproduces the resurrection", expected)
	}

	// Every address is off the interface as far as the pass can tell; only the
	// released one must stay off.
	restore, suppressed := m.restorableIPs("eth0", expected)
	if slices.Contains(restore, "10.0.0.2/24") {
		t.Errorf("restorable = %v, want the released address left down", restore)
	}
	if !slices.Equal(suppressed, released) {
		t.Errorf("suppressed = %v, want exactly %v", suppressed, released)
	}
	if want := []string{"10.0.0.1/24", "10.0.0.3/24"}; !slices.Equal(restore, want) {
		t.Errorf("restorable = %v, want the addresses nothing released %v", restore, want)
	}
}

// The netlink watcher takes the same decision one address at a time, and it is
// the path the defect was seen on.
func TestTheWatcherLeavesAReleasedAddressDown(t *testing.T) {
	m, _, _ := releaseProtectionMonitor(t, []string{"10.0.0.1/24", "10.0.0.2/24"})

	m.RemoveExpectedIPs("eth0", []string{"10.0.0.2/24"})

	if !m.restoreSuppressed("eth0", "10.0.0.2/24") {
		t.Error("the released address is restorable; the watcher would put it back")
	}
	if m.restoreSuppressed("eth0", "10.0.0.1/24") {
		t.Error("an address nothing released is suppressed; a real removal would go unrepaired")
	}
	if m.restoreSuppressed("eth1", "10.0.0.2/24") {
		t.Error("the protection leaked to another interface")
	}
}

// The mask is not part of the identity: a release and an expectation are written
// by different code paths and are not guaranteed to spell an address the same
// way.
func TestReleaseProtectionIgnoresTheMask(t *testing.T) {
	m, _, _ := releaseProtectionMonitor(t, []string{"10.0.0.1/24"})

	m.RemoveExpectedIPs("eth0", []string{"10.0.0.1/24"})

	if !m.restoreSuppressed("eth0", "10.0.0.1") {
		t.Error("a bare address is not matched against a release written in CIDR")
	}
}

// The protection is a backstop, not a state: it has to lapse, or an address this
// node genuinely owns and genuinely lost would stay down forever after any
// release it was once part of.
func TestReleaseProtectionExpires(t *testing.T) {
	m, nodeID, member := releaseProtectionMonitor(t, []string{"10.0.0.1/24", "10.0.0.2/24"})

	now := time.Now()
	m.now = func() time.Time { return now }

	m.RemoveExpectedIPs("eth0", []string{"10.0.0.2/24"})
	if !m.restoreSuppressed("eth0", "10.0.0.2/24") {
		t.Fatal("the release was not recorded at all")
	}

	now = now.Add(releaseGraceWindow + time.Second)
	if m.restoreSuppressed("eth0", "10.0.0.2/24") {
		t.Error("the protection outlived its window; the address can never be repaired")
	}

	restore, suppressed := m.restorableIPs("eth0", reDeriveExpectations(m, nodeID, member))
	if len(suppressed) != 0 {
		t.Errorf("suppressed = %v after the window, want none", suppressed)
	}
	if !slices.Contains(restore, "10.0.0.2/24") {
		t.Errorf("restorable = %v, want the address back under the monitor's care", restore)
	}
}

// A node handed an address back is a node that must serve it now, not once the
// window from an earlier release runs out. Which setter says that is the whole
// distinction: BringUpIP's per-address add is an instruction, and the two
// config-derived setters are a re-reading of the state that lags on exactly the
// node that has just released — clearing on those re-arms the defect.
func TestOnlyAnExplicitReassignmentClearsTheProtection(t *testing.T) {
	cases := []struct {
		name           string
		reassign       func(m *IPMonitor)
		wantRestorable bool
	}{
		{
			name:           "AddExpectedIPs, as BringUpIP does per address",
			reassign:       func(m *IPMonitor) { m.AddExpectedIPs("eth0", []string{"10.0.0.2/24"}) },
			wantRestorable: true,
		},
		{
			// refreshLocalMonitorExpectedIPs, run on every config sync. On the node
			// mid-release the config it reads still hands the address over.
			name:           "UpdateExpectedIPs, as a config refresh does",
			reassign:       func(m *IPMonitor) { m.UpdateExpectedIPs("eth0", []string{"10.0.0.1/24", "10.0.0.2/24"}) },
			wantRestorable: false,
		},
		{
			// The enforce loop's own write-back of what it derived.
			name: "UpdateExpectedIPsAll, as the enforce loop does",
			reassign: func(m *IPMonitor) {
				m.UpdateExpectedIPsAll(map[string][]string{"eth0": {"10.0.0.1/24", "10.0.0.2/24"}})
			},
			wantRestorable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := releaseProtectionMonitor(t, []string{"10.0.0.1/24", "10.0.0.2/24"})

			m.RemoveExpectedIPs("eth0", []string{"10.0.0.2/24"})
			tc.reassign(m)

			if restorable := !m.restoreSuppressed("eth0", "10.0.0.2/24"); restorable != tc.wantRestorable {
				t.Errorf("restorable = %v, want %v", restorable, tc.wantRestorable)
			}
		})
	}
}
