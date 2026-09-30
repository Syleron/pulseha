package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func nodeProposal(s *Server, subject string, n uint64) *rpc.RequestVoteRequest {
	r := voteRequest(s)
	r.InitiatorId = s.config.Pulse.LocalNode
	r.VoteType = "node_status"
	r.Subject = subject
	r.Ballot = n
	return r
}
func linkVoters(t *testing.T, peers ...*Server) {
	for _, peer := range peers {
		addr := serveVoter(t, peer)
		for _, s := range peers {
			setVoterAddress(t, s, peer.config.Pulse.LocalNode, addr)
		}
	}
}
func runNodeRound(t *testing.T, s *Server, subject string) (*quorum.VotingSession, error) {
	t.Helper()
	id, err := s.quorumManager.StartVotingSession(quorum.VoteTypeNodeStatus, subject, "elect", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = s.BroadcastVoteRequest(id, "node_status", subject, "elect", 3)
	session, getErr := s.quorumManager.GetVotingSession(id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	return session, err
}

func TestSplitVoteRecoversHighestAcceptedCandidate(t *testing.T) {
	peers := []*Server{newVotingServer(t, "a"), newVotingServer(t, "b"), newVotingServer(t, "c")}
	for _, s := range peers {
		r := nodeProposal(s, s.config.Pulse.LocalNode, 1)
		r.ExpiresAtUnixMilli = time.Now().Add(30 * time.Millisecond).UnixMilli()
		resp, err := preparedTestVote(s, r)
		if err != nil || !resp.Granted {
			t.Fatalf("split acceptance: %v %v", resp, err)
		}
	}
	time.Sleep(35 * time.Millisecond)
	linkVoters(t, peers...)
	// A higher round can recover any highest acceptance in its prepare majority,
	// but must return that actual candidate and preserve it on subsequent rounds.
	first, err := runNodeRound(t, peers[0], "a")
	if err != nil || first.Result == nil || !first.Result.Passed {
		t.Fatalf("split did not recover: %+v %v", first, err)
	}
	second, err := runNodeRound(t, peers[1], "b")
	if err != nil { // A prepare preempted by a higher counter learns it; retry.
		second, err = runNodeRound(t, peers[1], "b")
	}
	if err != nil || second.Subject != first.Subject || second.Result == nil || !second.Result.Passed {
		t.Fatalf("recovery changed chosen value %s: %+v %v", first.Subject, second, err)
	}
}

type lostAcceptReply struct {
	rpc.UnimplementedServerServer
	server *Server
	drop   atomic.Bool
}

func (v *lostAcceptReply) RequestVote(ctx context.Context, r *rpc.RequestVoteRequest) (*rpc.RequestVoteResponse, error) {
	resp, err := v.server.RequestVote(ctx, r)
	if err == nil && resp.Granted && r.Phase == "accept" && v.drop.Load() {
		return nil, status.Error(codes.Unavailable, "reply lost after durable acceptance")
	}
	return resp, err
}
func TestLostMajorityRepliesRecoveredAfterRestart(t *testing.T) {
	a, b, c := newVotingServer(t, "a"), newVotingServer(t, "b"), newVotingServer(t, "c")
	for _, peer := range []*Server{b, c} {
		wrapper := &lostAcceptReply{server: peer}
		wrapper.drop.Store(true)
		setVoterAddress(t, a, peer.config.Pulse.LocalNode, serveVoter(t, wrapper))
	}
	first, err := runNodeRound(t, a, "a")
	if err == nil || first.Result != nil && first.Result.Passed {
		t.Fatal("lost acknowledgements counted as votes")
	}
	// All acceptors restart from their durable files. No applied cluster state
	// was advanced, and the successful majority was never seen by the caller.
	var restarted []*Server
	for _, old := range []*Server{a, b, c} {
		next := newVotingServer(t, old.config.Pulse.LocalNode)
		next.voteStatePath = old.voteStatePath
		restarted = append(restarted, next)
	}
	linkVoters(t, restarted...)
	recovered, err := runNodeRound(t, restarted[1], "b")
	if err != nil || recovered.Result == nil || !recovered.Result.Passed || recovered.Subject != "a" {
		t.Fatalf("lost majority overwritten after restart: %+v %v", recovered, err)
	}
	// Even with an unexpired deadline, the old accept ballot is fenced by the
	// higher promise on an intersecting majority.
	rejects := 0
	for _, s := range restarted {
		old := nodeProposal(restarted[0], "b", 1)
		resp, err := s.RequestVote(context.Background(), old)
		if err != nil || !resp.Granted {
			rejects++
		}
	}
	if rejects < 2 {
		t.Fatalf("old ballot not fenced: %d rejections", rejects)
	}
}

func TestVoteStateFailuresFailClosed(t *testing.T) {
	for _, fault := range []string{"corrupt", "unwritable"} {
		t.Run(fault, func(t *testing.T) {
			s := newVotingServer(t, "a")
			if fault == "corrupt" {
				if err := os.WriteFile(s.voteStatePath, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				parent := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				s.voteStatePath = filepath.Join(parent, "votes.json")
			}
			r := nodeProposal(s, "a", 1)
			r.Phase = "prepare"
			resp, err := s.RequestVote(context.Background(), r)
			if err == nil && resp.Granted {
				t.Fatal("granted without durable promise")
			}
		})
	}
}
func TestPromisesSurviveRestartAndOldBallotsStayRejected(t *testing.T) {
	s := newVotingServer(t, "b")
	r := nodeProposal(s, "a", 7)
	r.Phase = "prepare"
	resp, err := s.RequestVote(context.Background(), r)
	if err != nil || !resp.Granted {
		t.Fatal(resp, err)
	}
	next := newVotingServer(t, "b")
	next.voteStatePath = s.voteStatePath
	r.Ballot = 6
	r.Phase = "accept"
	resp, err = next.RequestVote(context.Background(), r)
	if err != nil || resp.Granted || resp.PromisedBallot != 7 {
		t.Fatalf("durable promise lost: %v %v", resp, err)
	}
	info, err := os.Stat(s.voteStatePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
}
func TestConcurrentRecoveryRoundsCannotChooseDifferentCandidates(t *testing.T) {
	peers := []*Server{newVotingServer(t, "a"), newVotingServer(t, "b"), newVotingServer(t, "c")}
	linkVoters(t, peers...)
	type result struct {
		value string
		err   error
	}
	results := make(chan result, 3)
	for _, p := range peers {
		go func(s *Server) {
			session, err := runNodeRound(t, s, s.config.Pulse.LocalNode)
			results <- result{session.Subject, err}
		}(p)
	}
	chosen := ""
	for range peers {
		r := <-results
		if r.err == nil {
			if chosen != "" && r.value != chosen {
				t.Fatalf("conflicting decisions %s/%s", chosen, r.value)
			}
			chosen = r.value
		}
	}
	// Once contention stops, a stable proposer must recover in bounded rounds.
	for i := 0; i < 3; i++ {
		session, err := runNodeRound(t, peers[0], "a")
		if err == nil {
			if chosen != "" && session.Subject != chosen {
				t.Fatalf("overwrote %s with %s", chosen, session.Subject)
			}
			return
		}
	}
	t.Fatal(fmt.Sprintf("stable proposer failed to recover; prior choice %q", chosen))
}

func TestOldProtocolAndChangedElectorateCannotVote(t *testing.T) {
	s := newVotingServer(t, "a")
	r := nodeProposal(s, "a", 1)
	r.Ballot = 0
	r.Phase = ""
	resp, err := s.RequestVote(context.Background(), r)
	if err == nil && resp.Granted {
		t.Fatal("legacy request granted")
	}
	r.Ballot = 1
	r.Phase = "prepare"
	resp, err = s.RequestVote(context.Background(), r)
	if err != nil || !resp.Granted {
		t.Fatal(resp, err)
	}
	// A new electorate must not reuse the same slot, even if it matches the
	// latest config. Membership consensus itself remains a separate concern.
	s.config.Nodes["d"] = s.config.Nodes["c"]
	_ = s.memberList.AddMember("d", "d", "127.0.0.1", "1")
	r.MemberIds = append(r.MemberIds, "d")
	r.Ballot = 2
	resp, err = s.RequestVote(context.Background(), r)
	if err == nil && resp.Granted {
		t.Fatal("changed electorate reused an epoch")
	}
}

func TestCredentialRotationDoesNotForgetAcceptedVote(t *testing.T) {
	s := newVotingServer(t, "a")
	resp, err := preparedTestVote(s, nodeProposal(s, "a", 1))
	if err != nil || !resp.Granted {
		t.Fatal(resp, err)
	}
	next := newVotingServer(t, "a")
	next.voteStatePath = s.voteStatePath
	next.config.Pulse.ClusterToken = "rotated"
	r := nodeProposal(next, "b", 2)
	r.Phase = "prepare"
	resp, err = next.RequestVote(context.Background(), r)
	if err != nil || !resp.Granted || resp.AcceptedSubject != "a" {
		t.Fatalf("credential rotation forgot acceptance: %v %v", resp, err)
	}
}

func TestRestartEpochFloorAppliesToConvergence(t *testing.T) {
	s := newVotingServer(t, "a")
	s.clusterEpoch = 9
	r := nodeProposal(s, "a", 1)
	r.Epoch = 10
	r.Phase = "prepare"
	resp, err := s.RequestVote(context.Background(), r)
	if err != nil || !resp.Granted {
		t.Fatal(resp, err)
	}
	next := newVotingServer(t, "a")
	next.voteStatePath = s.voteStatePath
	epoch, err := next.votingEpoch(next.config)
	if err != nil || epoch != 10 {
		t.Fatalf("lost epoch after restart: %d %v", epoch, err)
	}
	held, _ := next.convergenceMetadata()
	if held != 9 {
		t.Fatalf("broadcast would advance from stale raw epoch %d", held)
	}
	if next.adoptConvergenceMetadata(1, "b", true) {
		t.Fatal("accepted metadata older than durable floor")
	}
	if !next.adoptConvergenceMetadata(10, "a", false) {
		t.Fatal("recovered decision could not advance convergence")
	}
}
