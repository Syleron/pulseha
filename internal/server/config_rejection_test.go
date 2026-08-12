package server

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// decliningPeer answers every ConfigSync with Success:false and a message the test
// chooses, so a test can distinguish the two very different reasons a real peer
// sends that: a payload it can never accept, and a save that happened to fail.
type decliningPeer struct {
	rpc.UnimplementedServerServer

	message string

	mu    sync.Mutex
	calls int
}

func (p *decliningPeer) ConfigSync(_ context.Context, _ *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	return &rpc.ConfigSyncResponse{Success: false, Message: p.message}, nil
}

func (p *decliningPeer) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func startDecliningPeer(t *testing.T, message string) (*decliningPeer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	peer := &decliningPeer{message: message}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// drainBroadcastTrigger empties the trigger channel so a directly-invoked
// broadcastConfigToPeersOnce behaves the way the broadcaster goroutine's invocation
// does.
//
// The pass abandons its retries when it sees a queued trigger, on the sound
// reasoning that a newer broadcast carries everything this one would. The real
// broadcaster receives from the channel before starting a pass, so that check only
// fires on a mutation that arrived *during* the pass. A test that calls the pass
// itself has to do the same receive, or it measures the supersede path while
// believing it is measuring the retry path.
func drainBroadcastTrigger(s *Server) {
	for {
		select {
		case <-s.broadcastTrigger:
		default:
			return
		}
	}
}

// outstandingPeers reports the peers the propagation bookkeeping currently
// considers behind, so a test can ask whether a failure was actually recorded
// rather than only whether it was logged.
func outstandingPeers(s *Server) []string {
	s.propagationMu.Lock()
	defer s.propagationMu.Unlock()

	if s.unpropagated == nil {
		return nil
	}
	return append([]string(nil), s.unpropagated.peers...)
}

// A peer whose *save* failed must stay in the retry set.
//
// broadcastConfigToPeersOnce deleted the peer from `pending` for every
// Success:false, on the rationale that "retrying the same bytes cannot change that
// answer". That holds for the unmarshal rejection and for a nil payload. It does
// not hold for `failed to save synchronized configuration` — ENOSPC, EIO, a
// read-only mount — which is transient and is the same reply. Because the peer left
// `pending`, recordUnpropagated never saw it, clearUnpropagated went on to report
// full propagation, and the only trace was a Debug line. That is #43's signature
// again: a diverged peer sitting behind a broadcast that reported success.
func TestATransientConfigSyncRejectionStaysInTheRetrySet(t *testing.T) {
	peer, addr := startDecliningPeer(t,
		"failed to save synchronized configuration: write /etc/pulseha/config.json: no space left on device")

	s := newPropagationTestServer(t, addr)

	s.Lock()
	s.config.Groups["group1"] = append(s.config.Groups["group1"], "10.0.0.50/24")
	s.markConfigDirty()
	s.Unlock()
	drainBroadcastTrigger(s)

	s.broadcastConfigToPeersOnce()

	if got := peer.callCount(); got < 2 {
		t.Errorf("the peer was called %d time(s); a transient save failure has to be "+
			"retried within the pass, not dropped after the first answer", got)
	}
	if got := outstandingPeers(s); len(got) == 0 {
		t.Errorf("no peer recorded as unpropagated after a save failure, so the " +
			"broadcaster believes this config reached every peer while the peer " +
			"holds none of it (defect #43's signature)")
	}
}

// The other half: a rejection that retrying genuinely cannot fix must not be
// retried, but must not be quiet either. The peer is diverged, so this is a
// terminal state worth an Error — and the broadcast must not go on to claim that
// every peer accepted the config.
func TestAPermanentConfigSyncRejectionIsNotRetriedButIsRecorded(t *testing.T) {
	peer, addr := startDecliningPeer(t, permanentRejectionPrefix+
		"failed to unmarshal configuration: invalid character 'x' looking for beginning of value")

	s := newPropagationTestServer(t, addr)

	// Something outstanding from an earlier round, so the test can tell that a
	// permanent rejection does not get counted as the repair landing.
	s.Lock()
	s.markConfigDirty()
	stamp := s.loadConfigStamp()
	s.Unlock()
	s.recordUnpropagated(stamp, []string{"peer-0"})

	s.broadcastConfigToPeersOnce()

	if got := peer.callCount(); got != 1 {
		t.Errorf("the peer was called %d times; a payload it can never accept must "+
			"not be re-sent", got)
	}
	if got := outstandingPeers(s); len(got) == 0 {
		t.Errorf("a permanent rejection cleared the propagation state, so the " +
			"broadcaster reports full propagation to a peer that rejected the config " +
			"outright")
	}
}

// The classification has to survive a peer that does not speak it. An older binary
// rejects an unparseable payload without the marker, and the safe reading of an
// unmarked rejection is the transient one: retrying four times and then saying so
// is noisy, whereas silently dropping the peer is the divergence this whole
// mechanism exists to surface.
func TestAnUnmarkedRejectionIsTreatedAsTransient(t *testing.T) {
	peer, addr := startDecliningPeer(t, "failed to unmarshal configuration: some older binary")

	s := newPropagationTestServer(t, addr)

	s.Lock()
	s.markConfigDirty()
	s.Unlock()
	drainBroadcastTrigger(s)

	s.broadcastConfigToPeersOnce()

	if got := peer.callCount(); got < 2 {
		t.Errorf("the peer was called %d time(s); an unmarked rejection has to be "+
			"read as transient, because a peer that cannot mark it is exactly the "+
			"peer whose answer cannot be trusted to be terminal", got)
	}
	if got := outstandingPeers(s); len(got) == 0 {
		t.Error("an unmarked rejection left nothing recorded as unpropagated")
	}
}

// isPermanentRejection must key off the marker rather than off the wording, so
// changing an error string cannot silently turn a terminal rejection into an
// infinite retry or the reverse.
func TestPermanentRejectionIsRecognisedByItsMarkerOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		want    bool
	}{
		{"marked", permanentRejectionPrefix + "failed to unmarshal configuration: bad", true},
		{"marked nil payload", permanentRejectionPrefix + "no configuration data provided", true},
		{"unmarked unmarshal from an older peer", "failed to unmarshal configuration: bad", false},
		{"save failure", "failed to save synchronized configuration: no space left on device", false},
		{"empty", "", false},
		{"marker in the middle", "something " + permanentRejectionPrefix + "bad", false},
	} {
		if got := isPermanentRejection(tc.message); got != tc.want {
			t.Errorf("%s: isPermanentRejection(%q) = %v, want %v",
				tc.name, tc.message, got, tc.want)
		}
	}
}

// Guard on the sender's timing assumption: the retries the transient path relies on
// must actually happen inside one pass rather than being deferred to the
// broadcaster's own schedule, or "stays in the retry set" would still mean the peer
// is a full backoff behind.
func TestTransientRejectionRetriesWithinTheSamePass(t *testing.T) {
	peer, addr := startDecliningPeer(t,
		"failed to save synchronized configuration: read-only file system")

	s := newPropagationTestServer(t, addr)

	s.Lock()
	s.markConfigDirty()
	s.Unlock()
	drainBroadcastTrigger(s)

	start := time.Now()
	s.broadcastConfigToPeersOnce()
	elapsed := time.Since(start)

	if got := peer.callCount(); got < 2 {
		t.Fatalf("the peer was called %d time(s), so no retry happened in the pass", got)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("the pass took %s, which is shorter than the first backoff; the "+
			"retries cannot have been real", elapsed)
	}
}
