package membership

import (
	"sync"
	"testing"
	"time"
)

// enforceGate replaces the monitor's enforce pass with one the test can hold
// open, and records both how many passes ran and how many ever ran at once.
//
// Holding the pass open is the whole method: concurrency that is never made to
// overlap is concurrency a test cannot see, and defect #63 is precisely passes
// overlapping — 34 of them inside one second on whitecrane node-4.
type enforceGate struct {
	mu       sync.Mutex
	passes   int
	inFlight int
	peak     int

	entered chan struct{} // one token per pass entry
	release chan struct{} // closed to let held passes return
}

func newEnforceGate(m *IPMonitor, capacity int) *enforceGate {
	g := &enforceGate{
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
	m.enforce = func() {
		g.mu.Lock()
		g.passes++
		g.inFlight++
		if g.inFlight > g.peak {
			g.peak = g.inFlight
		}
		g.mu.Unlock()

		g.entered <- struct{}{}
		<-g.release

		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}
	return g
}

func (g *enforceGate) totalPasses() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.passes
}

func (g *enforceGate) peakInFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// waitForEntry blocks until a pass has entered the gate.
func (g *enforceGate) waitForEntry(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(within):
		t.Fatalf("no enforce pass started within %s", within)
	}
}

// waitForOverlap returns as soon as more than one pass is in flight, and
// otherwise after the whole window. The expected result is a negative, so it has
// to be a window rather than a single read — a coalesced monitor never overlaps,
// and an uncoalesced one needs a moment to get its goroutines running.
func (g *enforceGate) waitForOverlap(within time.Duration) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if g.peakInFlight() > 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForEnforceIdle blocks until no pass is running and none is queued. Reads
// the coalescing state under its own mutex, the same way waitForReconcileIdle
// reads the reconciliation guard.
func waitForEnforceIdle(t *testing.T, m *IPMonitor, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		m.enforceMu.Lock()
		idle := !m.enforceRunning && !m.enforcePending
		m.enforceMu.Unlock()
		if idle {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("enforce passes did not drain within %s", within)
}

func coalesceTestMonitor() *IPMonitor {
	group := []string{"10.0.0.1/23", "10.0.0.2/23", "10.0.0.3/23"}
	m, _, _ := newExpectationsMonitor("active-active", group, group)
	return m
}

// Regression for docs/TEST-PLAN.md defect #63. Every expectation write called
// TriggerEnforce, which started an enforceExpectations *goroutine*, so a burst of
// writes started a burst of passes that each recomputed the same missing set and
// each announced what it placed through its own SendGARPBatch. SendGARPBatch caps
// one batch at 32 arpings in flight, but the cap is per call, so the real ceiling
// was 32 × passes: run 32 placed a 248-address group with 34 placement batches
// inside one epoch second, 618 per-address placements to settle 62 addresses, and
// 549 concurrent arping processes on one node. That is #7's resource shape — the
// condition where a node stops answering health checks because it is saturated
// announcing — reattached to the placement path.
func TestConcurrentEnforceTriggersNeverRunTwoPassesAtOnce(t *testing.T) {
	m := coalesceTestMonitor()
	const triggers = 20
	g := newEnforceGate(m, triggers+2)

	for range triggers {
		m.TriggerEnforce()
	}

	// The first pass is now held open, so anything that ran concurrently pre-fix
	// is still in flight and countable.
	g.waitForEntry(t, 2*time.Second)
	g.waitForOverlap(250 * time.Millisecond)

	if peak := g.peakInFlight(); peak != 1 {
		t.Errorf("%d enforce passes ran at once, want 1: %d triggers must coalesce, not multiply", peak, triggers)
	}

	close(g.release)
	waitForEnforceIdle(t, m, 2*time.Second)

	// One in flight and one queued: the 19 triggers that arrived during the first
	// pass collapse to a single follow-up, not 19 of them.
	if passes := g.totalPasses(); passes != 2 {
		t.Errorf("ran %d passes for %d triggers, want 2 (the one in flight plus one queued)", passes, triggers)
	}
}

// The counterpart requirement, and the reason this is not the reconciliation
// guard's drop-if-running: a pass already in flight may have taken its
// expectation snapshot before this write, so dropping the trigger would lose it.
// One follow-up pass has to run after the current one returns.
func TestATriggerDuringAnEnforcePassRunsAFollowUpAfterIt(t *testing.T) {
	m := coalesceTestMonitor()
	g := newEnforceGate(m, 4)

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second)

	// The write whose expectation the running pass may not have seen.
	m.TriggerEnforce()

	if passes := g.totalPasses(); passes != 1 {
		t.Fatalf("%d passes ran while the first was still held, want 1", passes)
	}

	close(g.release)
	waitForEnforceIdle(t, m, 2*time.Second)

	if passes := g.totalPasses(); passes != 2 {
		t.Errorf("ran %d passes, want 2: the trigger arriving mid-pass must not be dropped", passes)
	}
	if peak := g.peakInFlight(); peak != 1 {
		t.Errorf("peak in flight = %d, want 1: the follow-up must run after the first pass, not beside it", peak)
	}
}

// The gate has to re-arm. A monitor that coalesced once and then refused to run
// again would stop reconciling the interface at all, which is worse than the
// defect: the 30s periodic pass goes through the same gate.
func TestTheEnforceGateRearmsOnceTheQueueDrains(t *testing.T) {
	m := coalesceTestMonitor()
	g := newEnforceGate(m, 8)

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second)
	close(g.release)
	waitForEnforceIdle(t, m, 2*time.Second)

	if passes := g.totalPasses(); passes != 1 {
		t.Fatalf("ran %d passes for one trigger, want 1", passes)
	}

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second)
	waitForEnforceIdle(t, m, 2*time.Second)

	if passes := g.totalPasses(); passes != 2 {
		t.Errorf("ran %d passes, want 2: a trigger after the queue drained must start a fresh pass", passes)
	}
}

// A stopped monitor runs nothing, as TriggerEnforce already promised, and that
// now has to hold for the queued follow-up too — otherwise Stop returns while a
// pass is still to come.
func TestAQueuedEnforcePassIsAbandonedWhenTheMonitorStops(t *testing.T) {
	m := coalesceTestMonitor()
	g := newEnforceGate(m, 8)

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second)
	m.TriggerEnforce() // queued
	m.Stop()
	close(g.release)
	waitForEnforceIdle(t, m, 2*time.Second)

	if passes := g.totalPasses(); passes != 1 {
		t.Errorf("ran %d passes, want 1: the queued pass must be abandoned once the monitor stops", passes)
	}

	m.TriggerEnforce()
	time.Sleep(50 * time.Millisecond)
	if passes := g.totalPasses(); passes != 1 {
		t.Errorf("ran %d passes after Stop, want 1", passes)
	}
}
