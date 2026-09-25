package server

import (
	"context"
	"fmt"
	"github.com/syleron/pulseha/internal/quorum"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

func TestPromotionRejectsMinorityDespiteShrunkenManagerCount(t *testing.T) {
	s := newVotingServer(t, "a")
	s.config.Pulse.Mode = "active-passive"
	s.memberList.GetMemberByID("b").SetStatus(membership.StatusUnknown)
	s.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
	s.quorumManager.UpdateNodeCount(1)
	resp, err := s.Promote(context.Background(), &rpc.PromoteRequest{NodeId: "a"})
	if err != nil || resp.Success || resp.Message != "promotion requires a configured majority" {
		t.Fatalf("minority promotion accepted: %+v %v", resp, err)
	}
	// The worker must recheck too, before touching interfaces or changing states.
	s.performPromotionAsync("a", nil, false)
	if s.memberList.GetMemberByID("a").GetStatus() != membership.StatusPassive {
		t.Fatal("minority worker promoted")
	}
	s.memberList.GetMemberByID("b").SetStatus(membership.StatusPassive)
	if !s.hasConfiguredPromotionMajority() {
		t.Fatal("configured majority rejected")
	}
	delete(s.config.Nodes, "c")
	s.memberList.GetMemberByID("b").SetStatus(membership.StatusUnknown)
	if !s.hasConfiguredPromotionMajority() {
		t.Fatal("configured two-node exception lost")
	}
}

// Exercise the actual voting RPC, not reachability arithmetic: a locally
// observed majority does not supply the missing peer's affirmative ballot.
func TestNodeStatusVoteRequiresPeerConsent(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("peer_connected_%v", live), func(t *testing.T) {
			s := newVotingServer(t, "a")
			s.config.Pulse.Mode = "active-passive"
			s.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
			if live {
				peer := newVotingServer(t, "b")
				peer.config.Pulse.Mode = "active-passive"
				peer.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
				setVoterAddress(t, s, "b", serveVoter(t, peer))
			}
			id, err := s.quorumManager.StartVotingSession(quorum.VoteTypeNodeStatus, "a", "elect a", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.BroadcastVoteRequest(id, "node_status", "a", "elect a", 1)
			vote, err := s.quorumManager.GetVotingSession(id)
			if err != nil {
				t.Fatal(err)
			}
			passed := vote.Result != nil && vote.Result.Passed
			if passed != live {
				t.Fatalf("passed=%v, connected=%v", passed, live)
			}
		})
	}
}
