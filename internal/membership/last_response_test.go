package membership

import (
	"io"
	"net"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// LastHCResponse is the only field that can tell a blip from a corpse, and it
// used to be stamped unconditionally in two places: immediately before the
// branch that decides a member is unreachable, and again for every member in
// the snapshot by the heartbeat convergence nudge, "for consistent display".
//
// So it recorded whether a member had ever responded, never when it last did,
// and every consumer measuring silence with it was reading a constant. Three
// consumers do:
//
//   - clusterCoordinator treats Unknown-but-within-grace as eligible, so a
//     permanently dead node held the role forever if it sorted lowest by id --
//     and the role gates the config reconcile re-broadcast, active-passive
//     split-brain consolidation and the reconciliation pass.
//   - redistributeOrphanedIPs counts an Unknown-but-within-grace node's
//     addresses as hosted, so the arm that clears addresses stranded on a
//     failed node was unreachable and they were never re-placed.
//   - selectBestCandidate awards a recency bonus that every candidate always
//     won, so it differentiated nothing.
//
// The failover check at ACTIVE_CHECK is unaffected and was never broken by
// this: it ORs the timestamp comparison with Status == StatusUnknown, and the
// status is what carries it.

// refusedAddr returns an address that refuses connections immediately, so a
// dial fails on refusal rather than waiting out the 500ms timeout.
func refusedAddr(t *testing.T) (host, port string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	host, port, err = net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", l.Addr(), err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return host, port
}

// newSilenceTestChecker builds a checker over the local node and one peer at a
// refused port. node-dead sorts before node-local, so it wins the coordinator
// role whenever it is considered eligible.
func newSilenceTestChecker(t *testing.T, groups map[string][]string) (*HealthChecker, *Member, *MemberList) {
	t.Helper()

	host, port := refusedAddr(t)
	if groups == nil {
		groups = map[string][]string{}
	}
	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-passive",
			LocalNode:           "node-local",
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000, // grace = 10s
		},
		Groups: groups,
		Nodes: map[string]*config.Node{
			"node-local": {Hostname: "host-local", IP: "127.0.0.1", Port: "9999",
				IPGroups: map[string][]string{"eth0": {"group1"}}},
			"node-dead": {Hostname: "host-dead", IP: host, Port: port,
				IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}

	local := newAATestMember("node-local", "host-local", StatusActive, nil)
	local.IP, local.Port = "127.0.0.1", "9999"
	local.LastHCResponse = time.Now()
	dead := newAATestMember("node-dead", "host-dead", StatusUnknown, nil)
	dead.IP, dead.Port = host, port

	ml := newAATestMemberList(cfg, local, dead)
	return NewHealthChecker(ml, log.New(io.Discard)), dead, ml
}

// TestAnUnreachableMemberKeepsItsLastResponse is the mechanism.
func TestAnUnreachableMemberKeepsItsLastResponse(t *testing.T) {
	h, dead, _ := newSilenceTestChecker(t, nil)

	silent := time.Now().Add(-30 * time.Second)
	dead.mu.Lock()
	dead.LastHCResponse = silent
	dead.mu.Unlock()

	// Several passes, including the heartbeat nudge's third-cycle stamp and a
	// deep check on the fifth.
	for i := 0; i < 6; i++ {
		h.performHealthChecks()
	}

	dead.mu.Lock()
	status, stamped := dead.Status, dead.LastHCResponse
	dead.mu.Unlock()

	if status != StatusUnknown {
		t.Fatalf("the peer was not found unreachable (status %s); its port is not "+
			"refusing and this test proves nothing", StatusToString(status))
	}
	if !stamped.Equal(silent) {
		t.Errorf("LastHCResponse moved to %v across six passes in which the member "+
			"never answered (was %v). It has to measure silence, not liveness-ever",
			stamped.Format(time.RFC3339Nano), silent.Format(time.RFC3339Nano))
	}
}

// TestAReachableMemberDoesGetStamped is the other half: the field must still
// advance on genuine contact, or every consumer sees permanent silence instead.
func TestAReachableMemberDoesGetStamped(t *testing.T) {
	h, peer, _ := newSilenceTestChecker(t, nil)

	// Give the peer something that accepts, so the TCP dial succeeds.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	host, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", l.Addr(), err)
	}

	before := time.Now().Add(-30 * time.Second)
	peer.mu.Lock()
	peer.IP, peer.Port = host, port
	peer.LastHCResponse = before
	peer.mu.Unlock()

	// One cheap cycle: a passing TCP dial.
	h.performHealthChecks()

	peer.mu.Lock()
	stamped := peer.LastHCResponse
	peer.mu.Unlock()
	if !stamped.After(before) {
		t.Errorf("LastHCResponse was not advanced for a member that answered "+
			"(still %v)", stamped.Format(time.RFC3339Nano))
	}
}

// TestTheCoordinatorRoleLeavesAnIndefinitelyDeadNode is the first consequence.
//
// This is the test that fails against the unconditional stamp, reporting an
// apparent silence of 0s against a 10s grace.
func TestTheCoordinatorRoleLeavesAnIndefinitelyDeadNode(t *testing.T) {
	h, dead, ml := newSilenceTestChecker(t, nil)

	dead.mu.Lock()
	dead.LastHCResponse = time.Now().Add(-30 * time.Second)
	dead.mu.Unlock()

	for i := 0; i < 6; i++ {
		h.performHealthChecks()
	}

	dead.mu.Lock()
	silence := time.Since(dead.LastHCResponse)
	dead.mu.Unlock()

	got := clusterCoordinator(ml.MembersSnapshot(), h.failoverGrace())
	if got == "node-dead" {
		t.Errorf("a node silent for %v still holds the coordinator role against a "+
			"grace of %v. It gates the config reconcile re-broadcast, active-passive "+
			"split-brain consolidation and the reconciliation pass, and can perform "+
			"none of them", silence.Round(time.Second), h.failoverGrace())
	}
	if got != "node-local" {
		t.Errorf("coordinator = %q, want node-local (the only reachable member)", got)
	}
}

// TestTheCoordinatorRoleStaysWithABrieflySilentNode is the behaviour the grace
// window exists for, and it must survive the fix. A coordinator part-way
// through a batch of moves is slow enough to miss a check; handing the role on
// there had the next node re-place addresses the first was still holding
// (docs/TEST-PLAN.md #2/#26).
func TestTheCoordinatorRoleStaysWithABrieflySilentNode(t *testing.T) {
	h, dead, ml := newSilenceTestChecker(t, nil)

	// Silent for two seconds against a ten-second grace: busy, not gone.
	dead.mu.Lock()
	dead.LastHCResponse = time.Now().Add(-2 * time.Second)
	dead.mu.Unlock()

	if got := clusterCoordinator(ml.MembersSnapshot(), h.failoverGrace()); got != "node-dead" {
		t.Errorf("coordinator = %q, want node-dead — a briefly silent node keeps "+
			"the role", got)
	}
}

// TestAddressesStrandedOnADeadNodeAreReclaimed is the second consequence, and
// the more serious of the two.
//
// redistributeOrphanedIPs counts a member's addresses as hosted when it is
// Active, Passive, or Unknown-but-within-grace, and clears them only in the
// arm below that. With the grace always satisfied the first arm always matched,
// so the clearing arm was unreachable: addresses on a permanently dead node
// stayed counted as hosted, orphanedGroupIPs never saw them, and nothing ever
// re-placed them. That is docs/TEST-PLAN.md #44's shape.
func TestAddressesStrandedOnADeadNodeAreReclaimed(t *testing.T) {
	stranded := []string{"10.0.0.1/24", "10.0.0.2/24"}
	h, dead, _ := newSilenceTestChecker(t, map[string][]string{"group1": stranded})

	dead.mu.Lock()
	dead.Status = StatusUnknown
	dead.ActiveIPs = append([]string{}, stranded...)
	dead.LastHCResponse = time.Now().Add(-30 * time.Second)
	dead.mu.Unlock()

	h.redistributeOrphanedIPs(h.members.MembersSnapshot())

	dead.mu.Lock()
	held := append([]string{}, dead.ActiveIPs...)
	dead.mu.Unlock()

	if len(held) != 0 {
		t.Errorf("a node silent for 30s against a %v grace still claims %v. "+
			"Those addresses count as hosted, so orphanedGroupIPs never reports "+
			"them and nothing re-places them", h.failoverGrace(), held)
	}
}

// TestAddressesOnABrieflySilentNodeAreNotReclaimed is the arm that must
// survive: reclaiming on the first missed check took addresses off a node that
// was only busy and brought them up elsewhere while it was still serving them.
func TestAddressesOnABrieflySilentNodeAreNotReclaimed(t *testing.T) {
	stranded := []string{"10.0.0.1/24", "10.0.0.2/24"}
	h, dead, _ := newSilenceTestChecker(t, map[string][]string{"group1": stranded})

	dead.mu.Lock()
	dead.Status = StatusUnknown
	dead.ActiveIPs = append([]string{}, stranded...)
	dead.LastHCResponse = time.Now().Add(-2 * time.Second)
	dead.mu.Unlock()

	h.redistributeOrphanedIPs(h.members.MembersSnapshot())

	dead.mu.Lock()
	held := len(dead.ActiveIPs)
	dead.mu.Unlock()

	if held != len(stranded) {
		t.Errorf("a node silent for only 2s against a %v grace lost its addresses "+
			"(%d of %d left); it may still be serving them",
			h.failoverGrace(), held, len(stranded))
	}
}

// TestTheHeartbeatNudgeDoesNotStampAnUnreachableMember covers the second of the
// two unconditional stamps, and needs a ServerReference to reach at all: the
// nudge is guarded by `h.server != nil`, so a checker without one never runs it
// and the first draft of these tests let the mutation through.
//
// The nudge fires every third *unchanged* cycle, so the peer has to settle at
// Unknown first -- a status change resets the counter.
func TestTheHeartbeatNudgeDoesNotStampAnUnreachableMember(t *testing.T) {
	h, dead, ml := newSilenceTestChecker(t, nil)
	h.server = &stubServer{members: ml}

	silent := time.Now().Add(-30 * time.Second)
	dead.mu.Lock()
	dead.LastHCResponse = silent
	dead.mu.Unlock()

	// Enough unchanged cycles for the nudge to have fired several times.
	for i := 0; i < 12; i++ {
		h.performHealthChecks()
	}

	// The nudge must actually have run, or this proves nothing.
	if h.server.(*stubServer).broadcastStates == nil {
		t.Fatal("the heartbeat nudge never fired, so this test cannot see whether " +
			"it stamps; check the checksWithoutChange%3 gate")
	}

	dead.mu.Lock()
	stamped := dead.LastHCResponse
	dead.mu.Unlock()
	if !stamped.Equal(silent) {
		t.Errorf("the heartbeat nudge advanced LastHCResponse to %v for a member "+
			"that has never answered (was %v). Its stated purpose is to align "+
			"peers, which the broadcast does; stamping was only ever for display",
			stamped.Format(time.RFC3339Nano), silent.Format(time.RFC3339Nano))
	}
}
