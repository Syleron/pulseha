package membership

import (
	"io"
	"slices"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

func newExpectationsMonitor(mode string, groupIPs, assigned []string) (*IPMonitor, string, *Member) {
	const nodeID = "node-a"

	cfg := &config.Config{
		Pulse:  config.Local{Mode: mode, LocalNode: nodeID},
		Groups: map[string][]string{"group1": groupIPs},
		Nodes: map[string]*config.Node{
			nodeID: {IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}

	logger := log.New(io.Discard)
	member := newAATestMember(nodeID, "host-a", StatusActive, assigned)
	ml := newAATestMemberList(cfg, member)

	return NewIPMonitor(ml, logger), nodeID, member
}

// Regression for docs/TEST-PLAN.md defects #2/#26. The expectation set is written
// by several code paths, and a node that was the sole active-passive Active kept
// the whole group across a switch to active-active — then re-added all 201
// whitecrane RealTest addresses on every enforce tick, so the coordinator's
// rebalance moves were undone as fast as it could make them.
func TestDeriveExpectedIPs(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"}

	cases := []struct {
		name     string
		mode     string
		assigned []string
		want     []string
	}{
		{
			name:     "active-passive expects the whole group",
			mode:     "active-passive",
			assigned: nil,
			want:     group,
		},
		{
			name:     "active-passive ignores assignments entirely",
			mode:     "active-passive",
			assigned: []string{"10.0.0.2/24"},
			want:     group,
		},
		{
			name:     "active-active expects only the assigned subset",
			mode:     "active-active",
			assigned: []string{"10.0.0.1/24", "10.0.0.4/24"},
			want:     []string{"10.0.0.1/24", "10.0.0.4/24"},
		},
		{
			// The whole-group case that caused the thrash.
			name:     "active-active with no assignments expects nothing",
			mode:     "active-active",
			assigned: nil,
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, nodeID, member := newExpectationsMonitor(tc.mode, group, tc.assigned)
			got := m.deriveExpectedIPs(nodeID, member)

			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("deriveExpectedIPs() = %v, want no interfaces", got)
				}
				return
			}
			if !slices.Equal(got["eth0"], tc.want) {
				t.Errorf("deriveExpectedIPs()[eth0] = %v, want %v", got["eth0"], tc.want)
			}
		})
	}
}

// The enforce loop recomputes from assignments each tick, so a cache left over
// from active-passive must not survive into active-active.
func TestUpdateExpectedIPsAllReplacesStaleCache(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}
	m, nodeID, member := newExpectationsMonitor("active-active", group, []string{"10.0.0.2/24"})

	// Simulate the stale whole-group cache seeded while the node was the sole
	// active-passive Active.
	m.UpdateExpectedIPsAll(map[string][]string{"eth0": group})
	if got := m.GetExpectedIPs("eth0"); len(got) != 3 {
		t.Fatalf("precondition: expected stale cache of 3, got %v", got)
	}

	m.UpdateExpectedIPsAll(m.deriveExpectedIPs(nodeID, member))

	want := []string{"10.0.0.2/24"}
	if got := m.GetExpectedIPs("eth0"); !slices.Equal(got, want) {
		t.Errorf("after recompute, GetExpectedIPs(eth0) = %v, want %v", got, want)
	}
}

// Regression for docs/TEST-PLAN.md defect #40. The release pass drew its
// surplus set only from the groups currently assigned to the node, so
// unassigning a group put every address it was serving outside every set the
// pass could compute: node-3 logged "Current expectations expectations=map[]"
// and released nothing, on every tick, while still serving all 61 addresses.
func TestSurplusFloatingIPsReleasesAnUnassignedGroup(t *testing.T) {
	groups := map[string][]string{
		"RealTest":   {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"},
		"Management": {"10.0.1.1/24"},
	}
	// The node still serves all of RealTest but is only assigned Management.
	held := map[string]string{
		"10.0.0.1/24": "eth0",
		"10.0.0.2/24": "eth0",
		"10.0.0.3/24": "eth0",
		"10.0.1.1/24": "eth0",
	}
	expectations := map[string][]string{"eth0": {"10.0.1.1/24"}}

	surplus := surplusFloatingIPs(groups, expectations, locatorFor(held))

	want := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}
	if !slices.Equal(surplus["eth0"], want) {
		t.Errorf("surplus on eth0 = %v, want %v", surplus["eth0"], want)
	}
}

func TestSurplusFloatingIPsLeavesExpectedAddresses(t *testing.T) {
	groups := map[string][]string{"RealTest": {"10.0.0.1/24", "10.0.0.2/24"}}
	held := map[string]string{"10.0.0.1/24": "eth0", "10.0.0.2/24": "eth0"}
	expectations := map[string][]string{"eth0": {"10.0.0.1/24", "10.0.0.2/24"}}

	if surplus := surplusFloatingIPs(groups, expectations, locatorFor(held)); len(surplus) != 0 {
		t.Errorf("expected nothing released when the node holds exactly its expectations, got %v", surplus)
	}
}

// Addresses the node does not hold are not surplus: the release pass must not
// try to bring down what is not up, and an address hosted by a peer is that
// peer's business.
func TestSurplusFloatingIPsIgnoresAddressesNotHeld(t *testing.T) {
	groups := map[string][]string{"RealTest": {"10.0.0.1/24", "10.0.0.2/24"}}
	held := map[string]string{"10.0.0.1/24": "eth0"}

	surplus := surplusFloatingIPs(groups, map[string][]string{}, locatorFor(held))

	if !slices.Equal(surplus["eth0"], []string{"10.0.0.1/24"}) {
		t.Errorf("expected only the held address released, got %v", surplus["eth0"])
	}
}

// A node with no expectations at all — every group unassigned — releases
// everything it is still serving. This is the shape a fully drained node has,
// and the pass used to compute an empty surplus for it.
func TestSurplusFloatingIPsReleasesEverythingWhenNothingIsExpected(t *testing.T) {
	groups := map[string][]string{"RealTest": {"10.0.0.1/24"}, "Management": {"10.0.1.1/24"}}
	held := map[string]string{"10.0.0.1/24": "eth0", "10.0.1.1/24": "eth1"}

	surplus := surplusFloatingIPs(groups, nil, locatorFor(held))

	if !slices.Equal(surplus["eth0"], []string{"10.0.0.1/24"}) {
		t.Errorf("surplus on eth0 = %v, want [10.0.0.1/24]", surplus["eth0"])
	}
	if !slices.Equal(surplus["eth1"], []string{"10.0.1.1/24"}) {
		t.Errorf("surplus on eth1 = %v, want [10.0.1.1/24]", surplus["eth1"])
	}
}

// Addresses outside every configured group are never candidates: they are the
// node's own, not cluster floating IPs.
func TestSurplusFloatingIPsIgnoresNonGroupAddresses(t *testing.T) {
	groups := map[string][]string{"RealTest": {"10.0.0.1/24"}}
	held := map[string]string{"10.0.0.1/24": "eth0", "192.168.1.5/24": "eth0"}

	surplus := surplusFloatingIPs(groups, nil, locatorFor(held))

	if !slices.Equal(surplus["eth0"], []string{"10.0.0.1/24"}) {
		t.Errorf("expected only the group address released, got %v", surplus["eth0"])
	}
}

func locatorFor(held map[string]string) func(string) (string, bool) {
	return func(ip string) (string, bool) {
		iface, ok := held[ip]
		return iface, ok
	}
}

// Deliberate behaviour change with docs/TEST-PLAN.md defect #40. The old pass
// only ever looked at interfaces listed in the node's IPGroups, and unassigning
// a node's last group deletes that entry outright — so the interface still
// carrying the group's addresses became invisible to the release pass. Surplus
// is now decided by where an address actually is, not by which interfaces the
// node is still assigned.
func TestSurplusFloatingIPsReleasesOnAnUnmappedInterface(t *testing.T) {
	groups := map[string][]string{"RealTest": {"10.0.0.1/24", "10.0.0.2/24"}}
	held := map[string]string{
		"10.0.0.1/24": "eth1", // not an interface the node maps any group to
		"10.0.0.2/24": "eth0",
	}
	expectations := map[string][]string{"eth0": {"10.0.0.2/24"}}

	surplus := surplusFloatingIPs(groups, expectations, locatorFor(held))

	if !slices.Equal(surplus["eth1"], []string{"10.0.0.1/24"}) {
		t.Errorf("surplus on eth1 = %v, want [10.0.0.1/24]", surplus["eth1"])
	}
	if len(surplus["eth0"]) != 0 {
		t.Errorf("expected the expected address left alone, got %v", surplus["eth0"])
	}
}
