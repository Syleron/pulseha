package server

import (
	"io"
	"slices"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// newSeedTestServer builds the minimum Server needed by
// seedActiveActiveAssignments: groups, per-node interface mappings, and a member
// list whose statuses mirror an active-passive cluster about to switch mode.
func newSeedTestServer(t *testing.T, group []string, statuses map[string]membership.MemberStatus) (*Server, *membership.MemberList) {
	t.Helper()

	nodes := map[string]*config.Node{}
	for id := range statuses {
		nodes[id] = &config.Node{IPGroups: map[string][]string{"eth0": {"group1"}}}
	}

	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-active", LocalNode: "node-a"},
		Groups: map[string][]string{"group1": group},
		Nodes:  nodes,
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id, status := range statuses {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		m := ml.GetMemberByID(id)
		m.Status = status
	}

	return &Server{config: cfg, logger: logger, memberList: ml}, ml
}

// Regression for docs/TEST-PLAN.md defects #2/#26. Switching to active-active
// used to clear every member's ActiveIPs and redistribute the whole group as if
// nothing held it. The former sole Active still physically held all of it, so
// three peers were told to bring the same addresses up: on whitecrane the switch
// produced ~150 duplicated addresses immediately, and the coordinator then saw
// all 201 as orphaned and placed them again.
func TestSeedActiveActiveAssignmentsRecordsTheCurrentOwner(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"}
	s, ml := newSeedTestServer(t, group, map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
		"node-b": membership.StatusPassive,
		"node-c": membership.StatusPassive,
	})

	if !s.seedActiveActiveAssignments() {
		t.Fatal("expected the Active node to be found")
	}

	if got := ml.GetMemberByID("node-a").ActiveIPs; !slices.Equal(got, group) {
		t.Errorf("owner ActiveIPs = %v, want the whole group %v", got, group)
	}
	for _, id := range []string{"node-b", "node-c"} {
		if got := ml.GetMemberByID(id).ActiveIPs; len(got) != 0 {
			t.Errorf("%s ActiveIPs = %v, want none", id, got)
		}
	}
}

// The election case the old comment called out: Active but with no recorded
// assignments. Reading ActiveIPs would report an empty cluster; the group
// mapping is what says the node holds the addresses.
func TestSeedActiveActiveAssignmentsIgnoresEmptyActiveIPs(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	s, ml := newSeedTestServer(t, group, map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
		"node-b": membership.StatusPassive,
	})
	ml.GetMemberByID("node-a").ActiveIPs = nil

	if !s.seedActiveActiveAssignments() {
		t.Fatal("expected the Active node to be found")
	}
	if got := ml.GetMemberByID("node-a").ActiveIPs; !slices.Equal(got, group) {
		t.Errorf("owner ActiveIPs = %v, want the whole group %v", got, group)
	}
}

// With nothing Active, nothing holds the group and the caller has to place it.
func TestSeedActiveActiveAssignmentsReportsNoOwner(t *testing.T) {
	s, _ := newSeedTestServer(t, []string{"10.0.0.1/24"}, map[string]membership.MemberStatus{
		"node-a": membership.StatusPassive,
		"node-b": membership.StatusUnknown,
	})

	if s.seedActiveActiveAssignments() {
		t.Error("expected no owner to be reported when no node is Active")
	}
}
