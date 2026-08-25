package membership

import (
	"testing"
)

// The surviving Active has to re-announce when a peer stops being able to hold floating IPs.
//
// A gratuitous ARP is only ever sent by a bring-up and there is no periodic re-announce, so a
// node holding an address continuously never announces it again. After a two-node split both
// nodes held the group and the one that promoted second announced last; when it is demoted it
// drops those addresses without announcing anything — bring-down never does — and every ARP
// cache is left pointing at a node that no longer answers.
//
// Detected on this pass rather than at either end of the state broadcast. Both of those were
// tried against the live rig and each is half the answer: the ConfigSync receive hook fires
// only on a node TOLD of the demotion, and node-ID ordering decides whether that is the
// survivor — across three heals the survivor was the receiver once and the originator twice.
// The send side cannot diff, because callers apply the state before broadcasting it. This pass
// sees the settled view every tick whoever produced it (docs/TEST-PLAN.md #80).
func TestAnnounceOnPeerDemotion(t *testing.T) {
	// pass runs one detection cycle against the current member statuses.
	pass := func(h *HealthChecker) { h.announceOnPeerDemotion(h.members.Members) }

	t.Run("announces when a peer settles from Unknown to Passive", func(t *testing.T) {
		// The transition a healed partition produces. This node lost sight of the peer
		// during the split, so the peer's last known status was Unknown and never Active —
		// which is why a condition requiring Active missed the case entirely.
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusUnknown, nil)
		h, stub := newAPTestChecker("node-a", a, b)

		pass(h) // establishes the baseline view; nothing to compare against yet
		if len(stub.announced) != 0 {
			t.Fatalf("expected no announcement on the first pass, got %v", stub.announced)
		}

		b.Status = StatusPassive
		pass(h)

		if len(stub.announced) != 1 || stub.announced[0] != "node-a" {
			t.Errorf("expected the surviving Active to announce its own addresses, got %v", stub.announced)
		}
	})

	t.Run("announces when a peer is demoted from Active", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.1/24"})
		h, stub := newAPTestChecker("node-a", a, b)

		pass(h)
		b.Status = StatusPassive
		pass(h)

		if len(stub.announced) != 1 {
			t.Errorf("expected one announcement after a peer was demoted, got %v", stub.announced)
		}
	})

	t.Run("does not announce on the first pass", func(t *testing.T) {
		// There is no previous view to compare against, so every status looks new. Firing
		// here would announce on every daemon start rather than on a transition.
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", a, b)

		pass(h)

		if len(stub.announced) != 0 {
			t.Errorf("expected no announcement without a prior view, got %v", stub.announced)
		}
	})

	t.Run("does not announce when nothing changed", func(t *testing.T) {
		// This pass runs every health-check tick. Announcing on state rather than on change
		// would put a whole-group arping storm on the segment forever.
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", a, b)

		for i := 0; i < 4; i++ {
			pass(h)
		}

		if len(stub.announced) != 0 {
			t.Errorf("expected no announcement across steady passes, got %v", stub.announced)
		}
	})

	t.Run("does not announce when the local node is not Active", func(t *testing.T) {
		// A node holding nothing has nothing to announce, and claiming the group would tell
		// the segment to send traffic to a node that does not serve it.
		a := newAATestMember("node-a", "host-a", StatusPassive, nil)
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.1/24"})
		h, stub := newAPTestChecker("node-a", a, b)

		pass(h)
		b.Status = StatusMaintenance
		pass(h)

		if len(stub.announced) != 0 {
			t.Errorf("expected no announcement from a Passive node, got %v", stub.announced)
		}
	})

	t.Run("does not announce when a peer merely goes Unknown", func(t *testing.T) {
		// Unknown is not a release: the peer may still be holding every address. If it has
		// genuinely failed, the promotion that follows brings the addresses up here and
		// announces them itself.
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", a, b)

		pass(h)
		b.Status = StatusUnknown
		pass(h)

		if len(stub.announced) != 0 {
			t.Errorf("expected no announcement for a peer going Unknown, got %v", stub.announced)
		}
	})
}
