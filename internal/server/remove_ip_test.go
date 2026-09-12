package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// removeIPIface is groupDeleteIface's reason applied here: a name over the
// kernel's 15-character limit, so it is not merely absent from the test host but
// unnameable. Every local netlink call fails on it rather than touching the
// machine's own addresses.
const removeIPIface = groupDeleteIface

const (
	removeIPLocal  = "local-node"
	removeIPTarget = "10.0.0.2/24"
)

// newRemoveIPTestServer builds an active-passive Server that can take a real
// RemoveIPFromGroup: a config on disk under t.TempDir() so Save() succeeds, a
// three-address group assigned to the local node and to every peerAddr, and the
// local node Active holding the whole group — which is what active-passive means
// and what makes the removed address one this node is recorded as serving.
func newRemoveIPTestServer(t *testing.T, peerAddrs ...string) *Server {
	t.Helper()

	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	groupIPs := []string{"10.0.0.1/24", removeIPTarget, "10.0.0.3/24"}

	nodes := map[string]*config.Node{
		removeIPLocal: {
			Hostname: removeIPLocal,
			IP:       "127.0.0.1",
			Port:     "49151",
			IPGroups: map[string][]string{removeIPIface: {"group1"}},
		},
	}
	nodeOrder := []string{removeIPLocal}
	for i, addr := range peerAddrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("SplitHostPort(%s): %v", addr, err)
		}
		id := fmt.Sprintf("peer-%d", i)
		nodes[id] = &config.Node{
			Hostname: "peer-" + addr,
			IP:       host,
			Port:     port,
			IPGroups: map[string][]string{removeIPIface: {"group1"}},
		}
		nodeOrder = append(nodeOrder, id)
	}

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-passive",
			LocalNode:           removeIPLocal,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": append([]string(nil), groupIPs...)},
		Nodes:  nodes,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for _, id := range nodeOrder {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
	}
	// Active-passive: one Active holding the whole group, every peer passive.
	local := ml.GetMemberByID(removeIPLocal)
	local.Status = membership.StatusActive
	local.SetActiveIPs(append([]string(nil), groupIPs...))

	return &Server{
		config:     cfg,
		logger:     logger,
		memberList: ml,
		ipMonitor:  membership.NewIPMonitor(ml, logger),
	}
}

// groupIPs returns the addresses group1 currently holds.
func groupIPs(s *Server) []string {
	s.RLock()
	defer s.RUnlock()

	return append([]string(nil), s.config.Groups["group1"]...)
}

// Regression for the report behind this change: an address removed from a group
// stayed up on the interface, `pulsectl status` kept listing it under Active IPs,
// and no group listed it at all.
//
// The handler brought the address down best effort, turned any failure into a
// warning, and removed it from the group regardless. That is docs/TEST-PLAN.md
// defect #104 — #59 one address at a time — and it needs no unreachable peer to bite — once the address
// is out of every configured group, surplusFloatingIPs cannot compute it (it
// scans configured groups only) and in active-passive nothing releases anything
// anyway. Whatever went wrong with that one bring-down was therefore permanent.
//
// A release that cannot be confirmed must leave the address configured, which is
// the recoverable state: it stays in the expectation set, and a retried
// remove-ip finishes the job.
func TestRemoveIPKeepsTheAddressConfiguredWhenTheReleaseCannotBeConfirmed(t *testing.T) {
	peer := &releasingPeer{refuse: true}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	resp, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{
		GroupName: "group1",
		Ip:        removeIPTarget,
	})
	if err != nil {
		t.Fatalf("RemoveIPFromGroup: %v", err)
	}
	if resp.Success {
		t.Error("Success = true over a release that could not be confirmed; " +
			"the address is out of the config and no pass can ever see it again")
	}
	if got := groupIPs(s); !slices.Contains(got, removeIPTarget) {
		t.Errorf("group1 = %v, want %s still configured so it stays accounted for", got, removeIPTarget)
	}
}

// The release has to go out while the address is still configured. Removing it
// first and releasing after is the same strand from the other direction: a
// release that fails then has nothing left to retry against.
func TestRemoveIPReleasesBeforeTheAddressLeavesTheConfig(t *testing.T) {
	var configuredDuringRelease bool

	peer := &releasingPeer{}
	var s *Server
	peer.inspect = func() {
		configuredDuringRelease = slices.Contains(groupIPs(s), removeIPTarget)
	}
	s = newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	resp, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{
		GroupName: "group1",
		Ip:        removeIPTarget,
	})
	if err != nil {
		t.Fatalf("RemoveIPFromGroup: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q), want the removal to complete", resp.Message)
	}

	if !configuredDuringRelease {
		t.Error("the address had already left the config when the release went out; " +
			"a release that fails at that point has nothing left to retry against")
	}
	if got := peer.released(); !slices.Equal(got, []string{removeIPTarget}) {
		t.Errorf("peer released %v, want exactly %s", got, removeIPTarget)
	}
	if got := groupIPs(s); slices.Contains(got, removeIPTarget) {
		t.Errorf("group1 = %v, want %s removed once its release was confirmed", got, removeIPTarget)
	}
}

// The other addresses of the group are not touched: this is one address leaving,
// not a group being torn down.
func TestRemoveIPLeavesTheRestOfTheGroupAlone(t *testing.T) {
	peer := &releasingPeer{}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	if _, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{
		GroupName: "group1",
		Ip:        removeIPTarget,
	}); err != nil {
		t.Fatalf("RemoveIPFromGroup: %v", err)
	}

	want := []string{"10.0.0.1/24", "10.0.0.3/24"}
	if got := groupIPs(s); !slices.Equal(got, want) {
		t.Errorf("group1 = %v, want %v", got, want)
	}
	if got := peer.released(); !slices.Equal(got, []string{removeIPTarget}) {
		t.Errorf("peer released %v, want only the address asked for", got)
	}
}

// The plan must always visit the local node, for planGroupRelease's reason: it
// is the one node whose interfaces can be read rather than believed, and it is
// where the member's assignment list gets corrected (defect #58) — the list
// `pulsectl status` prints as Active IPs.
func TestPlanIPReleaseAlwaysVisitsTheLocalNode(t *testing.T) {
	s := newRemoveIPTestServer(t)

	// Nothing is recorded as held, which is the active-passive norm for a node
	// promoted by election rather than by a mode switch.
	s.memberList.GetMemberByID(removeIPLocal).SetActiveIPs(nil)

	s.RLock()
	targets := s.planIPRelease("group1", removeIPTarget)
	s.RUnlock()

	if len(targets) != 1 {
		t.Fatalf("planned %d targets, want the local node: %+v", len(targets), targets)
	}
	if !targets[0].local || targets[0].nodeID != removeIPLocal {
		t.Fatalf("planned %+v, want the local node", targets[0])
	}
	if !slices.Equal(targets[0].candidates, []string{removeIPTarget}) {
		t.Errorf("candidates = %v, want the one address so the kernel check is scoped to it",
			targets[0].candidates)
	}
}

// An address another still-configured group provides on the same interface is
// left up. Nothing in the CLI can create that overlap, but config.json is
// written by the appliance too (defect #3), and tearing down an address a live
// group still serves would be an outage.
func TestRemoveIPDoesNotReleaseAnAddressAnotherGroupStillProvides(t *testing.T) {
	peer := &releasingPeer{}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	s.Lock()
	s.config.Groups["group2"] = []string{removeIPTarget}
	for _, node := range s.config.Nodes {
		node.IPGroups[removeIPIface] = append(node.IPGroups[removeIPIface], "group2")
	}
	s.Unlock()

	if _, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{
		GroupName: "group1",
		Ip:        removeIPTarget,
	}); err != nil {
		t.Fatalf("RemoveIPFromGroup: %v", err)
	}

	if got := peer.released(); len(got) > 0 {
		t.Errorf("released %v, but group2 still provides %s on the same interface", got, removeIPTarget)
	}
	if got := groupIPs(s); slices.Contains(got, removeIPTarget) {
		t.Errorf("group1 = %v, want the address removed from this group", got)
	}
}

// `--ip` is documented as taking an *optional* subnet mask. The lookup defaulted
// a bare IPv4 address to /32 and compared strings, so removing 10.0.0.2 from a
// group holding 10.0.0.2/24 matched nothing — and the no-match path returns
// success. The operator was told the address was gone while it stayed configured
// and up, which is the same operator-visible lie from a different direction.
func TestRemoveIPMatchesAConfiguredEntryWrittenWithoutTheSameMask(t *testing.T) {
	peer := &releasingPeer{}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	resp, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{
		GroupName: "group1",
		Ip:        "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("RemoveIPFromGroup: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q)", resp.Message)
	}
	if got := groupIPs(s); slices.Contains(got, removeIPTarget) {
		t.Errorf("group1 = %v, want %s removed: a bare address means the configured one",
			got, removeIPTarget)
	}
	if got := peer.released(); !slices.Equal(got, []string{removeIPTarget}) {
		t.Errorf("peer released %v, want the configured form %s", got, removeIPTarget)
	}
	if len(resp.Warnings) == 0 {
		t.Error("no warning: the operator asked for one spelling and got another")
	}
}

func TestMatchGroupIP(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}

	t.Run("an exact entry wins", func(t *testing.T) {
		got, warnings, err := matchGroupIP(group, "10.0.0.2/24")
		if err != nil || got != "10.0.0.2/24" || len(warnings) != 0 {
			t.Fatalf("= %q, %v, %v", got, warnings, err)
		}
	})

	t.Run("a bare address resolves to the configured form", func(t *testing.T) {
		got, warnings, err := matchGroupIP(group, "10.0.0.2")
		if err != nil || got != "10.0.0.2/24" {
			t.Fatalf("= %q, %v", got, err)
		}
		if len(warnings) == 0 {
			t.Error("resolving to a different spelling is worth saying out loud")
		}
	})

	t.Run("a mask the group does not hold still resolves", func(t *testing.T) {
		// The one that produced the silent no-op: /32 against a /24 entry. It is
		// also the mask mismatch that makes netlink answer EADDRNOTAVAIL while the
		// address stays up, so matching it here is what stops the release being
		// asked for in a form the kernel will not act on.
		got, _, err := matchGroupIP(group, "10.0.0.2/32")
		if err != nil || got != "10.0.0.2/24" {
			t.Fatalf("= %q, %v", got, err)
		}
	})

	t.Run("an address the group does not hold is not found", func(t *testing.T) {
		got, _, err := matchGroupIP(group, "10.0.0.9/24")
		if err != nil || got != "" {
			t.Fatalf("= %q, %v, want no match and no error", got, err)
		}
	})

	t.Run("a malformed address is rejected", func(t *testing.T) {
		if _, _, err := matchGroupIP(group, "not-an-ip"); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("two prefixes of one address are refused rather than guessed", func(t *testing.T) {
		both := []string{"10.0.0.2/24", "10.0.0.2/32"}
		if _, _, err := matchGroupIP(both, "10.0.0.2"); err == nil {
			t.Fatal("want an error: tearing down the wrong one is an outage")
		}
	})
}

// Regression for docs/TEST-PLAN.md defect #105, which is what the field report
// actually was: the remove worked, and a stale post-load VIP reconcile undid it a
// second later.
//
// Measured on MC-LB-3-node-1 — 23:48:26 `Successfully brought down IP
// 10.20.70.78/24`, expectations correctly down to one address; 23:48:27 `RPC
// BringUpIP` puts it straight back and re-adds the expectation. The appliance
// issues a full RESYNC immediately before each CLI mutation, and that RESYNC is
// what schedules the pass — so every `remove-ip` lands inside the window of a
// pass that captured the address list before the removal existed.
//
// A pass is a convergence step. It has to read the config when it acts, not when
// it was scheduled.
func TestVIPReconcilePlanFollowsTheConfigAtRunTime(t *testing.T) {
	peer := &releasingPeer{}
	s := newRemoveIPTestServer(t, startReleasingPeer(t, peer))

	// What the pass would have been handed at schedule time.
	scheduled, claim := s.vipReconcilePlanNow(removeIPLocal)
	if !claim {
		t.Fatal("the local node is Active, so this pass claims")
	}
	if !slices.Contains(scheduled[removeIPIface], removeIPTarget) {
		t.Fatalf("plan = %v, want the address before it is removed", scheduled)
	}

	if _, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{
		GroupName: "group1",
		Ip:        removeIPTarget,
	}); err != nil {
		t.Fatalf("RemoveIPFromGroup: %v", err)
	}

	// The pass runs after the removal. It must converge on the config that exists
	// now, not re-place an address nothing references.
	planned, _ := s.vipReconcilePlanNow(removeIPLocal)
	if slices.Contains(planned[removeIPIface], removeIPTarget) {
		t.Errorf("plan = %v, want %s gone: the pass would bring back an address "+
			"no group configures, and re-add it to the monitor's expectations "+
			"where nothing in active-passive ever recomputes it downward",
			planned, removeIPTarget)
	}
	if !slices.Contains(planned[removeIPIface], "10.0.0.1/24") {
		t.Errorf("plan = %v, want the rest of the group still claimed", planned)
	}
}

// The scheduler must not carry addresses at all — that is the property the fix
// rests on, and a field added back to the snapshot would silently restore the
// defect.
func TestVIPReconcileSnapshotCarriesOnlyTheNode(t *testing.T) {
	if n := reflect.TypeOf(vipReconcileSnapshot{}).NumField(); n != 1 {
		t.Fatalf("vipReconcileSnapshot has %d fields, want only localID: anything "+
			"else is config captured at schedule time, which is defect #105", n)
	}
}
