package server

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// Regression for GitHub #272 (docs/TEST-PLAN.md defect #117). `cluster mode set
// --mode active-passive`, sent from active-active to the node that becomes the
// sole Active, wedged that node's daemon. SetMode holds s.Lock() and called
// refreshLocalMonitorExpectedIPs under it; with addresses missing — always true
// on the consolidating node, which has yet to take its peers' share — that
// reached bringIPsOnNodeUp -> currentConfig() -> s.RLock(), a re-acquisition of
// a lock the same goroutine held. The CLI call never returned, the peer stayed
// in active-active and took the whole group as well: 118 addresses on both
// nodes for ~6 minutes, ended only by kill -9.
//
// In the daemon that is a hang; under `go test` pulselock panics on the
// reentrancy instead. Either one fails this test.
func TestSetModeToActivePassiveOnTheConsolidatingNodeReturns(t *testing.T) {
	runSetModeDeadlockCase(t, "active-active", "active-passive", map[string]membership.Claim{
		// The active-active split: both Active, the local node holding more, so it
		// is the one the switch consolidates onto.
		"node-a": {Status: membership.StatusActive, ActiveIPs: []string{"10.0.0.1/24", "10.0.0.2/24"}},
		"node-b": {Status: membership.StatusActive, ActiveIPs: []string{"10.0.0.3/24"}},
	}, membership.StatusActive, membership.StatusPassive)
}

// The other direction went through the same locked refresh. An Active node with
// one of its addresses missing — a NetworkManager re-apply is enough — wedged the
// same way on a switch to active-active.
func TestSetModeToActiveActiveOnAnActiveNodeMissingAnAddressReturns(t *testing.T) {
	runSetModeDeadlockCase(t, "active-passive", "active-active", map[string]membership.Claim{
		"node-a": {Status: membership.StatusActive, ActiveIPs: []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}},
		"node-b": {Status: membership.StatusPassive},
	}, membership.StatusActive, membership.StatusPassive)
}

func runSetModeDeadlockCase(t *testing.T, from, to string, claims map[string]membership.Claim,
	wantLocal, wantPeer membership.MemberStatus) {
	t.Helper()
	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	// A peer nothing listens for, so the post-switch broadcast and release to it
	// fail at once rather than waiting out a deadline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, peerPort, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	// Over the kernel's 15-character limit, so every address is missing and the
	// bring-up fails at netlink instead of touching this machine.
	const iface = "pulseha-no-such-interface"
	const localID, peerID = "node-a", "node-b"
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                from,
			LocalNode:           localID,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": group},
		Nodes: map[string]*config.Node{
			localID: {Hostname: "node-4", IP: "127.0.0.1", Port: "1", IPGroups: map[string][]string{iface: {"group1"}}},
			peerID:  {Hostname: "node-5", IP: "127.0.0.1", Port: peerPort, IPGroups: map[string][]string{iface: {"group1"}}},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id, claim := range claims {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		ml.GetMemberByID(id).SetClaim(claim)
	}
	monitor := membership.NewIPMonitor(ml, logger)
	t.Cleanup(monitor.Stop)
	s := &Server{config: cfg, logger: logger, memberList: ml, ipMonitor: monitor}

	type result struct {
		resp  *rpc.SetModeResponse
		err   error
		panic any
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{panic: p}
			}
		}()
		resp, err := s.SetMode(context.Background(), &rpc.SetModeRequest{Mode: to})
		done <- result{resp: resp, err: err}
	}()

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetMode did not return within 5s: the daemon would be wedged with the server lock held")
	}
	if r.panic != nil {
		t.Fatalf("SetMode re-took the server lock it holds: %v", r.panic)
	}
	if r.err != nil || r.resp == nil || !r.resp.Success {
		t.Fatalf("SetMode = %v, %v; want success", r.resp, r.err)
	}

	// The lock must be free afterwards, or every other path is wedged behind it.
	locked := make(chan struct{})
	go func() {
		s.Lock()
		s.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the server lock is still held 5s after SetMode returned")
	}

	// And the refresh still happens — it moved, it was not dropped. The local node
	// owns the whole group in both cases, so the monitor must come to expect all of it.
	deadline := time.Now().Add(5 * time.Second)
	for len(monitor.GetExpectedIPs(iface)) != len(group) {
		if time.Now().After(deadline) {
			t.Fatalf("monitor expects %v on %s 5s after the switch, want the whole group %v: the refresh never ran",
				monitor.GetExpectedIPs(iface), iface, group)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := ml.GetMemberByID(localID).GetStatus(); got != wantLocal {
		t.Errorf("local status = %v, want %v", membership.StatusToString(got), membership.StatusToString(wantLocal))
	}
	if got := ml.GetMemberByID(peerID).GetStatus(); got != wantPeer {
		t.Errorf("peer status = %v, want %v", membership.StatusToString(got), membership.StatusToString(wantPeer))
	}
}
