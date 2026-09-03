package quorum

import (
	"io"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// This package had no tests at all until END-2339, which is how it came to be
// the one place a reintroduced deadlock went unnoticed. QuorumManager embeds an
// RWMutex, hasQuorumLocked is one of the nine historical shapes, and the rule it
// implements is what ADR-0002 rests on — none of it was pinned by anything.
//
// Found by mutation rather than by reading: making CastVote and
// concludeVotingSessionLocked call the locking HasQuorum instead of
// hasQuorumLocked — which is exactly the deadlock the method's own comment warns
// about — changed nothing observable, because `go test ./internal/quorum/`
// printed "no test files".

func newQuorumManager(t *testing.T, nodes int) *QuorumManager {
	t.Helper()

	cfg := &config.Config{Nodes: map[string]*config.Node{}}
	for i := 0; i < nodes; i++ {
		id := string(rune('a' + i))
		cfg.Nodes[id] = &config.Node{Hostname: id}
	}
	return NewQuorumManager(cfg, log.New(io.Discard))
}

// TestHasQuorumFromAWriteLockHolderDoesNotWedge is the historical shape.
//
// hasQuorumLocked exists only because HasQuorum takes the read lock, and
// RWMutex is not reentrant: a caller already holding the write lock — CastVote,
// concludeVotingSessionLocked — that reached for HasQuorum would queue behind
// its own write lock and never be granted. The lock is then held forever, and
// every other quorum operation on this node queues behind it.
//
// Asserted on a timeout, because a deadlock hangs the package rather than
// failing it. With pulselock in place the mutation panics instead, which is
// faster and names the site — but the timeout is what makes this test correct
// regardless of whether the mechanism is present.
func TestHasQuorumFromAWriteLockHolderDoesNotWedge(t *testing.T) {
	q := newQuorumManager(t, 3)

	done := make(chan bool, 1)
	go func() {
		q.Lock()
		got := q.hasQuorumLocked(2)
		q.Unlock()
		done <- got
	}()

	select {
	case got := <-done:
		if !got {
			t.Error("2 of 3 should meet quorum")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("evaluating quorum under the write lock did not return — " +
			"it retook a non-reentrant lock")
	}
}

// TestHasQuorumAndHasQuorumLockedAgreeOnEveryCount pins the two entry points to
// one rule, which is what the naming convention promises and nothing checked.
func TestHasQuorumAndHasQuorumLockedAgreeOnEveryCount(t *testing.T) {
	for _, nodes := range []int{0, 1, 2, 3, 4, 5, 7} {
		q := newQuorumManager(t, nodes)
		for votes := 0; votes <= nodes+1; votes++ {
			outside := q.HasQuorum(votes)

			q.Lock()
			inside := q.hasQuorumLocked(votes)
			q.Unlock()

			if outside != inside {
				t.Errorf("nodes=%d votes=%d: HasQuorum=%v hasQuorumLocked=%v — "+
					"the two entry points disagree", nodes, votes, outside, inside)
			}
		}
	}
}

// TestQuorumIsWaivedBelowThreeNodes is ADR-0002's load-bearing rule, and it had
// no test.
//
// A two-node cluster has no majority to appeal to: each side reaches exactly one
// of two, so a rule requiring a majority produces a cluster that fails closed on
// every heartbeat glitch. So quorum is waived below three nodes, which lets a
// partitioned node promote itself and keep the Floating IP Group reachable.
//
// ADR-0002 says a reader who finds this without the document "will read it as an
// oversight and close it". This test is the other half of that defence: closing
// it fails here, by name.
func TestQuorumIsWaivedBelowThreeNodes(t *testing.T) {
	for _, nodes := range []int{0, 1, 2} {
		q := newQuorumManager(t, nodes)
		// Zero votes, which is the least favourable case there is.
		if !q.HasQuorum(0) {
			t.Errorf("nodes=%d: quorum must be waived below three nodes "+
				"(docs/adr/0002-two-node-availability-over-safety.md) — a two-node "+
				"cluster that fails closed cannot fail over at all", nodes)
		}
	}
}

// TestQuorumNeedsAMajorityFromThreeNodesUp is the mirror, and the reason the
// waiver above is a deliberate exception rather than the rule. At three or more
// a majority exists, so split-brain is a defect rather than accepted behaviour.
func TestQuorumNeedsAMajorityFromThreeNodesUp(t *testing.T) {
	for _, tc := range []struct {
		nodes, votes int
		want         bool
	}{
		{nodes: 3, votes: 0, want: false},
		{nodes: 3, votes: 1, want: false},
		{nodes: 3, votes: 2, want: true},
		{nodes: 3, votes: 3, want: true},
		{nodes: 4, votes: 2, want: false}, // half is not a majority
		{nodes: 4, votes: 3, want: true},
		{nodes: 5, votes: 2, want: false},
		{nodes: 5, votes: 3, want: true},
	} {
		q := newQuorumManager(t, tc.nodes)
		if got := q.HasQuorum(tc.votes); got != tc.want {
			t.Errorf("nodes=%d votes=%d: HasQuorum=%v, want %v",
				tc.nodes, tc.votes, got, tc.want)
		}
	}
}

// TestUpdateNodeCountChangesTheThreshold covers the write path, and with it the
// transition that matters: a cluster growing from two nodes to three stops
// having quorum waived, which is the moment split-brain goes from accepted to
// defective.
func TestUpdateNodeCountChangesTheThreshold(t *testing.T) {
	q := newQuorumManager(t, 2)

	if !q.HasQuorum(0) {
		t.Fatal("a two-node cluster should have quorum waived")
	}

	q.UpdateNodeCount(3)

	if q.HasQuorum(1) {
		t.Error("after growing to three nodes, one vote must not meet quorum")
	}
	if !q.HasQuorum(2) {
		t.Error("after growing to three nodes, two votes must meet quorum")
	}
}

// TestConcurrentQuorumReadsAndCountUpdates gives -race both sides to pair up.
// nodeCount is read by every quorum decision and written by UpdateNodeCount,
// which runs whenever the cluster gains or loses a member.
func TestConcurrentQuorumReadsAndCountUpdates(t *testing.T) {
	q := newQuorumManager(t, 3)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			q.UpdateNodeCount(3 + i%3)
		}
	}()
	for i := 0; i < 500; i++ {
		_ = q.HasQuorum(2)
	}
	<-done
}

// TestCastVoteConcludesASessionWithoutWedging drives the real callers, and it
// is the test the ones above were missing.
//
// hasQuorumLocked's own comment names its two internal callers — CastVote and
// concludeVotingSessionLocked — and both hold the write lock when they reach it.
// Taking the lock in a test and calling hasQuorumLocked directly proves only
// that the method does not lock; it never enters either caller, so making either
// of them call the locking HasQuorum instead went undetected.
//
// Two Yes votes on a three-node cluster walks both sites: CastVote records the
// second vote, canConcludeVoting reaches hasQuorumLocked(yesCount) and returns
// true, and concludeVotingSessionLocked then reaches hasQuorumLocked(totalVotes)
// to decide whether quorum was met. All of it inside one write-locked region.
//
// Asserted on a timeout, because a deadlock hangs the package rather than
// failing it.
func TestCastVoteConcludesASessionWithoutWedging(t *testing.T) {
	q := newQuorumManager(t, 3)

	sessionID, err := q.StartVotingSession(VoteTypeNodeStatus, "node-b",
		"promote node-b", 5*time.Second)
	if err != nil {
		t.Fatalf("StartVotingSession: %v", err)
	}

	// One vote must not conclude a three-node session: two is the majority, and
	// two Yes remain possible, so neither outcome is settled yet.
	castOrFail(t, q, sessionID, "node-a", VoteDecisionYes)
	if s, err := q.GetVotingSession(sessionID); err != nil {
		t.Fatalf("GetVotingSession after one vote: %v", err)
	} else if s.Result != nil {
		t.Fatalf("one vote of three concluded the session: %+v", s.Result)
	}

	// The second Yes reaches quorum, which walks both hasQuorumLocked sites.
	castOrFail(t, q, sessionID, "node-b", VoteDecisionYes)

	s, err := q.GetVotingSession(sessionID)
	if err != nil {
		t.Fatalf("GetVotingSession after two votes: %v", err)
	}
	if s.Result == nil {
		t.Fatal("two Yes votes of three did not conclude the session")
	}
	if !s.Result.Passed {
		t.Errorf("Passed = false, want true (2 yes, 0 no, quorum met): %+v", s.Result)
	}
	if !s.Result.QuorumMet {
		t.Errorf("QuorumMet = false, want true at 2 votes of 3: %+v", s.Result)
	}
	if s.Result.YesCount != 2 || s.Result.TotalVotes != 2 {
		t.Errorf("YesCount=%d TotalVotes=%d, want 2 and 2", s.Result.YesCount, s.Result.TotalVotes)
	}
}

// TestCastVoteConcludesAFailingSessionWithoutWedging is the other arm through
// the same two sites: enough No votes to settle the outcome. Worth covering
// separately because canConcludeVoting reaches its NO-side arithmetic only when
// the YES side has not already returned true, so the Yes test above never
// executes it.
func TestCastVoteConcludesAFailingSessionWithoutWedging(t *testing.T) {
	q := newQuorumManager(t, 3)

	sessionID, err := q.StartVotingSession(VoteTypeNodeStatus, "node-c",
		"promote node-c", 5*time.Second)
	if err != nil {
		t.Fatalf("StartVotingSession: %v", err)
	}

	castOrFail(t, q, sessionID, "node-a", VoteDecisionNo)
	castOrFail(t, q, sessionID, "node-b", VoteDecisionNo)

	s, err := q.GetVotingSession(sessionID)
	if err != nil {
		t.Fatalf("GetVotingSession: %v", err)
	}
	if s.Result == nil {
		t.Fatal("two No votes of three did not conclude the session")
	}
	if s.Result.Passed {
		t.Errorf("Passed = true, want false (0 yes, 2 no): %+v", s.Result)
	}
}

// castOrFail casts a vote and bounds how long it may take, so a deadlock inside
// CastVote fails this test rather than hanging the whole package.
func castOrFail(t *testing.T, q *QuorumManager, sessionID, voterID string, d VoteDecision) {
	t.Helper()

	errCh := make(chan error, 1)
	go func() { errCh <- q.CastVote(sessionID, voterID, d) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("CastVote(%s, %s): %v", voterID, d, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("CastVote(%s, %s) did not return — it took a lock it already held",
			voterID, d)
	}
}

// TestASessionConcludesWhenEveryNodeHasVoted covers the scenario where every
// node has voted and the result is still not a pass.
//
// It does NOT pin canConcludeVoting's all-voted arm, and the reason is worth
// recording, because two drafts of this test tried to and the second one taught
// me why it cannot be done.
//
// The first draft assumed three abstentions of three would need that arm. They
// do not: at two abstentions, `yesCount + remainingPossibleYes` is 0+1 against a
// minVotes of 2, so the "Yes can no longer win" arm concludes the session before
// a third node votes.
//
// The second draft moved to four nodes voting yes/no/yes/no, so that no
// arithmetic arm fires until the last vote. Neutering the all-voted arm still
// changed nothing — and that is because the arm is **redundant**. Once every
// node has voted, remainingPossibleYes is 0, so the remaining two arms are
// exhaustive: either yesCount reaches minVotes and the YES arm concludes, or it
// cannot and `yesCount + 0 < minVotes` concludes. There is no state in which
// "everyone has voted" is the only arm that can settle the outcome.
//
// So the arm is belt-and-braces rather than load-bearing, no test can
// discriminate it, and pretending otherwise would be the empty-coverage fault
// this branch has already objected to twice. What is worth keeping is below: a
// fully-voted session that falls short of a majority must conclude, and must
// conclude as a failure.
func TestASessionConcludesWhenEveryNodeHasVoted(t *testing.T) {
	q := newQuorumManager(t, 4)

	sessionID, err := q.StartVotingSession(VoteTypeNodeStatus, "node-d",
		"promote node-d", 5*time.Second)
	if err != nil {
		t.Fatalf("StartVotingSession: %v", err)
	}

	// The first three settle nothing: two Yes is short of the three-vote
	// majority, and a fourth Yes would still reach it.
	for _, v := range []struct {
		voter    string
		decision VoteDecision
	}{
		{"node-a", VoteDecisionYes},
		{"node-b", VoteDecisionNo},
		{"node-c", VoteDecisionYes},
	} {
		castOrFail(t, q, sessionID, v.voter, v.decision)
		s, err := q.GetVotingSession(sessionID)
		if err != nil {
			t.Fatalf("GetVotingSession after %s: %v", v.voter, err)
		}
		if s.Result != nil {
			t.Fatalf("the session concluded after %s voted, before every node had: %+v",
				v.voter, s.Result)
		}
	}

	castOrFail(t, q, sessionID, "node-d", VoteDecisionNo)

	s, err := q.GetVotingSession(sessionID)
	if err != nil {
		t.Fatalf("GetVotingSession: %v", err)
	}
	if s.Result == nil {
		t.Fatal("a session every node had voted in never concluded — nobody is left " +
			"to vote, so it would stay open until it timed out")
	}
	if s.Result.TotalVotes != 4 {
		t.Errorf("TotalVotes = %d, want 4", s.Result.TotalVotes)
	}
	// Two Yes of four is short of the three-vote majority, so it must not pass.
	if s.Result.Passed {
		t.Errorf("Passed = true on 2 yes / 2 no of four, want false: %+v", s.Result)
	}
}
