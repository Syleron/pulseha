package server

import (
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/network"
)

// The flat 5s this replaced (defect #57) could not cover the rebalance batches
// the coordinator actually emits. Run 25 moved 23–24 addresses at a time; every
// one of those seven moves landed and every one was reported failed, because
// `rebalance move failed` reads the RPC's status rather than the outcome.
func TestBringUpTimeoutCoversTheBatchesThatOverranTheFlatDeadline(t *testing.T) {
	const flat = 5 * time.Second

	if got := bringUpTimeoutFor(24); got <= flat {
		t.Errorf("24 addresses got %s, which is no better than the flat %s it replaced", got, flat)
	}
	// The batch the other side of run 25's storms was busy with, on top of the move.
	if got := bringUpTimeoutFor(71); got <= flat {
		t.Errorf("71 addresses got %s, which is no better than the flat %s it replaced", got, flat)
	}
}

func TestBringUpTimeoutGrowsWithTheBatch(t *testing.T) {
	small := bringUpTimeoutFor(24)
	large := bringUpTimeoutFor(96)

	if large <= small {
		t.Errorf("96 addresses got %s, 24 addresses got %s; the deadline must grow with the batch",
			large, small)
	}
}

// The announcement is the part a demotion has no equivalent of, and the part
// that dominates: the netlink adds are sub-millisecond, while the batch of
// arpings that ends the RPC costs seconds per wave. Sizing a bring-up on the
// address work alone is what left 5s looking sufficient.
func TestBringUpTimeoutIncludesTheAnnouncementBatch(t *testing.T) {
	for _, count := range []int{1, 24, 96} {
		got := bringUpTimeoutFor(count)
		want := membership.DemotionTimeoutFor(count) + network.AnnounceBatchTimeout(count)
		if got != want {
			t.Errorf("%d addresses got %s, want %s (address work plus its announcement)", count, got, want)
		}
		if got <= membership.DemotionTimeoutFor(count) {
			t.Errorf("%d addresses got %s, which leaves no room for the announcement batch", count, got)
		}
	}
}

func TestBringUpTimeoutIsBounded(t *testing.T) {
	// A floor, so an empty or miscounted batch still gets a usable deadline
	// rather than one already expired.
	if got := bringUpTimeoutFor(0); got <= 0 {
		t.Errorf("empty batch got %s, want a usable deadline", got)
	}
	if got := bringUpTimeoutFor(-5); got <= 0 {
		t.Errorf("negative count got %s, want a usable deadline", got)
	}
	// And a ceiling: this deadline is how long the coordinator blocks inside one
	// move, and the reconcile pass it runs on has its own backstop.
	if got := bringUpTimeoutFor(1_000_000); got != bringUpMaxTimeout {
		t.Errorf("a million addresses got %s, want the %s cap", got, bringUpMaxTimeout)
	}
}
