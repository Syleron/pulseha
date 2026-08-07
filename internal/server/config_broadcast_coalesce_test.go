package server

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// countingPeer records every config it is pushed, so a test can compare how many
// pushes a burst of mutations cost against what the burst finally said.
type countingPeer struct {
	rpc.UnimplementedServerServer

	mu     sync.Mutex
	pushes int
	last   []byte
}

func (p *countingPeer) ConfigSync(_ context.Context, req *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	p.mu.Lock()
	p.pushes++
	p.last = req.Config
	p.mu.Unlock()
	return &rpc.ConfigSyncResponse{Success: true, Message: "applied"}, nil
}

// groupAddresses returns the addresses the last pushed config carried for a
// group, which is what the peer would have applied.
func (p *countingPeer) groupAddresses(t *testing.T, group string) (pushes int, ips []string) {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.last == nil {
		return p.pushes, nil
	}
	var payload struct {
		Groups map[string][]string `json:"floating_ip_groups"`
	}
	if err := json.Unmarshal(p.last, &payload); err != nil {
		t.Fatalf("unmarshal last pushed config: %v", err)
	}
	return p.pushes, payload.Groups[group]
}

func startCountingPeer(t *testing.T) (*countingPeer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	peer := &countingPeer{}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// addAddress is one `group add-ip`, shaped as the real mutations are: the config
// change and markConfigDirty together under s.Lock().
func addAddress(s *Server, group, ip string) {
	s.Lock()
	defer s.Unlock()

	s.config.Groups[group] = append(s.config.Groups[group], ip)
	s.markConfigDirty()
}

// The second half of docs/TEST-PLAN.md defect #62: a burst of mutations is a
// self-inflicted storm at the receiver.
//
// The trigger channel coalesces mutations that land while a broadcast is in
// flight, which covers concurrent ones. Serial ones fell straight through it —
// since #37 an add-ip completes in ~38ms and a healthy broadcast finishes well
// inside that — so whitecrane's 248 back-to-back adds cost ~248 broadcasts and
// 744 ConfigSync RPCs, each a parse, a file write, a member-list reload and a
// `go Reconfigure()` on a node that was already the bottleneck.
//
// Timed against the window rather than a fixed number, so the assertion holds
// whatever the window is set to.
func TestASerialBurstOfMutationsCostsOnePushPerWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a burst against a real peer for several linger windows")
	}

	peer, addr := startCountingPeer(t)

	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	// A burst roughly two windows long, at a rate a broadcast per mutation
	// would comfortably keep up with.
	const (
		mutations = 40
		spacing   = 10 * time.Millisecond
	)
	start := time.Now()
	for i := 0; i < mutations; i++ {
		addAddress(s, "group1", "10.0.0."+string(rune('0'+i%10))+"/24")
		time.Sleep(spacing)
	}
	last := "10.0.0.254/24"
	addAddress(s, "group1", last)

	// Let the final window close and its push land.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ips := peer.groupAddresses(t, "group1"); len(ips) > 0 && ips[len(ips)-1] == last {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	elapsed := time.Since(start)

	pushes, ips := peer.groupAddresses(t, "group1")

	// One push per window, plus slack for the window in progress when the burst
	// started and the one that carried the final mutation.
	allowed := int(elapsed/configBroadcastLinger) + 2
	if pushes > allowed {
		t.Errorf("%d mutations over %s cost %d pushes; at one per %s window that should be at most %d. "+
			"A serial burst has to coalesce, not push every version (defect #62)",
			mutations+1, elapsed.Round(time.Millisecond), pushes, configBroadcastLinger, allowed)
	}

	// Coalescing that drops the last word is not coalescing.
	if len(ips) == 0 || ips[len(ips)-1] != last {
		t.Errorf("the peer's last config ends %v, want it to carry %s; a coalesced burst must "+
			"still deliver the final state", ips, last)
	}
}

// The window is a delay, not a gate: a single mutation on an idle cluster still
// has to reach its peers, and within the window rather than whenever something
// else happens to change.
func TestASingleMutationStillPropagatesPromptly(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on a real push")
	}

	peer, addr := startCountingPeer(t)

	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	start := time.Now()
	addAddress(s, "group1", "10.0.0.50/24")

	// Generous against the window, tight against "never".
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pushes, _ := peer.groupAddresses(t, "group1"); pushes > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pushes, ips := peer.groupAddresses(t, "group1")
	if pushes == 0 {
		t.Fatal("a single mutation was never pushed; the linger must delay a broadcast, not replace it")
	}
	if took := time.Since(start); took > 20*configBroadcastLinger {
		t.Errorf("a single mutation took %s to reach the peer, which is far past the %s window",
			took.Round(time.Millisecond), configBroadcastLinger)
	}
	if len(ips) == 0 || ips[len(ips)-1] != "10.0.0.50/24" {
		t.Errorf("the peer got %v, want the mutation to be in it", ips)
	}
}
