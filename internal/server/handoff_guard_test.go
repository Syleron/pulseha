package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// newHandoffCluster serves a four-node active-passive cluster with a Active.
// a is also the lowest ID, so once demoted it is the election coordinator:
// the shape #266 was reproduced on. Groups are empty so MakePassive releases
// nothing on the test host.
func newHandoffCluster(t *testing.T) map[string]*Server {
	t.Helper()
	servers := map[string]*Server{}
	addresses := map[string]string{}
	for _, id := range []string{"a", "b", "c", "d"} {
		s := newVotingServer(t, id)
		s.config.Pulse.Mode = "active-passive"
		s.config.Groups = nil
		s.config.Nodes["d"] = &config.Node{Hostname: "d", IP: "127.0.0.1", Port: "1"}
		if err := s.memberList.AddMember("d", "d", "127.0.0.1", "1"); err != nil {
			t.Fatal(err)
		}
		s.memberList.GetMemberByID("d").SetStatus(membership.StatusPassive)
		s.memberList.GetMemberByID("a").SetStatus(membership.StatusActive)
		s.quorumManager.UpdateNodeCount(4)
		servers[id] = s
		addresses[id] = serveVoter(t, s)
	}
	for _, s := range servers {
		for id, addr := range addresses {
			setVoterAddress(t, s, id, addr)
		}
	}
	return servers
}

func runElection(t *testing.T, s *Server, candidate string) bool {
	t.Helper()
	id, err := s.quorumManager.StartVotingSession(quorum.VoteTypeNodeStatus, candidate, "elect "+candidate, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.BroadcastVoteRequest(id, "node_status", candidate, "elect "+candidate, 3)
	result, _ := s.quorumManager.GetVotingSession(id)
	return result.Result != nil && result.Result.Passed
}

// #266: c promotes itself over a. Once a has released and every member sees
// it Passive, nothing looks Active until c broadcasts — and a, as coordinator,
// must not win an election in that gap, for itself or for anybody else.
func TestSelfPromotionHandoffBlocksCoordinatorElection(t *testing.T) {
	servers := newHandoffCluster(t)
	a, c := servers["a"], servers["c"]

	endHandoff := c.beginHandoff("c")
	resp, err := c.MakePassive(context.Background(), &rpc.MakePassiveRequest{NodeId: "a", HandoffTo: "c"})
	if err != nil || !resp.Success {
		t.Fatalf("demotion over RPC: %+v %v", resp, err)
	}
	if a.memberList.GetMemberByID("a").GetStatus() != membership.StatusPassive {
		t.Fatal("a did not demote itself")
	}
	if got := a.handoffInProgress(); got != "c" {
		t.Fatalf("demoted node does not hold the handoff guard: %q", got)
	}
	for _, s := range servers {
		s.memberList.GetMemberByID("a").SetStatus(membership.StatusPassive)
	}

	for _, candidate := range []string{"a", "b", "c"} {
		if runElection(t, a, candidate) {
			t.Fatalf("election for %s passed during the handoff", candidate)
		}
	}

	// Negative control: the same cluster elects once the handoff is over, so
	// the refusals above came from the guard and not from the fixture.
	endHandoff()
	a.handoff.mu.Lock()
	a.handoff.until = time.Now()
	a.handoff.mu.Unlock()
	if !runElection(t, a, "a") {
		t.Fatal("election still refused after the handoff ended")
	}
}

func TestDemotedHandoffGuardClearsWhenAnotherMemberTurnsActive(t *testing.T) {
	s := newVotingServer(t, "a")
	s.noteDemotedForHandoff("c")
	// Its own Active status does not count: it is still Active when it notes the guard.
	s.memberList.GetMemberByID("a").SetStatus(membership.StatusActive)
	if s.handoffInProgress() != "c" {
		t.Fatal("guard cleared by the demoted node's own status")
	}
	s.memberList.GetMemberByID("a").SetStatus(membership.StatusPassive)
	s.memberList.GetMemberByID("c").SetStatus(membership.StatusActive)
	if got := s.handoffInProgress(); got != "" {
		t.Fatalf("guard outlived the target turning Active: %q", got)
	}
}

func TestHandoffGuardLapses(t *testing.T) {
	s := newVotingServer(t, "c")
	end := s.beginHandoff("c")
	defer end()
	// The orchestrator keeps its guard while others turn Active; only it ends it.
	s.memberList.GetMemberByID("a").SetStatus(membership.StatusActive)
	if s.handoffInProgress() != "c" {
		t.Fatal("orchestrator guard cleared early")
	}
	s.handoff.mu.Lock()
	s.handoff.until = time.Now()
	s.handoff.mu.Unlock()
	if got := s.handoffInProgress(); got != "" {
		t.Fatalf("guard outlived its window: %q", got)
	}
}

func TestEndingSupersededHandoffKeepsNewerGuard(t *testing.T) {
	s := newVotingServer(t, "c")
	endOld := s.beginHandoff("b")
	endNew := s.beginHandoff("c")
	defer endNew()
	endOld()
	if got := s.handoffInProgress(); got != "c" {
		t.Fatalf("stale handoff cleared its replacement: %q", got)
	}
}

// #266, second race: the demoted node's health check broadcasts "nobody is
// Active" at epoch+1 while the target is still acquiring. The target must not
// demote itself on it, or it strips the addresses it is bringing up.
func TestHandoffTargetIgnoresPeerDemotionOfItself(t *testing.T) {
	for _, receiving := range []bool{true, false} {
		s, ml := newConfigSyncTestServer(t, "target", "previous", "other")
		s.config.Pulse.Mode = "active-passive"
		if receiving {
			end := s.beginHandoff("target")
			defer end()
		}
		ml.GetMemberByID("target").SetStatus(membership.StatusActive)
		payload, _ := json.Marshal(map[string]any{
			"member_states": map[string]int{"target": int(membership.StatusPassive), "previous": int(membership.StatusPassive)},
			"epoch":         s.GetClusterEpoch() + 1})
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatal(err)
		}
		got := ml.GetMemberByID("target").GetStatus()
		// Control: without the handoff the same higher-epoch sync does demote it,
		// so the hold above is the guard's doing.
		want := membership.StatusPassive
		if receiving {
			want = membership.StatusActive
		}
		if got != want {
			t.Fatalf("receiving=%v: local status %s, want %s", receiving,
				membership.StatusToString(got), membership.StatusToString(want))
		}
	}
}
