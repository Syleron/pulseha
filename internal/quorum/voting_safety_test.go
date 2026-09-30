package quorum

import (
	"testing"
	"time"
)

func TestSessionElectorateCannotShrinkOrAcceptUnknownVoters(t *testing.T) {
	q := newQuorumManager(t, 5)
	id, err := q.StartVotingSession(VoteTypeNodeStatus, "node-a", "promote", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	q.UpdateNodeCount(1)
	if err := q.CastVote(id, "outsider", VoteDecisionYes); err == nil {
		t.Fatal("unknown voter accepted")
	}
	for _, v := range []string{"node-a", "node-b"} {
		if err := q.CastVote(id, v, VoteDecisionYes); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := q.GetVotingSession(id)
	if s.Result != nil {
		t.Fatal("minority concluded after membership shrink")
	}
	if err := q.CastVote(id, "node-c", VoteDecisionYes); err != nil {
		t.Fatal(err)
	}
	s, _ = q.GetVotingSession(id)
	if s.Result == nil || !s.Result.Passed {
		t.Fatal("fixed majority failed")
	}
}

func TestSessionRequiresAffirmativeMajority(t *testing.T) {
	q := newQuorumManager(t, 5)
	id, _ := q.StartVotingSession(VoteTypeNodeStatus, "node-a", "promote", time.Hour)
	for _, vote := range []struct {
		id string
		d  VoteDecision
	}{{"node-a", VoteDecisionYes}, {"node-b", VoteDecisionAbstain}, {"node-c", VoteDecisionAbstain}, {"node-d", VoteDecisionAbstain}} {
		if err := q.CastVote(id, vote.id, vote.d); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := q.GetVotingSession(id)
	if s.Result == nil || s.Result.Passed || !s.Result.QuorumMet {
		t.Fatalf("participation mistaken for approval: %+v", s.Result)
	}
}

func TestExpiredAndChangedVotesAreRejected(t *testing.T) {
	q := newQuorumManager(t, 3)
	id, _ := q.StartVotingSession(VoteTypeNodeStatus, "node-a", "promote", time.Hour)
	if err := q.CastVote(id, "node-a", VoteDecisionYes); err != nil {
		t.Fatal(err)
	}
	if err := q.CastVote(id, "node-a", VoteDecisionYes); err != nil {
		t.Fatal("idempotent retry rejected")
	}
	if err := q.CastVote(id, "node-a", VoteDecisionNo); err == nil {
		t.Fatal("vote changed")
	}
	if err := q.CastVote(id, "node-b", VoteDecision("bogus")); err == nil {
		t.Fatal("invalid decision accepted")
	}
	q.Lock()
	q.activeSessions[id].EndTime = time.Now().Add(-time.Second)
	q.Unlock()
	if err := q.CastVote(id, "node-b", VoteDecisionYes); err == nil {
		t.Fatal("late majority accepted")
	}
	s, _ := q.GetVotingSession(id)
	if s.Result == nil || s.Result.Passed || len(s.Votes) != 1 {
		t.Fatalf("expired vote altered outcome: %+v", s)
	}
}

func TestSessionEpochCannotChangeOnRetry(t *testing.T) {
	q := newQuorumManager(t, 3)
	id, err := q.StartVotingSession(VoteTypeNodeStatus, "node-a", "promote", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.BindSessionEpoch(id, 7); err != nil {
		t.Fatal(err)
	}
	if err := q.BindSessionEpoch(id, 7); err != nil {
		t.Fatal("same epoch retry rejected")
	}
	if err := q.BindSessionEpoch(id, 8); err == nil {
		t.Fatal("mixed-epoch ballots allowed")
	}
	s, _ := q.GetVotingSession(id)
	if s.Epoch != 7 {
		t.Fatal("original epoch changed")
	}
	// Returned snapshots must not let a caller rewrite the fixed electorate.
	s.MemberIDs[0] = "outsider"
	if err := q.CastVote(id, "outsider", VoteDecisionYes); err == nil {
		t.Fatal("snapshot mutated electorate")
	}
}

func TestRecoveredProposalCannotReuseExistingVotes(t *testing.T) {
	q := newQuorumManager(t, 3)
	id, err := q.StartVotingSession(VoteTypeNodeStatus, "node-a", "elect", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.RecoverProposal(id, "node-b"); err != nil {
		t.Fatal(err)
	}
	if err = q.CastVote(id, "node-a", VoteDecisionYes); err != nil {
		t.Fatal(err)
	}
	if err = q.RecoverProposal(id, "node-c"); err == nil {
		t.Fatal("mixed ballots for different proposals")
	}
	session, _ := q.GetVotingSession(id)
	if session.Subject != "node-b" {
		t.Fatal("recovered subject changed after a vote")
	}
}
