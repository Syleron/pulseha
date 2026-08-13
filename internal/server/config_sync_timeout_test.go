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

// The flat 2s this replaced (defect #62). Named here rather than inlined so the
// assertions below read as the comparison they are.
const flatConfigSyncTimeout = 2 * time.Second

// An empty payload gets the full base, because the payload is not what overran
// the flat deadline. The receiver's payload-proportional work — three parses of
// the message, the group deep-copies, the marshal and the file write — is well
// under a millisecond per KiB. What overran 2s is the write lock every mutation
// holds, the member-list lock the health-check cycle holds through its IP work,
// and a lazily-dialled peer client paying for its handshake inside the RPC.
// A fix that only scaled the payload term would leave the small-config case
// exactly as broken as it was.
func TestConfigSyncTimeoutCoversAReceiverThatIsMerelyBusy(t *testing.T) {
	if got := configSyncTimeoutFor(0); got <= flatConfigSyncTimeout {
		t.Errorf("an empty payload got %s, which is no better than the flat %s it replaced",
			got, flatConfigSyncTimeout)
	}
	// The payload run 32 timed out on: a 248-address group, roughly 9KiB.
	if got := configSyncTimeoutFor(9 * 1024); got <= flatConfigSyncTimeout {
		t.Errorf("run 32's payload got %s, which is no better than the flat %s it replaced",
			got, flatConfigSyncTimeout)
	}
}

func TestConfigSyncTimeoutGrowsWithThePayload(t *testing.T) {
	small := configSyncTimeoutFor(1 * 1024)
	large := configSyncTimeoutFor(16 * 1024)

	if large <= small {
		t.Errorf("16KiB got %s, 1KiB got %s; the deadline must grow with the payload",
			large, small)
	}
}

func TestConfigSyncTimeoutIsBounded(t *testing.T) {
	// A floor, so an empty or miscounted payload still gets a usable deadline
	// rather than one already expired.
	if got := configSyncTimeoutFor(0); got < configSyncBaseTimeout {
		t.Errorf("an empty payload got %s, want at least the %s base", got, configSyncBaseTimeout)
	}
	if got := configSyncTimeoutFor(-5); got < configSyncBaseTimeout {
		t.Errorf("a negative size got %s, want at least the %s base", got, configSyncBaseTimeout)
	}
	// And a ceiling. Past it, waiting longer stops being the right instrument:
	// a peer that cannot answer inside this is unavailable rather than busy, and
	// #43's retry is what covers that. Blocking the broadcaster on it only holds
	// the next config behind a peer that will not answer either way.
	if got := configSyncTimeoutFor(10 << 20); got != configSyncMaxTimeout {
		t.Errorf("a 10MiB payload got %s, want the %s cap", got, configSyncMaxTimeout)
	}
}

// slowPeer is a real gRPC peer that takes its time in ConfigSync and then
// succeeds — a receiver that is loaded, not one that is down. It records the
// pushes the sender abandoned mid-handler, which is the sender's own
// DeadlineExceeded seen from the other end.
type slowPeer struct {
	rpc.UnimplementedServerServer

	delay time.Duration

	mu        sync.Mutex
	accepted  int
	abandoned int
}

func (p *slowPeer) ConfigSync(ctx context.Context, _ *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		p.mu.Lock()
		p.abandoned++
		p.mu.Unlock()
		return nil, ctx.Err()
	}

	p.mu.Lock()
	p.accepted++
	p.mu.Unlock()
	return &rpc.ConfigSyncResponse{Success: true, Message: "applied"}, nil
}

func (p *slowPeer) counts() (accepted, abandoned int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted, p.abandoned
}

func startSlowPeer(t *testing.T, delay time.Duration) (*slowPeer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	peer := &slowPeer{delay: delay}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// Regression for docs/TEST-PLAN.md defect #62: a config push abandoned on a peer
// that was going to answer.
//
// This peer is slower than the flat 2s and faster than the sized deadline, so it
// separates the two directly. Against the flat deadline every attempt and every
// retry gave up before the handler returned — on whitecrane that left a peer
// without the group key at all for ~3 minutes, after 248 add-ip calls that had
// each already reported success to the operator — and no amount of retrying
// helps, because the retry re-sends the same payload under the same deadline to
// the same busy receiver.
func TestABusyPeerIsWaitedOutRatherThanAbandoned(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a real RPC open past the deadline it is testing")
	}

	// Comfortably past the 2s, comfortably inside the base.
	peer, addr := startSlowPeer(t, 3*time.Second)

	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	// A mutation, shaped as the real ones are: config change and markConfigDirty
	// under s.Lock().
	s.Lock()
	s.config.Groups["group1"] = append(s.config.Groups["group1"], "10.0.0.50/24")
	s.markConfigDirty()
	s.Unlock()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if accepted, _ := peer.counts(); accepted > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	accepted, abandoned := peer.counts()
	if accepted == 0 {
		t.Fatalf("the peer never got the config: %d pushes abandoned mid-handler, 0 accepted. "+
			"A receiver that is busy rather than down has to be waited out; the retry "+
			"cannot rescue this, since it re-sends under the same deadline (defect #62)",
			abandoned)
	}
	if abandoned > 0 {
		t.Errorf("%d pushes were abandoned before the peer answered, and %d accepted; "+
			"the deadline should cover this receiver on the first attempt", abandoned, accepted)
	}
}
