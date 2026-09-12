package server

import (
	"context"
	"slices"
	"testing"

	"github.com/syleron/pulseha/rpc"
)

// Regression for docs/TEST-PLAN.md defect #107.
//
// UnassignGroupFromNode dropped the assignment, reported success, and released
// nothing. Its comment said the config broadcast would land as a ConfigSync and
// the health checker would reconcile — but a node never ConfigSyncs itself, so on
// the node the command ran on nothing was driven at all, and in active-passive
// the pass that would have caught it was gated off. The group's addresses stayed
// up on the Active node with the operator told the unassign had worked.
func TestUnassignReleasesTheGroupsAddressesOnTheNode(t *testing.T) {
	peer := &releasingPeer{}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	// Unassign from the peer, whose release is observable.
	resp, err := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
		GroupName: "group1",
		NodeId:    "peer-0",
		Interface: removeIPIface,
	})
	if err != nil {
		t.Fatalf("UnassignGroupFromNode: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q)", resp.Message)
	}

	want := groupIPs(s)
	if got := peer.released(); !slices.Equal(got, want) {
		t.Errorf("peer released %v, want the whole group %v it no longer holds", got, want)
	}
}

// The assignment has to be gone before the release goes out, which is the
// opposite order from RemoveIPFromGroup and deliberate: an Active node expects
// every address of every group mapped to the interface, so releasing while it is
// still assigned means the next enforce tick brings it all back (#60).
func TestUnassignDropsTheAssignmentBeforeReleasing(t *testing.T) {
	var stillAssigned bool

	peer := &releasingPeer{}
	var s *Server
	peer.inspect = func() {
		s.RLock()
		defer s.RUnlock()
		stillAssigned = slices.Contains(s.config.Nodes["peer-0"].IPGroups[removeIPIface], "group1")
	}
	s = newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	if _, err := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
		GroupName: "group1",
		NodeId:    "peer-0",
		Interface: removeIPIface,
	}); err != nil {
		t.Fatalf("UnassignGroupFromNode: %v", err)
	}

	if stillAssigned {
		t.Error("the group was still assigned when the release went out; the enforce " +
			"loop re-adds what it is still expected to hold")
	}
}

// Unlike RemoveIPFromGroup and DeleteGroup, an unconfirmed release must not fail
// the operation. The group stays configured, so its addresses are still
// referenced by something every pass can compute — and an unassign that could be
// blocked by an unreachable node is one you cannot use when you most need it.
func TestUnassignSucceedsButWarnsWhenTheReleaseCannotBeConfirmed(t *testing.T) {
	peer := &releasingPeer{refuse: true}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	resp, err := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
		GroupName: "group1",
		NodeId:    "peer-0",
		Interface: removeIPIface,
	})
	if err != nil {
		t.Fatalf("UnassignGroupFromNode: %v", err)
	}
	if !resp.Success {
		t.Error("Success = false; the addresses are still configured, so nothing is stranded " +
			"and refusing would make the command unusable against an unreachable node")
	}
	if len(resp.Warnings) == 0 {
		t.Error("no warning: the operator cannot tell that convergence is now the " +
			"enforce pass's rather than this call's")
	}
	if slices.Contains(s.config.Nodes["peer-0"].IPGroups[removeIPIface], "group1") {
		t.Error("the assignment survived; the unassign did not happen at all")
	}
}

// An address another group still assigned to the same interface also provides is
// left up. Nothing in the CLI can create that overlap, but config.json is written
// by the appliance too (#3), and tearing down an address a live group still
// serves would be an outage.
func TestUnassignDoesNotReleaseAnAddressAnotherAssignedGroupProvides(t *testing.T) {
	peer := &releasingPeer{}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	shared := removeIPTarget
	s.Lock()
	s.config.Groups["group2"] = []string{shared}
	for _, node := range s.config.Nodes {
		node.IPGroups[removeIPIface] = append(node.IPGroups[removeIPIface], "group2")
	}
	s.Unlock()

	if _, err := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
		GroupName: "group1",
		NodeId:    "peer-0",
		Interface: removeIPIface,
	}); err != nil {
		t.Fatalf("UnassignGroupFromNode: %v", err)
	}

	if got := peer.released(); slices.Contains(got, shared) {
		t.Errorf("released %v, which includes %s — group2 still provides that "+
			"address on the same interface", got, shared)
	}
}

// The plan is taken before the mutation, because expectedIfaceIPs answers
// "which of these are mine" from the very assignment being removed. Taken after,
// it answers nothing and the release is empty — which is the defect wearing a
// different hat.
func TestUnassignPlanIsTakenBeforeTheAssignmentGoes(t *testing.T) {
	s := newRemoveIPTestServer(t)

	s.Lock()
	before := s.planGroupUnassignRelease("group1", removeIPLocal, removeIPIface)
	s.Unlock()
	if len(before) != 1 || len(before[0].ips) == 0 {
		t.Fatalf("plan = %+v, want the local node's whole share", before)
	}

	s.Lock()
	delete(s.config.Nodes[removeIPLocal].IPGroups, removeIPIface)
	after := s.planGroupUnassignRelease("group1", removeIPLocal, removeIPIface)
	s.Unlock()
	if len(after) != 0 && len(after[0].ips) != 0 {
		t.Fatalf("plan after the mutation = %+v, want nothing: this is why the "+
			"plan has to be taken first", after)
	}
}

// Idempotent and invalid requests keep answering as they did.
func TestUnassignStillAnswersTheEasyCases(t *testing.T) {
	s := newRemoveIPTestServer(t)

	t.Run("an unknown group is refused", func(t *testing.T) {
		resp, _ := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
			GroupName: "nope", NodeId: removeIPLocal, Interface: removeIPIface,
		})
		if resp.Success {
			t.Error("Success = true for a group that does not exist")
		}
	})

	t.Run("a missing node_id is refused", func(t *testing.T) {
		resp, _ := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
			GroupName: "group1", Interface: removeIPIface,
		})
		if resp.Success {
			t.Error("Success = true with no node_id")
		}
	})

	t.Run("unassigning twice is success", func(t *testing.T) {
		for i := range 2 {
			resp, err := s.UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
				GroupName: "group1", NodeId: removeIPLocal, Interface: removeIPIface,
			})
			if err != nil || !resp.Success {
				t.Fatalf("call %d: Success = %v (%q), err = %v", i+1, resp.Success, resp.Message, err)
			}
		}
	})
}
