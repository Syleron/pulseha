package server

import (
	"net"
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/utils"
)

// The probe confirmPeerReleasedIPs uses to tell "nothing is there" from "something is there
// but stuck", pinned directly.
//
// Promotion safety turns entirely on that distinction, and before this the RPC could not
// supply it: grpc.NewClient never dials, so a blackholed peer and a peer that accepted the
// call and hung both surfaced as DeadlineExceeded on the first RPC. The blackhole was read as
// the hang, `canPromoteWithoutConfirmedRelease` saw peerStillAlive and refused, and the
// floating IP stayed dark for as long as the node stayed dead — at any cluster size, since
// peerStillAlive short-circuits ahead of the quorum check.
//
// Asserted at the same layer the code decides at: whether a TCP connection to the peer's
// address completes inside transportProbeTimeout. Not gRPC's READY state, which reports the
// HTTP/2 handshake and so would read a wedged-but-live daemon as gone.
func TestPeerSocketReachability(t *testing.T) {
	dial := func(addr string) error {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("split %q: %v", addr, err)
		}
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort(utils.SanitizeIPv6(host), port), transportProbeTimeout)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}

	t.Run("a listening socket is reachable even when nothing answers on it", func(t *testing.T) {
		// Accepted but never spoken to. This is the wedged-Active shape: the daemon's
		// goroutines are stuck, but the kernel still completes the handshake on its behalf.
		// It MUST come back reachable, because promoting over a node that may still hold every
		// address is TC-6, and this is the check that keeps that case safe.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()

		if err := dial(ln.Addr().String()); err != nil {
			t.Errorf("expected a listening socket to be reachable, got %v", err)
		}
	})

	t.Run("a closed port is unreachable", func(t *testing.T) {
		// The `systemctl stop` shape, and the only failure the old code handled — a refusal
		// reaches the RPC as Unavailable. Asserted so the probe does not regress it.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close()

		if err := dial(addr); err == nil {
			t.Error("expected a closed port to be unreachable")
		}
	})

	// The blackhole case -- a powered-off node or a firewall DROP, which produces no
	// refusal and is exactly what used to strand the promotion -- is NOT asserted here.
	// It cannot be simulated portably: a reserved TEST-NET address is answered rather
	// than dropped on any host behind a VPN or intercepting proxy (203.0.113.1:9083
	// connects in 20ms on this developer machine), so a test built on one asserts the
	// network's behaviour, not the code's.
	//
	// It is covered where the drop can actually be caused: docs/TEST-PLAN.md TC-9, which
	// severs the cluster link with `iptables -j DROP` and asserts the failover that used
	// to abort.

	t.Run("the probe timeout is sized for a handshake, not an answer", func(t *testing.T) {
		// Guards the constant against being retuned toward DemotionTimeoutFor's scale. Too
		// short strands a promotion on one lost SYN; long enough to cover an application
		// response would hold every failover open for a node that is simply gone.
		if transportProbeTimeout < time.Second {
			t.Errorf("transportProbeTimeout = %v, too short to absorb a retransmit", transportProbeTimeout)
		}
		if transportProbeTimeout > 10*time.Second {
			t.Errorf("transportProbeTimeout = %v, that is an application-response budget, not a handshake",
				transportProbeTimeout)
		}
	})
}
