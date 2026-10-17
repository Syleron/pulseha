package server

import (
	"context"
	"strings"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// A Witness can never become Active, and forcing it does not change that.
func TestPromoteRefusesTheWitness(t *testing.T) {
	for _, force := range []bool{false, true} {
		s := newVotingServer(t, "a")
		s.config.Nodes["c"].Witness = true
		s.memberList.UpdateConfig(s.config)

		resp, err := s.Promote(context.Background(), &rpc.PromoteRequest{NodeId: "c", ForceDemote: force})
		if err != nil || resp.Success || !strings.Contains(resp.Message, "Witness") {
			t.Fatalf("Promote(witness, force=%v) = %+v, %v; want a refusal naming the Witness", force, resp, err)
		}
		if got := s.memberList.GetMemberByID("c").GetStatus(); got != membership.StatusWitness {
			t.Fatalf("Witness status after refused promotion = %s", membership.StatusToString(got))
		}
	}
}

// Status carries the role, so a consumer can tell a Witness that is down from a
// node that has failed.
func TestStatusReportsTheWitnessRole(t *testing.T) {
	s := newVotingServer(t, "a")
	s.config.Nodes["c"].Witness = true
	s.memberList.UpdateConfig(s.config)
	s.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)

	resp, err := s.GetClusterStatus(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range resp.Members {
		if want := m.NodeId == "c"; m.Witness != want {
			t.Errorf("member %s witness = %v, want %v", m.NodeId, m.Witness, want)
		}
	}
}
