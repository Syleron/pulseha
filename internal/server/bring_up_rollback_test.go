package server

import (
	"context"
	"io"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// Regression for GitHub #265, third trigger: BringUpIP's rollback of additions it
// could not verify recorded them as released, and the Active then refused its own
// addresses for the whole grace window (74s on the lab pair). The rollback must
// drop the expectation and nothing more.
func TestRollingBackAnUnverifiedBringUpDoesNotProtectTheAddresses(t *testing.T) {
	const localID = "node-a"
	// Over the kernel's 15-character limit, so the enforce pass the monitor starts
	// on Linux cannot touch a real interface.
	const iface = "pulseha-no-such-interface"
	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-passive", LocalNode: localID},
		Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
		Nodes: map[string]*config.Node{
			localID: {Hostname: localID, IPGroups: map[string][]string{iface: {"group1"}}},
		},
	}
	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	if err := ml.AddMemberQuiet(localID); err != nil {
		t.Fatalf("AddMemberQuiet: %v", err)
	}
	ml.GetMemberByID(localID).SetStatus(membership.StatusActive)
	monitor := membership.NewIPMonitor(ml, logger)
	t.Cleanup(monitor.Stop)
	s := &Server{config: cfg, logger: logger, memberList: ml, ipMonitor: monitor}

	group := cfg.Groups["group1"]
	monitor.AddExpectedIPs(iface, group)
	s.rollBackUnverifiedAdditions(iface, []string{"10.0.0.2/24"})

	if _, released := monitor.RestorableIPs(iface, group); len(released) != 0 {
		t.Errorf("the rollback protected %v from this Active node's own restore paths", released)
	}
}

// newReleaseTestServer is a single active-passive node assigned group1 on an
// interface that cannot exist, with its monitor expecting the whole group.
func newReleaseTestServer(t *testing.T) (*Server, *membership.IPMonitor, string, []string) {
	t.Helper()
	const localID = "node-a"
	const iface = "pulseha-no-such-interface"
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-passive", LocalNode: localID},
		Groups: map[string][]string{"group1": group},
		Nodes: map[string]*config.Node{
			localID: {Hostname: localID, IPGroups: map[string][]string{iface: {"group1"}}},
		},
	}
	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	if err := ml.AddMemberQuiet(localID); err != nil {
		t.Fatalf("AddMemberQuiet: %v", err)
	}
	monitor := membership.NewIPMonitor(ml, logger)
	t.Cleanup(monitor.Stop)
	monitor.AddExpectedIPs(iface, group)
	return &Server{config: cfg, logger: logger, memberList: ml, ipMonitor: monitor}, monitor, iface, group
}

// Regression for GitHub #265, second trigger, as it actually happens. A restarted
// Active comes up Passive and its VIP reconcile pass releases the group; the node
// is made Active again within the same millisecond — on the lab pair the release
// was decided at .4244 and recorded at .4249, after the promotion at ~.4245 — so
// no comparison of the two moments can tell the release from a newer one. The
// pass releases because the node is not the Active, not because it was told to,
// and must not arm the protection at all.
//
// So this asserts there is no record, with the node left Passive: promoting it in
// the test would be strictly later than the release, and the promotion check
// would discard a record that, live, it cannot see past.
func TestTheReconcilePassReleaseDoesNotArmTheProtection(t *testing.T) {
	s, monitor, iface, group := newReleaseTestServer(t)
	s.memberList.GetMemberByID("node-a").SetStatus(membership.StatusPassive)

	s.runVIPReconcile(vipReconcileSnapshot{localID: "node-a"})

	if _, released := monitor.RestorableIPs(iface, group); len(released) != 0 {
		t.Errorf("the reconcile pass recorded its own release of %v as one this node was told to make", released)
	}
}

// The guard: a release requested over the RPC is one this node was told to make,
// and an Active node keeps it protected (#60).
func TestAnRPCReleaseStillArmsTheProtection(t *testing.T) {
	s, monitor, iface, group := newReleaseTestServer(t)
	s.memberList.GetMemberByID("node-a").SetStatus(membership.StatusActive)

	_, _ = s.BringDownIP(context.Background(), &rpc.DownIpRequest{Iface: iface, Ips: group[1:]})

	if _, released := monitor.RestorableIPs(iface, group); len(released) != 1 || released[0] != group[1] {
		t.Errorf("released = %v, want %v protected after an RPC release on an Active node (#60)", released, group[1:])
	}
}
