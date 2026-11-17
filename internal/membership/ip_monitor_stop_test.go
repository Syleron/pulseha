package membership

import (
	"testing"
	"time"
)

// Regression for GitHub #273 (docs/TEST-PLAN.md defect #119). Server.Stop takes
// the node's floating IPs down after stopping the monitor, and that only works if
// Stop leaves nothing of the monitor running: a pass still in flight would put
// back the addresses being released, and the process would exit with the group
// in the kernel while the peer took it over.
func TestStopWaitsForTheEnforcePassInFlight(t *testing.T) {
	m := coalesceTestMonitor()
	g := newEnforceGate(m, 2)

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second)

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while an enforce pass was still running; the caller would release addresses the pass can put back")
	case <-time.After(200 * time.Millisecond):
	}

	close(g.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the enforce pass finished")
	}

	if !m.Stopped() {
		t.Error("Stopped() = false after Stop returned")
	}
}

// The wait is bounded. A pass wedged on the kernel must not block a daemon
// shutdown for good: systemd would SIGKILL it at the stop timeout, and a killed
// daemon releases nothing at all.
func TestStopGivesUpOnAnEnforcePassThatNeverReturns(t *testing.T) {
	m := coalesceTestMonitor()
	m.stopWaitLimit = 50 * time.Millisecond
	g := newEnforceGate(m, 2)
	defer close(g.release)

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second)

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop is still waiting on a wedged enforce pass after 2s; the wait must be bounded")
	}
}

// Stop with nothing running returns at once and reports itself stopped, and a
// second Stop is harmless.
func TestStopWithNothingRunning(t *testing.T) {
	m := coalesceTestMonitor()
	if m.Stopped() {
		t.Fatal("Stopped() = true before Stop")
	}

	done := make(chan struct{})
	go func() {
		m.Stop()
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop on an idle monitor did not return")
	}
	if !m.Stopped() {
		t.Error("Stopped() = false after Stop")
	}
}

// Once Stop has begun, a trigger must not start a pass, even one racing it.
func TestNoEnforcePassStartsAfterStop(t *testing.T) {
	m := coalesceTestMonitor()
	g := newEnforceGate(m, 2)
	defer close(g.release)

	m.Stop()
	m.TriggerEnforce()

	time.Sleep(50 * time.Millisecond)
	if n := g.totalPasses(); n != 0 {
		t.Errorf("%d enforce passes ran after Stop, want 0", n)
	}
}
