package server

import (
	"io"
	"slices"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// newExpectedIPsServer builds the minimum Server needed by expectedIfaceIPs: a
// config carrying the mode, the groups and the node's interface mapping, plus a
// member list holding the node's current assignments.
func newExpectedIPsServer(mode string, groupIPs, assigned []string) *Server {
	const nodeID = "node-a"

	cfg := &config.Config{
		Pulse:  config.Local{Mode: mode, LocalNode: nodeID},
		Groups: map[string][]string{"group1": groupIPs},
		Nodes: map[string]*config.Node{
			nodeID: {IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	if err := ml.AddMemberQuiet(nodeID); err != nil {
		panic(err)
	}
	m := ml.GetMemberByID(nodeID)
	m.Status = membership.StatusActive
	m.ActiveIPs = assigned

	return &Server{config: cfg, logger: logger, memberList: ml}
}

// Regression for docs/TEST-PLAN.md defects #2/#26. Every site that seeded the IP
// monitor's expectations rebuilt them from the whole configured group, ignoring
// cluster mode. In active-active the group is shared, so each Active node's next
// enforce tick re-added all 201 addresses of the whitecrane RealTest group —
// undoing the coordinator's rebalance moves as fast as it could make them. TC-6
// never converged: 141 "successful" moves, counts stuck at 37/53/45/68 against a
// ~50 target, and one node climbing to 199 of 201 addresses.
func TestExpectedIfaceIPs(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"}

	cases := []struct {
		name     string
		mode     string
		assigned []string
		want     []string
	}{
		{
			name:     "active-passive expects the whole group regardless of assignments",
			mode:     "active-passive",
			assigned: nil,
			want:     group,
		},
		{
			name:     "active-active expects only the assigned subset",
			mode:     "active-active",
			assigned: []string{"10.0.0.2/24", "10.0.0.3/24"},
			want:     []string{"10.0.0.2/24", "10.0.0.3/24"},
		},
		{
			// The distinction that made the bug: "assigned nothing" must not
			// collapse into "no restriction". A node awaiting its first
			// assignment should hold nothing, not the entire group.
			name:     "active-active with no assignments expects nothing",
			mode:     "active-active",
			assigned: nil,
			want:     nil,
		},
		{
			name:     "active-active ignores assignments outside the group",
			mode:     "active-active",
			assigned: []string{"10.0.0.2/24", "192.168.5.5/24"},
			want:     []string{"10.0.0.2/24"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newExpectedIPsServer(tc.mode, group, tc.assigned)
			got := s.expectedIfaceIPs("node-a", "eth0")
			if !slices.Equal(got, tc.want) {
				t.Errorf("expectedIfaceIPs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unknown node must not silently resolve to the whole group.
func TestExpectedIfaceIPsUnknownNodeOrInterface(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	s := newExpectedIPsServer("active-passive", group, nil)

	if got := s.expectedIfaceIPs("node-missing", "eth0"); got != nil {
		t.Errorf("unknown node: expectedIfaceIPs() = %v, want nil", got)
	}
	if got := s.expectedIfaceIPs("node-a", "eth9"); got != nil {
		t.Errorf("unknown interface: expectedIfaceIPs() = %v, want nil", got)
	}
}
