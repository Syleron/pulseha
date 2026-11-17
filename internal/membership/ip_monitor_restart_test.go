package membership

import (
	"io"
	"slices"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// restartTestIface is over the kernel's 15-character limit, so on Linux the
// monitor's real loops — which these tests start — cannot touch an interface.
const restartTestIface = "pulseha-no-such-interface"

// restartableMonitor is an Active node in an active-passive cluster holding
// group1 on restartTestIface, with its enforce pass replaced by a gate the test
// controls.
func restartableMonitor(t *testing.T) (*IPMonitor, *enforceGate, []string) {
	t.Helper()
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-passive", LocalNode: "node-a"},
		Groups: map[string][]string{"group1": group},
		Nodes: map[string]*config.Node{
			"node-a": {IPGroups: map[string][]string{restartTestIface: {"group1"}}},
		},
	}
	member := newAATestMember("node-a", "host-a", StatusActive, nil)
	m := NewIPMonitor(newAATestMemberList(cfg, member), log.New(io.Discard))
	g := newEnforceGate(m, 16)
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
		m.Stop()
	})
	return m, g, group
}

// Regression for GitHub #274. `pulsectl cluster leave` stops the IP monitor, the
// daemon keeps running, and the node can join a cluster again. The monitor is
// built once, in NewServer, and Stop could not be undone, so a node that left and
// rejoined without a restart ran with no monitor at all: no netlink watcher
// restoring an address that vanished, no enforce pass placing or releasing, no
// periodic reconcile — reported nowhere but a Debug line.
func TestAMonitorStartedAgainAfterStopRuns(t *testing.T) {
	m, g, _ := restartableMonitor(t)
	close(g.release) // passes run straight through; this test counts them

	if err := m.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	m.Stop()
	if !m.Stopped() {
		t.Fatal("Stopped() = false after Stop")
	}

	if err := m.Start(); err != nil {
		t.Fatalf("Start after Stop = %v; a node that left and rejoined runs with no IP monitor", err)
	}
	if !m.IsRunning() || m.Stopped() {
		t.Errorf("after the restart IsRunning=%v Stopped=%v, want running and not stopped", m.IsRunning(), m.Stopped())
	}

	waitForEnforceIdle(t, m, 2*time.Second)
	before := g.totalPasses()
	m.TriggerEnforce()
	waitForEnforceIdle(t, m, 2*time.Second)
	if g.totalPasses() <= before {
		t.Error("a trigger on the restarted monitor ran no enforce pass")
	}
}

// A restarted monitor belongs to a new cluster membership. What the previous one
// was told to release, and what it expected, are about the old cluster's
// addresses and must not shape the new one.
func TestARestartForgetsThePreviousRunsRecords(t *testing.T) {
	m, g, group := restartableMonitor(t)
	close(g.release) // let passes through; this test is about state, not timing

	if err := m.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	m.RemoveExpectedIPs(restartTestIface, group)
	m.UpdateExpectedIPs("eth-from-the-old-cluster", []string{"192.0.2.1/24"})
	m.Stop()

	if err := m.Start(); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}

	if _, released := m.restorableIPs(restartTestIface, group); len(released) != 0 {
		t.Errorf("the restarted monitor still refuses %v the previous run was told to release", released)
	}
	if got := m.GetExpectedIPs("eth-from-the-old-cluster"); len(got) != 0 {
		t.Errorf("the restarted monitor still expects %v on an interface only the old cluster used", got)
	}
	if got := m.GetExpectedIPs(restartTestIface); !slices.Equal(got, group) {
		t.Errorf("expectations after the restart = %v, want the current config's %v", got, group)
	}
}

// Stop's wait is bounded, so a pass from the stopped run can still be in flight
// when the monitor is started again. It belongs to the old run: it must finish
// without running a follow-up under the new one, and without disturbing it.
func TestAPassFromTheStoppedRunDoesNotCarryIntoTheNewOne(t *testing.T) {
	m, g, _ := restartableMonitor(t)
	m.stopWaitLimit = 50 * time.Millisecond

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second) // the old run's pass, held open
	m.TriggerEnforce()               // and a follow-up queued behind it

	m.Stop() // gives up waiting on the held pass
	if err := m.Start(); err != nil {
		t.Fatalf("Start while the old run's pass is in flight: %v", err)
	}

	close(g.release)
	waitForEnforceIdle(t, m, 2*time.Second)

	// The new run is unaffected by the old pass finishing — its own trigger is not
	// swallowed by the old run's coalescing flags — and keeps working.
	before := g.totalPasses()
	m.TriggerEnforce()
	waitForEnforceIdle(t, m, 2*time.Second)
	if g.totalPasses() <= before {
		t.Error("a trigger on the new run ran no enforce pass after the old run's pass finished")
	}
	if m.Stopped() || !m.IsRunning() {
		t.Errorf("new run Stopped=%v IsRunning=%v, want running", m.Stopped(), m.IsRunning())
	}
}

// The coalescing flags belong to a run. With them shared, a trigger on the new
// run landed while the stopped run's pass was still in flight, was queued as that
// pass's follow-up, and was dropped when the pass saw its run stopped — the new
// run's first enforce pass never ran.
func TestTheNewRunsFirstTriggerIsNotSwallowedByTheOldRunsPass(t *testing.T) {
	m, g, _ := restartableMonitor(t)
	m.stopWaitLimit = 50 * time.Millisecond

	m.TriggerEnforce()
	g.waitForEntry(t, 2*time.Second) // the old run's pass, held open
	m.Stop()
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start's own initialisation triggered the new run's first pass. It must be
	// running — entering the gate — while the old one is still held.
	g.waitForEntry(t, 2*time.Second)
	close(g.release)
}
