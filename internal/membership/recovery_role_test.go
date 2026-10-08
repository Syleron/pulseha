package membership

import (
	"testing"

	"github.com/syleron/pulseha/rpc"
)

// A coordinator's Unknown gossip can reach observers that still reach the
// incumbent. Recovery must report that incumbent, not manufacture a demotion.
func TestRecoveryUsesReportedRoleWithoutElecting(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, role := range []MemberStatus{StatusActive, StatusPassive, StatusMaintenance, StatusUnknown} {
			h, peer, _ := newLatchTestChecker(t)
			h.members.Config().Pulse.AutoFailback = auto
			h.members.GetMemberByID("node-local").SetStatus(StatusPassive)
			peer.SetStatus(StatusUnknown)
			h.deepCheck = nil
			servePeer(t, peer, &rpc.HealthCheckResponse{Success: true, ClusterToken: "token-a", NodeId: peer.ID, Status: rpc.MemberStatusEnum(role)})
			h.deepCheckCounter = 4
			h.performHealthChecks()
			if got := peer.GetStatus(); got != role {
				t.Fatalf("auto=%v report=%v: got %v", auto, role, got)
			}
		}
	}
}

func TestRecoveryDoesNotInferRoleFromTCPOrLegacyReply(t *testing.T) {
	for _, identity := range []string{"", "wrong-node"} {
		h, peer, _ := newLatchTestChecker(t)
		h.members.Config().Pulse.AutoFailback = true
		h.deepCheck = nil
		peer.SetStatus(StatusUnknown)
		servePeer(t, peer, &rpc.HealthCheckResponse{Success: true, ClusterToken: "token-a", NodeId: identity, Status: rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE})
		// First a cheap TCP check, then the scheduled RPC. Neither reply provides
		// an identified incumbent, so neither can invent Active or Passive.
		h.performHealthChecks()
		if peer.GetStatus() != StatusUnknown {
			t.Fatal("TCP reachability assigned a role")
		}
		h.deepCheckCounter = 4
		h.performHealthChecks()
		if peer.GetStatus() != StatusUnknown {
			t.Fatal("unidentified role accepted")
		}
	}
}

func TestRecoveryDoesNotOverwriteConcurrentRoleChange(t *testing.T) {
	for _, role := range []MemberStatus{StatusActive, StatusPassive, StatusMaintenance} {
		h, peer, _ := newLatchTestChecker(t)
		h.members.Config().Pulse.AutoFailback = true
		peer.SetStatus(StatusUnknown)
		h.deepCheck = func(m *Member) (membershipVerdict, MemberStatus) {
			m.SetStatus(role)
			return membershipConfirmed, StatusActive
		}
		h.deepCheckCounter = 4
		h.performHealthChecks()
		if peer.GetStatus() != role {
			t.Fatalf("overwrote role %v during probe", role)
		}
	}
}
