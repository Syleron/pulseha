package membership

import (
	"io"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// Start has to survive being called before there is a cluster, and succeed when one turns up.
//
// initializeExpectedIPs needs a local node ID, which needs a configured cluster. The daemon's
// only Start call used to sit in Server.Start, which runs before `cluster create` on a node
// being set up for the first time — so it failed, nothing retried, and the node ran with no
// enforce loop for the rest of its life. Observed live: a node demoted after a partition
// healed was still holding the whole floating IP group, with no ENFORCE or REFRESH activity at
// all, because the loop that releases addresses had never started (docs/TEST-PLAN.md #83).
func TestIPMonitorStartIsRetryableAndIdempotent(t *testing.T) {
	newMonitor := func(cfg *config.Config) (*IPMonitor, *MemberList) {
		logger := log.New(io.Discard)
		ml := NewMemberList(cfg, logger)
		m := NewIPMonitor(ml, logger)
		// enforceExpectations needs netlink and is a no-op off Linux; the lifecycle under
		// test is the same either way, so the pass itself is stubbed out.
		m.enforce = func() {}
		return m, ml
	}

	// A config naming no nodes is what ClusterCheck rejects, and what a freshly installed
	// node holds between daemon start and `pulsectl cluster create`.
	clusterless := func() *config.Config {
		return &config.Config{
			Pulse: config.Local{Mode: "active-passive"},
			Nodes: map[string]*config.Node{},
		}
	}

	t.Run("fails without a cluster and stays not-running", func(t *testing.T) {
		m, _ := newMonitor(clusterless())
		defer m.Stop()

		if err := m.Start(); err == nil {
			t.Error("expected Start to fail with no cluster configured")
		}
		// The load-bearing half. Left running, a later retry would no-op and the node would
		// never get its enforce loop — which is the defect, not the initial failure.
		if m.IsRunning() {
			t.Error("expected the monitor to stay not-running after a failed Start, so a retry can succeed")
		}
	})

	t.Run("starts on a later call once a cluster exists", func(t *testing.T) {
		cfg := clusterless()
		m, ml := newMonitor(cfg)
		defer m.Stop()

		if err := m.Start(); err == nil {
			t.Fatal("expected the first Start to fail with no cluster")
		}

		// `cluster create` populating the config and the member list, which is the moment
		// the retry has to work.
		const nodeID = "node-a"
		cfg.Pulse.LocalNode = nodeID
		cfg.Nodes[nodeID] = &config.Node{
			Hostname: "host-a",
			IPGroups: map[string][]string{},
		}
		if err := ml.AddMemberQuiet(nodeID); err != nil {
			t.Fatalf("add member: %v", err)
		}

		if err := m.Start(); err != nil {
			t.Fatalf("expected Start to succeed once a cluster exists, got %v", err)
		}
		if !m.IsRunning() {
			t.Error("expected the monitor to report running after a successful Start")
		}
	})

	t.Run("a repeat Start does not re-initialise", func(t *testing.T) {
		const nodeID = "node-a"
		cfg := &config.Config{
			Pulse:  config.Local{Mode: "active-passive", LocalNode: nodeID},
			Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
			Nodes: map[string]*config.Node{
				nodeID: {Hostname: "host-a", IPGroups: map[string][]string{"eth0": {"group1"}}},
			},
		}
		m, ml := newMonitor(cfg)
		defer m.Stop()
		if err := ml.AddMemberQuiet(nodeID); err != nil {
			t.Fatalf("add member: %v", err)
		}
		ml.GetMemberByID(nodeID).Status = StatusActive

		if err := m.Start(); err != nil {
			t.Fatalf("first Start: %v", err)
		}

		// startHealthChecker is called from six places and now carries Start with it, so
		// repeat calls are the normal case rather than a mistake. Asserted through a side
		// effect rather than the return value, because the damage a repeat call does is
		// invisible to the caller: initializeExpectedIPs resets the expectation map and two
		// more goroutines start racing the first over the same addresses, while Start still
		// returns nil either way.
		sentinel := []string{"10.9.9.9/24"}
		m.UpdateExpectedIPs("eth0", sentinel)

		for i := 0; i < 3; i++ {
			if err := m.Start(); err != nil {
				t.Errorf("repeat Start %d returned %v, want nil", i+1, err)
			}
		}

		got := m.GetExpectedIPs("eth0")
		if len(got) != 1 || got[0] != sentinel[0] {
			t.Errorf("repeat Start re-initialised the monitor: expected IPs are %v, want %v", got, sentinel)
		}
		if !m.IsRunning() {
			t.Error("expected the monitor to still be running")
		}
	})

	t.Run("a nil monitor is safe", func(t *testing.T) {
		var m *IPMonitor
		if err := m.Start(); err != nil {
			t.Errorf("expected a nil monitor's Start to be a no-op, got %v", err)
		}
		if m.IsRunning() {
			t.Error("expected a nil monitor to report not running")
		}
	})
}
