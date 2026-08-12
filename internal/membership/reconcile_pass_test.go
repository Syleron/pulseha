package membership

import (
	"testing"
	"time"
)

// waitForReconcileIdle blocks until no reconciliation pass is in flight. The
// atomic is also the synchronisation point that makes the pass's writes visible
// here, so tests must not read state it touched without going through this.
func waitForReconcileIdle(t *testing.T, h *HealthChecker, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !h.reconcileInFlight.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("reconciliation pass did not finish within %s", within)
}

// countActives reports how many members are Active, read under the member locks.
func countActives(members ...*Member) int {
	n := 0
	for _, m := range members {
		if m.GetStatus() == StatusActive {
			n++
		}
	}
	return n
}

// The health-check tick fires every second, and everything reconciliation reaches
// can block for far longer than that: a serial MakePassive per extra Active, a
// quorum vote that polls for 30s, a remote BringDownIPs per duplicate address.
// Running it inline stopped this node answering its own health checks, so peers
// marked it Unknown and elected around it — the "busy node looks dead" failure the
// rest of this PR exists to prevent.
func TestReconcilePassDoesNotBlockTheHealthCheckTick(t *testing.T) {
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24", "10.0.0.3/24"})
	c := newAATestMember("node-c", "host-c", StatusPassive, nil)
	h, stub := newAPTestChecker("node-a", a, b, c)
	h.ready.Store(true)

	// One slow demotion is enough: the tick's whole budget is 1s.
	stub.makePassiveDelay = 2 * time.Second

	start := time.Now()
	h.startReconcilePassLocked()
	elapsed := time.Since(start)

	if elapsed > 250*time.Millisecond {
		t.Errorf("dispatching reconciliation took %s; it must not run on the tick", elapsed)
	}

	waitForReconcileIdle(t, h, 20*time.Second)

	// And it really did the work, rather than being skipped. The survivor is
	// ConsolidationTarget's choice — the most-loaded Active, host-b — so the
	// assertion is on the count, not on which node.
	if got := countActives(a, b, c); got != 1 {
		t.Errorf("expected consolidation onto a single Active, got %d Actives", got)
	}
}

// A pass slower than the tick must not stack up: four ticks arriving during one
// pass should start one pass, not four. reconcileCycles advances once per pass, so
// it counts them. The guard must then be released, or one slow pass would stop
// reconciliation for the lifetime of the daemon.
func TestReconcilePassesDoNotStackAndTheGuardIsReleased(t *testing.T) {
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})
	c := newAATestMember("node-c", "host-c", StatusActive, []string{"10.0.0.3/24"})
	h, stub := newAPTestChecker("node-a", a, b, c)
	h.ready.Store(true)
	stub.makePassiveDelay = 300 * time.Millisecond

	before, _ := h.reconcileCounters()

	h.startReconcilePassLocked()
	// Three more ticks while the first pass is still demoting.
	for i := 0; i < 3; i++ {
		h.startReconcilePassLocked()
	}

	waitForReconcileIdle(t, h, 20*time.Second)

	mid, _ := h.reconcileCounters()
	if got := mid - before; got != 1 {
		t.Fatalf("four ticks during one slow pass ran %d passes, want 1", got)
	}

	// The guard is free again, so a later tick can start a pass. Asserted by
	// claiming it rather than by running one: a second pass on this cluster goes
	// into the election path, which is itself slow — the very thing being moved
	// off the tick.
	if !h.reconcileInFlight.CompareAndSwap(false, true) {
		t.Error("guard was not released after the pass; no further reconciliation could run")
	}
	h.reconcileInFlight.Store(false)
}

// A stopped health checker must not start new passes, so Stop() does not leave
// reconciliation running against a torn-down cluster.
func TestReconcilePassDoesNotStartWhenStopped(t *testing.T) {
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})
	h, _ := newAPTestChecker("node-a", a, b)
	h.ready.Store(false)

	before, _ := h.reconcileCounters()
	h.startReconcilePassLocked()
	waitForReconcileIdle(t, h, 5*time.Second)

	if after, _ := h.reconcileCounters(); after != before {
		t.Errorf("a stopped health checker ran %d reconciliation passes, want 0", after-before)
	}
}

// Regression for docs/TEST-PLAN.md defect #56. performHealthChecks takes the
// health checker's write lock for its whole body and, from inside that region,
// advances the unchanged-cycles counter and dispatches the reconciliation pass.
// The #51 fix put both behind helpers that took the same lock again — and
// sync.RWMutex is not reentrant, so the first tick to reach either line wedged
// the health-check goroutine *while holding the write lock*. On whitecrane run 24
// that meant `ACTIVE_CHECK: Starting active node failure check` never appeared
// once in six minutes: no node was promoted, nothing placed the 287-address
// group, and all four nodes sat Standby/Passive with the whole group down.
//
// Called with the lock held, the way the tick calls them. A deadlock here would
// hang the package rather than fail it, so the work runs on a goroutine and the
// assertion is the timeout.
func TestTheTickDoesNotSelfDeadlockOnItsOwnLock(t *testing.T) {
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	h, _ := newAPTestChecker("node-a", a)
	h.ready.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Lock()
		defer h.Unlock()
		h.resetChecksWithoutChangeLocked()
		h.incChecksWithoutChangeLocked()
		h.startReconcilePassLocked()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick deadlocked on its own lock; the helpers it calls must not retake it")
	}
	waitForReconcileIdle(t, h, 20*time.Second)
}
