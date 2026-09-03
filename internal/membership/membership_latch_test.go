package membership

import (
	"context"
	"io"
	"net"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// The deep membership check runs every fifth cycle and its result latches until
// a later deep check clears it. The latch exists so a node running its own
// isolated cluster stays unreachable even though it answers a TCP dial.
//
// It used to latch on any failure at all, because the check returned a bare
// bool over seven unrelated causes. A two-second gRPC deadline on a loaded VM
// therefore cost four further cycles of Unknown after the peer was answering
// again, which is what made an lb_api CI pipeline report a node unknown on odd
// occasions.
//
// The contract now has three parts, and the middle one was got wrong once
// already:
//
//   - REJECTED (not in the peer's memberlist, or a different cluster token) is
//     a conclusion about the peer's cluster. It latches until a later deep
//     check confirms, because a passing TCP dial says nothing about which
//     cluster answered.
//   - UNRESOLVED (a deadline, a transport failure, local misconfiguration) is
//     not a conclusion. It must not latch like a rejection -- but it must not
//     be dropped either. An earlier version dropped it, so the four cheap
//     cycles that follow answered on a TCP dial alone and a frozen-but-
//     listening peer read healthy four cycles in five. Measured on the docker
//     rig: frozen 14s, reported Active for 6.2s of it. So an unresolved check
//     keeps the node unreachable AND forces a deep check next cycle, which
//     recovers a transient in about one cycle and keeps a wedged peer Unknown
//     for as long as it stays wedged.
//   - CONFIRMED clears both.

// listeningPeer returns a member pointed at a socket that accepts and holds
// connections, so checkNodeConnectivity's TCP dial succeeds. That is the case
// the latch is about: reachable at the transport, and possibly not ours.
func listeningPeer(t *testing.T, id, hostname string) *Member {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
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
	m := newAATestMember(id, hostname, StatusPassive, nil)
	m.IP, m.Port = host, port
	return m
}

// newLatchTestChecker builds a checker over the local node and one peer that is
// reachable by TCP, with the deep check driven by the returned pointer.
func newLatchTestChecker(t *testing.T) (*HealthChecker, *Member, *membershipVerdict) {
	t.Helper()

	peer := listeningPeer(t, "node-peer", "host-peer")
	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-passive",
			LocalNode:           "node-local",
			ClusterToken:        "token-a",
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{},
		Nodes: map[string]*config.Node{
			"node-local": {Hostname: "host-local", IP: "127.0.0.1", Port: "9999"},
			"node-peer":  {Hostname: "host-peer", IP: peer.IP, Port: peer.Port},
		},
	}

	local := newAATestMember("node-local", "host-local", StatusActive, nil)
	local.IP, local.Port = "127.0.0.1", "9999"

	ml := newAATestMemberList(cfg, local, peer)
	h := NewHealthChecker(ml, log.New(io.Discard))

	verdict := membershipConfirmed
	h.deepCheck = func(*Member) membershipVerdict { return verdict }
	return h, peer, &verdict
}

// runCycles drives whole cycles. The deep check fires on every fifth, so five
// cycles is exactly one deep check followed by four cheap ones.
func runCycles(h *HealthChecker, n int) {
	for i := 0; i < n; i++ {
		h.performHealthChecks()
	}
}

func statusOf(m *Member) MemberStatus {
	m.Lock()
	defer m.Unlock()
	return m.Status
}

// runUntilUnknown advances cycles until the peer reads Unknown, up to the
// deep-check period, and reports how many it took.
//
// The latency is inherent and worth naming: a peer that stops answering gRPC
// while still accepting TCP is invisible until the next deep check, which is up
// to five cycles away. The docker rig measures the same thing -- frozen at
// 11:27:02.8, first reported Unknown at 11:27:06.5.
func runUntilUnknown(t *testing.T, h *HealthChecker, peer *Member, limit int) int {
	t.Helper()
	for i := 1; i <= limit; i++ {
		h.performHealthChecks()
		if statusOf(peer) == StatusUnknown {
			return i
		}
	}
	t.Fatalf("the peer never read Unknown within %d cycles", limit)
	return 0
}

// TestAnUnresolvedDeepCheckKeepsTheNodeUnknownWhileItStaysUnresolved is the
// regression the docker rig caught.
//
// A peer that answers a TCP dial but cannot answer gRPC is a wedged daemon, and
// must read Unknown for as long as that is true. An earlier version of this fix
// dropped the unresolved state instead of carrying it, so the four cheap cycles
// that follow reported the peer healthy on the strength of the dial: frozen for
// 14s on the rig, reported Active for 6.2s of it.
func TestAnUnresolvedDeepCheckKeepsTheNodeUnknownWhileItStaysUnresolved(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	runCycles(h, 5)
	if got := statusOf(peer); got == StatusUnknown {
		t.Fatalf("the peer never came up (status %s); its listener is not accepting",
			StatusToString(got))
	}

	// The peer freezes: TCP still accepts, gRPC never answers.
	*verdict = membershipUnverified
	runUntilUnknown(t, h, peer, 6)

	// From here every cycle must keep it Unknown, including the four in each
	// period that would otherwise be a cheap TCP dial.
	for i := 0; i < 12; i++ {
		h.performHealthChecks()
		if got := statusOf(peer); got != StatusUnknown {
			t.Fatalf("cycle %d after the freeze was detected reported %s. A "+
				"wedged-but-listening peer must not be reported healthy on a TCP "+
				"dial alone", i+1, StatusToString(got))
		}
	}
}

// TestAnUnresolvedNodeIsDeepCheckedEveryCycle is the mechanism behind the test
// above, asserted directly: once a deep check fails to conclude, the node stays
// on the expensive path instead of waiting out the five-cycle period.
func TestAnUnresolvedNodeIsDeepCheckedEveryCycle(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	var deepChecks int
	h.deepCheck = func(*Member) membershipVerdict {
		deepChecks++
		return *verdict
	}

	// Reach the first scheduled deep check with the peer already unresolved.
	*verdict = membershipUnverified
	runUntilUnknown(t, h, peer, 6)

	// Every one of the next eight cycles must deep-check.
	before := deepChecks
	for i := 0; i < 8; i++ {
		h.performHealthChecks()
	}
	if got := deepChecks - before; got != 8 {
		t.Errorf("%d deep checks across 8 cycles after the node became "+
			"unresolved, want 8 — it must be re-checked every cycle, not every "+
			"fifth", got)
	}
}

// TestAnUnresolvedDeepCheckRecoversOnTheNextCycle is the CI flake, and the
// reason the forced re-check is worth its extra gRPC call.
//
// Before this, a single deadline cost four further cycles of Unknown while the
// peer was answering again -- measured at 4.3s on a four-node lab cluster.
func TestAnUnresolvedDeepCheckRecoversOnTheNextCycle(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	runCycles(h, 5)
	if got := statusOf(peer); got == StatusUnknown {
		t.Fatalf("the peer never came up (status %s)", StatusToString(got))
	}

	*verdict = membershipUnverified
	runUntilUnknown(t, h, peer, 6)

	// The peer is fine again. Recovery must not wait out the five-cycle period.
	*verdict = membershipConfirmed
	recovered := -1
	for i := 1; i <= 5; i++ {
		h.performHealthChecks()
		if statusOf(peer) != StatusUnknown {
			recovered = i
			break
		}
	}
	if recovered == -1 {
		t.Fatal("the peer never recovered within five cycles of answering again")
	}
	if recovered > 1 {
		t.Errorf("recovered after %d cycles; an unresolved node is deep-checked "+
			"every cycle, so the first cycle after it answers again should clear it",
			recovered)
	}
}

// TestARejectedDeepCheckDoesLatch is the behaviour the latch exists for, and
// the one that must survive the fix: a peer in a different cluster answers a
// TCP dial perfectly well and must still read Unknown.
func TestARejectedDeepCheckDoesLatch(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	runCycles(h, 5)
	if got := statusOf(peer); got == StatusUnknown {
		t.Fatalf("the peer never came up (status %s)", StatusToString(got))
	}

	*verdict = membershipRejected
	runCycles(h, 5)
	if got := statusOf(peer); got != StatusUnknown {
		t.Fatalf("status = %s after a rejected deep check, want Unknown", StatusToString(got))
	}

	// Four more cheap cycles, every one of them a passing TCP dial. The latch
	// must hold: TCP says something answered, not that it is ours.
	for i := 0; i < 4; i++ {
		runCycles(h, 1)
		if got := statusOf(peer); got != StatusUnknown {
			t.Fatalf("cheap cycle %d cleared a rejected membership on the strength "+
				"of a TCP dial; status = %s", i+1, StatusToString(got))
		}
	}
}

// TestAConfirmedDeepCheckClearsTheLatch closes the loop: a peer that rejoins
// the cluster has to be able to recover.
func TestAConfirmedDeepCheckClearsTheLatch(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	*verdict = membershipRejected
	runCycles(h, 5)
	if got := statusOf(peer); got != StatusUnknown {
		t.Fatalf("status = %s after a rejected deep check, want Unknown", StatusToString(got))
	}

	*verdict = membershipConfirmed
	runCycles(h, 5)
	if got := statusOf(peer); got == StatusUnknown {
		t.Errorf("a confirmed deep check did not clear the latch; status = %s",
			StatusToString(got))
	}
}

// TestAnUnresolvedDeepCheckDoesNotClearARealRejection is the other half of "an
// unverified check leaves the latch alone".
//
// A peer in a foreign cluster that then goes slow must not be readmitted
// because the check that would have rejected it again failed to complete.
func TestAnUnresolvedDeepCheckDoesNotClearARealRejection(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	*verdict = membershipRejected
	runCycles(h, 5)
	if got := statusOf(peer); got != StatusUnknown {
		t.Fatalf("status = %s after a rejected deep check, want Unknown", StatusToString(got))
	}

	// The rejecting peer now stops answering the deep check entirely.
	*verdict = membershipUnverified
	runCycles(h, 5)

	// Its TCP dial still passes, so only the surviving latch can keep it
	// Unknown.
	runCycles(h, 1)
	if got := statusOf(peer); got != StatusUnknown {
		t.Errorf("a foreign-cluster peer was readmitted because the deep check "+
			"that would have rejected it again could not be completed; status = %s",
			StatusToString(got))
	}
}

// TestTheVerdictOfAnUnansweredPeerIsUnverified covers the real
// checkClusterMembership rather than the seam, for the case a unit test can
// actually produce: a peer that cannot be dialled at all.
//
// It must be unverified, not rejected. This is the arm a loaded CI runner hits.
func TestTheVerdictOfAnUnansweredPeerIsUnverified(t *testing.T) {
	h, peer, _ := newLatchTestChecker(t)
	h.deepCheck = nil // the real thing

	// A port nothing is listening on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	host, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", l.Addr(), err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	peer.Lock()
	peer.IP, peer.Port = host, port
	peer.Unlock()

	if got := h.checkClusterMembership(peer); got != membershipUnverified {
		t.Errorf("verdict for an undialable peer = %s, want unverified. Anything "+
			"else latches a transport failure as a statement about the peer's "+
			"cluster", got)
	}
}

func TestTheVerdictOfAPeerWithNoAddressIsUnverified(t *testing.T) {
	h, peer, _ := newLatchTestChecker(t)
	h.deepCheck = nil

	peer.Lock()
	peer.IP, peer.Port = "", ""
	peer.Unlock()

	if got := h.checkClusterMembership(peer); got != membershipUnverified {
		t.Errorf("verdict for a peer with no address = %s, want unverified — a "+
			"local misconfiguration is not a foreign cluster", got)
	}
}

// answeringPeer is a real gRPC peer that answers HealthCheck with a canned
// response, so the verdicts that require a peer to actually reply can be tested
// against checkClusterMembership itself rather than through the seam.
//
// The seam covers what the caller does with a verdict; this covers how the
// verdict is reached. Without it, "a token mismatch latches" — which is the
// entire reason the latch exists — was only ever asserted through a stub, and
// removing the real check passed every test.
type answeringPeer struct {
	rpc.UnimplementedServerServer
	reply *rpc.HealthCheckResponse
}

func (p *answeringPeer) HealthCheck(context.Context, *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	return p.reply, nil
}

// servePeer starts an answeringPeer and points the given member at it.
func servePeer(t *testing.T, member *Member, reply *rpc.HealthCheckResponse) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, &answeringPeer{reply: reply})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", ln.Addr(), err)
	}
	member.Lock()
	member.IP, member.Port = host, port
	member.Unlock()
}

// TestTheVerdictsThatNeedARealPeer covers the three conclusions that require an
// answer, against the real checkClusterMembership.
func TestTheVerdictsThatNeedARealPeer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply *rpc.HealthCheckResponse
		want  membershipVerdict
		why   string
	}{
		{
			name:  "agrees and echoes our token",
			reply: &rpc.HealthCheckResponse{Success: true, ClusterToken: "token-a"},
			want:  membershipConfirmed,
			why:   "the peer answered and is in this cluster",
		},
		{
			name:  "does not have us in its memberlist",
			reply: &rpc.HealthCheckResponse{Success: false, Message: "Node not found with ID: x"},
			want:  membershipRejected,
			why: "an asymmetric membership is a real condition on an appliance " +
				"whose node id is re-minted by lbcli setup, and must latch",
		},
		{
			name:  "echoes a different cluster token",
			reply: &rpc.HealthCheckResponse{Success: true, ClusterToken: "token-b"},
			want:  membershipRejected,
			why:   "a peer running its own isolated cluster is the reason the latch exists",
		},
		{
			name: "echoes no token at all",
			// An older peer that does not send one. The check only compares
			// when both sides have a token, so this must not be a rejection.
			reply: &rpc.HealthCheckResponse{Success: true, ClusterToken: ""},
			want:  membershipConfirmed,
			why:   "an absent token is not a mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, peer, _ := newLatchTestChecker(t)
			h.deepCheck = nil // the real thing
			servePeer(t, peer, tc.reply)

			if got := h.checkClusterMembership(peer); got != tc.want {
				t.Errorf("verdict = %s, want %s — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestAConfirmedDeepCheckReturnsTheNodeToTheCheapPath bounds the cost of the
// forced re-check.
//
// While a node is unresolved it is deep-checked every cycle, which is the point.
// A confirmation has to put it back on the five-cycle schedule, or any node that
// ever suffered one blip is deep-checked every cycle for the life of the daemon
// -- five times the gRPC load, permanently, for a peer that is perfectly
// healthy. Status alone cannot see that, which is why this counts calls.
func TestAConfirmedDeepCheckReturnsTheNodeToTheCheapPath(t *testing.T) {
	h, peer, verdict := newLatchTestChecker(t)

	var deepChecks int
	h.deepCheck = func(*Member) membershipVerdict {
		deepChecks++
		return *verdict
	}

	*verdict = membershipUnverified
	runUntilUnknown(t, h, peer, 6)

	// It answers again.
	*verdict = membershipConfirmed
	h.performHealthChecks()
	if statusOf(peer) == StatusUnknown {
		t.Fatal("the peer did not recover on the cycle after it answered again")
	}

	// Ten further cycles should now contain at most two scheduled deep checks.
	before := deepChecks
	for i := 0; i < 10; i++ {
		h.performHealthChecks()
	}
	if got := deepChecks - before; got > 3 {
		t.Errorf("%d deep checks across 10 cycles after the node was confirmed, "+
			"want at most 3 — a recovered node must go back to the five-cycle "+
			"schedule rather than staying on the expensive path", got)
	}
}
