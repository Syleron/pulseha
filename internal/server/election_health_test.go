package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

type electionHealthPeer struct {
	rpc.UnimplementedServerServer
	reply *rpc.HealthCheckResponse
	hang  bool
}

func (p *electionHealthPeer) HealthCheck(ctx context.Context, _ *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	if p.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.reply, nil
}

func TestElectionChecksDirectRoleAfterUnknownGossip(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       rpc.MemberStatusEnum
		identity   string
		hang, want bool
	}{
		{"live incumbent", rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE, "c", false, false},
		{"released incumbent", rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE, "c", false, true},
		{"unknown role", rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN, "c", false, false},
		{"legacy reply", rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE, "", false, false},
		{"wrong identity", rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE, "b", false, false},
		{"wedged daemon", rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE, "c", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newVotingServer(t, "b")
			s.config.Pulse.Mode = "active-passive"
			addr := serveVoter(t, &electionHealthPeer{reply: &rpc.HealthCheckResponse{Success: true, NodeId: tc.identity, ClusterToken: s.config.Pulse.ClusterToken, Status: tc.role}, hang: tc.hang})
			setVoterAddress(t, s, "c", addr)
			// ConfigSync erases the converged role before voting. There is deliberately
			// no cached direct observation: even the startup window must be protected.
			s.memberList.GetMemberByID("c").SetStatus(membership.StatusActive)
			payload, _ := json.Marshal(map[string]any{"member_states": map[string]int{"c": 0}, "epoch": 1, "leader_id": "a"})
			if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
				t.Fatal(err)
			}
			if s.memberList.GetMemberByID("c").GetStatus() != membership.StatusUnknown {
				t.Fatal("test did not overwrite incumbent through gossip")
			}
			r := voteRequest(s)
			r.Epoch = s.GetClusterEpoch() + 1
			r.VoteType = "node_status"
			r.Subject = "a"
			response, err := preparedTestVote(s, r)
			if err != nil || response.Granted != tc.want {
				t.Fatalf("vote=%+v err=%v want granted=%v", response, err, tc.want)
			}
			if tc.name == "live incumbent" && !strings.Contains(response.Reason, "directly reports Active") {
				t.Fatalf("wrong rejection: %+v", response)
			}
		})
	}
}

func TestAsymmetricPartitionCannotObtainElectionMajority(t *testing.T) {
	servers := map[string]*Server{}
	addresses := map[string]string{}
	for _, id := range []string{"a", "b", "c", "d"} {
		s := newVotingServer(t, id)
		s.config.Pulse.Mode = "active-passive"
		s.config.Nodes["d"] = &config.Node{Hostname: "d", IP: "127.0.0.1", Port: "1"}
		if err := s.memberList.AddMember("d", "d", "127.0.0.1", "1"); err != nil {
			t.Fatal(err)
		}
		s.memberList.GetMemberByID("d").SetStatus(membership.StatusPassive)
		s.quorumManager.UpdateNodeCount(4)
		servers[id] = s
		addresses[id] = serveVoter(t, s)
	}
	for _, s := range servers {
		for id, addr := range addresses {
			setVoterAddress(t, s, id, addr)
		}
	}
	// Only the coordinator cannot reach the incumbent; the other voters can.
	servers["a"].config.Nodes["c"].Port = "1"
	servers["c"].memberList.GetMemberByID("c").SetStatus(membership.StatusActive)
	for _, s := range servers {
		s.memberList.GetMemberByID("c").ObserveRole(membership.StatusActive, time.Now())
		payload, _ := json.Marshal(map[string]any{"member_states": map[string]int{"c": 0}, "epoch": 1, "leader_id": "a"})
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatal(err)
		}
	}
	// Its own failed checks can mark the incumbent Unknown. Local evidence at the
	// coordinator has expired; the independent observations at b/d have not.
	servers["a"].memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
	servers["a"].memberList.GetMemberByID("c").ObserveRole(membership.StatusUnknown, time.Now())
	s := servers["a"]
	run := func(want bool) {
		id, err := s.quorumManager.StartVotingSession(quorum.VoteTypeNodeStatus, "a", "elect a", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		err = s.BroadcastVoteRequest(id, "node_status", "a", "elect a", 3)
		result, _ := s.quorumManager.GetVotingSession(id)
		passed := result.Result != nil && result.Result.Passed
		if passed != want {
			t.Fatalf("passed=%v want=%v err=%v result=%+v", passed, want, err, result.Result)
		}
	}
	run(false)
	// Genuine failure: all surviving voters lose the socket and their direct
	// Active evidence has been superseded. The configured majority can elect.
	for _, s := range servers {
		s.config.Nodes["c"].Port = "1"
		s.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
		s.memberList.GetMemberByID("c").ObserveRole(membership.StatusUnknown, time.Now())
	}
	run(true)
}

func TestUnknownGossipCannotEraseDirectActiveEvidence(t *testing.T) {
	s, ml := newConfigSyncTestServer(t, "observer", "owner", "coordinator")
	s.config.Pulse.FailOverLimit = 10000
	m := ml.GetMemberByID("owner")
	m.SetStatus(membership.StatusActive)
	m.ObserveRole(membership.StatusActive, time.Now())
	for epoch := 1; epoch <= 3; epoch++ {
		payload, _ := json.Marshal(map[string]any{"member_states": map[string]int{"owner": 0}, "epoch": epoch, "leader_id": "coordinator"})
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatal(err)
		}
		if m.GetStatus() != membership.StatusActive {
			t.Fatal("gossip erased directly observed Active")
		}
	}
}

func TestElectionRetainsRecentActiveAcrossTransportFailure(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		s := newVotingServer(t, "b")
		m := s.memberList.GetMemberByID("c")
		at := time.Now()
		if !fresh {
			at = at.Add(-2 * time.Second)
		}
		m.ObserveRole(membership.StatusActive, at)
		m.SetStatus(membership.StatusUnknown)
		r := voteRequest(s)
		r.VoteType = "node_status"
		r.Subject = "a"
		response, err := preparedTestVote(s, r)
		if err != nil || response.Granted == fresh {
			t.Fatalf("fresh=%v response=%+v err=%v", fresh, response, err)
		}
	}
}

func TestElectionProbeHonorsCallerDeadline(t *testing.T) {
	s := newVotingServer(t, "b")
	setVoterAddress(t, s, "c", serveVoter(t, &electionHealthPeer{hang: true}))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.checkElectionIncumbents(ctx, s.config, "a"); err == nil {
		t.Fatal("cancelled probe allowed vote")
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("caller deadline ignored")
	}
}
