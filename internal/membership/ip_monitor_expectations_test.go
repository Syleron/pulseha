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
