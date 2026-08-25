package membership

import (
	"slices"
	"testing"
)

// Consolidating a two-node split-brain used to leave the group dark on a cluster
// that had just healed.
//
// A gratuitous ARP is only ever sent by a bring-up, and there is no periodic
// re-announce. During the split both nodes hold the whole group, and the node that
// promoted second announced last — so the segment has learned its MAC. Consolidation
// then picks a survivor by node ID (ConsolidationTarget breaks the tie on address
// count, which is equal here, then on ID), which bears no relation to which node the
// segment learned. When the survivor is the *other* one, the demoted node drops
// addresses every ARP cache still points at, while the survivor — which held them
// throughout and so brings nothing up — says nothing at all. The group is then
// present on the survivor and reachable by nobody until the caches age out.
//
// See docs/adr/0002-two-node-availability-over-safety.md. The two-node split-brain
// is deliberate; a recovery that turns it into an outage is not.
func TestConsolidationAnnouncesOnTheSurvivor(t *testing.T) {
	// The whole group on both nodes, which is what a two-node split-brain looks
	// like: each side promoted itself and claimed everything.
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}

	t.Run("announces on the surviving Active", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, group)
		b := newAATestMember("node-b", "host-b", StatusActive, group)
		h, stub := newAPTestChecker("node-a", a, b)

		if !h.enforceSingleActive(h.members.Members) {
			t.Fatal("expected consolidation to report a demotion")
		}

		// Equal address counts, so ConsolidationTarget breaks the tie on the lower
		// node ID: node-a survives and node-b is demoted.
		if !slices.Equal(stub.announced, []string{"node-a"}) {
			t.Errorf("expected the surviving Active to re-announce its addresses, got %v", stub.announced)
		}
	})

	t.Run("announces after the demotions, not before", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, group)
		b := newAATestMember("node-b", "host-b", StatusActive, group)
		h, stub := newAPTestChecker("node-a", a, b)

		if !h.enforceSingleActive(h.members.Members) {
			t.Fatal("expected consolidation to report a demotion")
		}

		// Ordering is the entire point. The demoted node's own bring-up announced
		// after the survivor's, so an announcement issued before the demotion is
		// still the older one on the segment and changes nothing.
		want := []string{"demote:node-b", "announce:node-a"}
		if !slices.Equal(stub.sequence, want) {
			t.Errorf("expected the announcement to be the last word on the segment\n got %v\nwant %v",
				stub.sequence, want)
		}
	})

	t.Run("does not announce when there was nothing to consolidate", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, group)
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", a, b)

		if h.enforceSingleActive(h.members.Members) {
			t.Error("expected no consolidation with a single Active node")
		}
		// A healthy active-passive cluster runs this pass every health-check cycle.
		// Announcing on each one would put a whole-group arping storm on the segment
		// forever, which is defect #4's cost paid continuously rather than once.
		if len(stub.announced) != 0 {
			t.Errorf("expected no announcement without a demotion, got %v", stub.announced)
		}
	})

	t.Run("a rejected demotion does not announce", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, group)
		b := newAATestMember("node-b", "host-b", StatusActive, group)
		h, stub := newAPTestChecker("node-a", a, b)
		stub.makePassiveFails = true

		if h.enforceSingleActive(h.members.Members) {
			t.Error("expected a failed demotion to report no change")
		}
		// Both nodes still hold everything, so the segment's entry is not stale and
		// re-announcing would only pick a winner the cluster has not chosen.
		if len(stub.announced) != 0 {
			t.Errorf("expected no announcement when nothing was demoted, got %v", stub.announced)
		}
	})

	t.Run("a failed announcement does not abort the pass", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, group)
		b := newAATestMember("node-b", "host-b", StatusActive, group)
		h, stub := newAPTestChecker("node-a", a, b)
		stub.announceFails = true

		// The addresses are up on the survivor either way. A failed announcement
		// leaves the pre-existing stale ARP entries rather than creating a new
		// fault, so the demotion must still be reported and the state still pushed —
		// reporting no change here would make the next cycle retry a consolidation
		// that already happened.
		if !h.enforceSingleActive(h.members.Members) {
			t.Fatal("expected the demotion to be reported despite a failed announcement")
		}
		if b.Status != StatusPassive {
			t.Errorf("expected node-b to stay demoted, got %s", StatusToString(b.Status))
		}
		if stub.broadcastLeader != "node-a" {
			t.Errorf("expected the corrected state to be broadcast anyway, got %q", stub.broadcastLeader)
		}
	})
}
