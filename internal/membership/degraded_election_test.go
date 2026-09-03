package membership

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/quorum"
)

// END-2325's tie-break is reached through attemptVotingElection, and this is the
// coverage the ticket asked for and TestTwoNodeTiebreakDecidesOnTheSubject does
// not provide: that one calls initiateNodeStatusVote directly, so it pins the
// decision but proves nothing about whether a degraded cluster ever arrives at it.
//
// Reachability is not obvious, because the two gates count different things.
// attemptVotingElection needs three or more members that are Passive **or
// Unknown**; the tie-break branch needs exactly two that are Active **or
// Passive**. Only a degraded cluster satisfies both at once, which is why the
// branch was once mistaken for dead code. These tests build the two shapes that
// do -- the Active-failed-two-Passives-remain three-node case and the two-of-four
// Unknown case -- drive the real entry point, and check the branch was actually
// entered rather than assuming it.
//
// The third test is the other half, and the one that matters most: a genuine
// two-node cluster must never arrive here. Electing a single owner by node ID
// means deciding from a node that cannot see its peer, so the winner may be the
// one whose service network is the broken half -- a duplicated address becomes a
// dark one. See docs/adr/0002-two-node-availability-over-safety.md.

// logSink collects log output while the code under test is still writing to it.
// charmbracelet/log does not serialise writes, and attemptVotingElection runs the
// vote on its own goroutine.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// tiebreakEntered is the line initiateNodeStatusVote logs on entering the
// two-available-node branch. Matching on it is what makes these tests about
// reachability rather than about the verdict.
const tiebreakEntered = "Exactly 2 nodes available"

// newVotingChecker builds a checker whose stub carries a real QuorumManager,
// because attemptVotingElection skips the vote when GetQuorumManager returns nil
// and would otherwise measure the fallback path instead.
func newVotingChecker(t *testing.T, localID string, members ...*Member) (*HealthChecker, *logSink) {
	t.Helper()

	h, stub := newAPTestChecker(localID, members...)
	sink := &logSink{}
	h.logger = log.New(sink)

	cfg := h.members.Config()
	if cfg == nil {
		t.Fatal("the checker has no config, so the vote cannot resolve a local node")
	}
	stub.quorum = quorum.NewQuorumManager(cfg, log.New(sink))
	return h, sink
}

// candidate finds a member by ID, since attemptVotingElection takes the member
// rather than its ID.
func candidateNamed(t *testing.T, h *HealthChecker, id string) *Member {
	t.Helper()
	m := h.members.GetMemberByID(id)
	if m == nil {
		t.Fatalf("no member %q in the test cluster", id)
	}
	return m
}

// TestADegradedThreeNodeClusterReachesTheTiebreak is the ticket's three-node case:
// the Active has failed and two Passives remain.
//
// Both askers must reach the same verdict about the same candidate. Under the old
// `localNodeID < otherNodeID` rule they did not, and this is that failure observed
// through the path production actually takes.
func TestADegradedThreeNodeClusterReachesTheTiebreak(t *testing.T) {
	build := func(localID string) (*HealthChecker, *logSink) {
		failed := newAATestMember("node-c", "host-c", StatusUnknown, nil)
		low := newAATestMember("node-a", "host-a", StatusPassive, nil)
		high := newAATestMember("node-b", "host-b", StatusPassive, nil)
		return newVotingChecker(t, localID, failed, low, high)
	}

	for _, subject := range []string{"node-a", "node-b"} {
		verdicts := map[string]bool{}
		for _, asker := range []string{"node-a", "node-b", "node-c"} {
			h, sink := build(asker)
			verdicts[asker] = h.attemptVotingElection(candidateNamed(t, h, subject))

			if logged := sink.String(); !strings.Contains(logged, tiebreakEntered) {
				t.Fatalf("asked by %s about %s: the election never reached the "+
					"two-available-node branch, so this test is measuring something "+
					"else. Log:\n%s", asker, subject, logged)
			}
		}
		if verdicts["node-a"] != verdicts["node-b"] || verdicts["node-b"] != verdicts["node-c"] {
			t.Errorf("the verdict on %s depends on who ran the election: %v. A "+
				"tie-break with no majority behind it that disagrees with itself is "+
				"a coin flip, not a tie-break", subject, verdicts)
		}
	}
}

// TestADegradedFourNodeClusterReachesTheTiebreak is the ticket's other shape: two
// of four Unknown, which satisfies attemptVotingElection's gate on Passive-or-
// Unknown while leaving exactly two Active-or-Passive for the tie-break.
func TestADegradedFourNodeClusterReachesTheTiebreak(t *testing.T) {
	build := func(localID string) (*HealthChecker, *logSink) {
		gone := newAATestMember("node-c", "host-c", StatusUnknown, nil)
		alsoGone := newAATestMember("node-d", "host-d", StatusUnknown, nil)
		low := newAATestMember("node-a", "host-a", StatusPassive, nil)
		high := newAATestMember("node-b", "host-b", StatusPassive, nil)
		return newVotingChecker(t, localID, gone, alsoGone, low, high)
	}

	verdicts := map[string]bool{}
	for _, asker := range []string{"node-a", "node-b"} {
		h, sink := build(asker)
		verdicts[asker] = h.attemptVotingElection(candidateNamed(t, h, "node-b"))

		if logged := sink.String(); !strings.Contains(logged, tiebreakEntered) {
			t.Fatalf("asked by %s: a four-node cluster with two Unknown did not reach "+
				"the two-available-node branch. Log:\n%s", asker, logged)
		}
	}
	if verdicts["node-a"] != verdicts["node-b"] {
		t.Errorf("the verdict on node-b depends on who ran the election: %v", verdicts)
	}
}

// TestAGenuineTwoNodeClusterNeverReachesTheTiebreak pins ADR-0002 at the election
// level, where the existing test only pins the rule's own arithmetic.
//
// Two Passives is availableCount == 2, below attemptVotingElection's gate of
// three, so the vote is skipped and the pair promotes directly -- both sides
// claiming the group, which is the availability-over-safety choice that ADR made
// deliberately. If a future change routes two nodes into this branch, one of them
// loses on node ID alone and a partition can leave the address dark on the half
// that is still serving.
func TestAGenuineTwoNodeClusterNeverReachesTheTiebreak(t *testing.T) {
	for _, asker := range []string{"node-a", "node-b"} {
		low := newAATestMember("node-a", "host-a", StatusPassive, nil)
		high := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, sink := newVotingChecker(t, asker, low, high)

		if h.attemptVotingElection(candidateNamed(t, h, "node-b")) {
			t.Errorf("asked by %s: a two-node cluster ran a voting election; it must "+
				"fall through to direct promotion", asker)
		}
		if logged := sink.String(); strings.Contains(logged, tiebreakEntered) {
			t.Errorf("asked by %s: a genuine two-node cluster reached the "+
				"two-available-node tie-break. One node then loses on node ID alone, "+
				"and across a partition that can be the half still serving traffic "+
				"(docs/adr/0002-two-node-availability-over-safety.md). Log:\n%s",
				asker, logged)
		}
	}
}

// TestTheTiebreakIsSkippedWithoutAQuorumManager records why these tests carry one,
// so the next person to write one does not measure the fallback by accident.
func TestTheTiebreakIsSkippedWithoutAQuorumManager(t *testing.T) {
	failed := newAATestMember("node-c", "host-c", StatusUnknown, nil)
	low := newAATestMember("node-a", "host-a", StatusPassive, nil)
	high := newAATestMember("node-b", "host-b", StatusPassive, nil)

	h, stub := newAPTestChecker("node-a", failed, low, high)
	sink := &logSink{}
	h.logger = log.New(sink)
	if stub.quorum != nil {
		t.Fatal("the default stub is meant to have no quorum manager")
	}

	if h.attemptVotingElection(candidateNamed(t, h, "node-a")) {
		t.Error("a checker with no quorum manager reported a successful vote")
	}
	if logged := sink.String(); strings.Contains(logged, tiebreakEntered) {
		t.Errorf("the vote ran without a quorum manager:\n%s", logged)
	}
}
