package membership

import (
	"testing"
)

// The two-available-node tie-break has to decide about the SUBJECT of the vote, never about the
// node running it.
//
// `initiateNodeStatusVote(nodeID, StatusActive)` asks "should nodeID become Active". The rule
// used to answer with `localNodeID < otherNodeID` — "should *I* win" — which is a different
// question whenever the candidate is not the local node, and via attemptVotingElection it
// frequently is not: selectBestCandidate gives the local node only +5 against a score built
// from status, latency and recency. In handlePartialFailure the answer is not advisory,
// `!voteResult` returns and abandons the failover.
//
// Reachable only in a DEGRADED cluster of three or more — availableNodes counts Active and
// Passive, so this is the Active-has-failed-and-two-Passives-remain case, or two of four
// Unknown. A genuine two-node cluster never arrives here, and must not: see
// docs/adr/0002-two-node-availability-over-safety.md. END-2325.
func TestTwoNodeTiebreakDecidesOnTheSubject(t *testing.T) {
	// Three members: the failed Active plus two Passive contenders, which is what
	// handlePartialFailure produces after it marks the Active Unknown.
	newDegradedChecker := func(localID string) (*HealthChecker, *stubServer) {
		failed := newAATestMember("node-c", "host-c", StatusUnknown, nil)
		low := newAATestMember("node-a", "host-a", StatusPassive, nil)
		high := newAATestMember("node-b", "host-b", StatusPassive, nil)
		return newAPTestChecker(localID, failed, low, high)
	}

	t.Run("the lower-ID candidate wins whoever asks", func(t *testing.T) {
		// The property that matters. Two nodes running this concurrently for the same
		// candidate must reach the same verdict — a tie-break with no majority behind it
		// that depends on the asker is not a tie-break, it is a coin flip. Under the old
		// rule these two lines returned opposite answers.
		for _, asker := range []string{"node-a", "node-b"} {
			h, _ := newDegradedChecker(asker)
			if !h.initiateNodeStatusVote("node-a", StatusActive) {
				t.Errorf("asked by %s: expected the lower-ID candidate node-a to win", asker)
			}
		}
	})

	t.Run("the higher-ID candidate loses whoever asks", func(t *testing.T) {
		for _, asker := range []string{"node-a", "node-b"} {
			h, _ := newDegradedChecker(asker)
			if h.initiateNodeStatusVote("node-b", StatusActive) {
				t.Errorf("asked by %s: expected the higher-ID candidate node-b to lose", asker)
			}
		}
	})

	t.Run("the verdict does not change when the asker is the candidate", func(t *testing.T) {
		// node-b asking about node-b got a different answer from node-a asking about
		// node-b under the old rule. It must not.
		hSelf, _ := newDegradedChecker("node-b")
		hOther, _ := newDegradedChecker("node-a")

		self := hSelf.initiateNodeStatusVote("node-b", StatusActive)
		other := hOther.initiateNodeStatusVote("node-b", StatusActive)
		if self != other {
			t.Errorf("verdict on node-b depends on who asked: self=%v other=%v", self, other)
		}
	})

	t.Run("a subject outside the tie is allowed", func(t *testing.T) {
		// node-c is Unknown, so it is not one of the two contenders this rule exists to
		// separate and the rule has nothing to say about it. Refusing here would block a
		// promotion on a rule that does not apply; the promotion still has to satisfy
		// confirmPeerReleasedIPs downstream.
		h, _ := newDegradedChecker("node-a")
		if !h.initiateNodeStatusVote("node-c", StatusActive) {
			t.Error("expected a subject outside the available pair to be allowed")
		}
	})

	t.Run("non-Active status changes are unaffected", func(t *testing.T) {
		// The rule guards promotion. Marking a node Unknown or Passive is not a claim on
		// the floating IPs and was never gated by it.
		h, _ := newDegradedChecker("node-b")
		if !h.initiateNodeStatusVote("node-b", StatusUnknown) {
			t.Error("expected a non-Active status change to be allowed")
		}
	})
}

// The caller has to agree with the rule about who the candidate is.
//
// handlePartialFailure took the first Passive the member map yielded, and Go randomises map
// iteration — so with two Passives it asked the vote about a coin flip. Now that the vote is
// decided on the subject's ID, asking about the higher one is refused and `!voteResult`
// returns, abandoning the failover instead of trying the other node. Roughly half of these
// failovers would have been dropped. Selection and adjudication have to use the same rule
// (END-2325).
func TestPartialFailurePromotesTheSameNodeEveryTime(t *testing.T) {
	const attempts = 20

	for i := 0; i < attempts; i++ {
		// Three nodes: an Active whose addresses have all failed, and two Passive
		// contenders whose IDs sort in a known order.
		active := newAATestMember("node-c", "host-c", StatusActive, []string{"10.0.0.1/24"})
		low := newAATestMember("node-a", "host-a", StatusPassive, nil)
		high := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", active, low, high)

		h.handlePartialFailure(active, []string{"10.0.0.1/24"})

		if len(stub.failovers) != 1 {
			t.Fatalf("attempt %d: expected exactly one promotion, got %v", i, stub.failovers)
		}
		// Lower ID, every time. A single differing attempt means the selection is back
		// to depending on map order, which is the bug.
		if stub.failovers[0] != "node-a" {
			t.Fatalf("attempt %d: promoted %s, want the lowest-ID Passive node-a",
				i, stub.failovers[0])
		}
	}
}
