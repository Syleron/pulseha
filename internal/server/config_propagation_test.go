package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newPropagationTestServer builds a Server that can run a real config broadcast:
// a config on disk under t.TempDir() so Save() succeeds, an initialised peer
// connection pool, and one extra node per peerAddr for the broadcast to push to.
//
// The local node binds 127.0.0.1 on an ephemeral port, so Reconfigure can be
// called against it without fighting the host for a fixed port.
func newPropagationTestServer(t *testing.T, peerAddrs ...string) *Server {
	t.Helper()

	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	const localID = "local-node"
	nodes := map[string]*config.Node{
		localID: {Hostname: localID, IP: "127.0.0.1", Port: "0"},
	}
	for i, addr := range peerAddrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("SplitHostPort(%s): %v", addr, err)
		}
		nodes[fmt.Sprintf("peer-%d", i)] = &config.Node{
			Hostname: "peer-" + addr, IP: host, Port: port,
		}
	}

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-active",
			LocalNode:           localID,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": {"10.0.0.1/24"}},
		Nodes:  nodes,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id := range nodes {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		ml.GetMemberByID(id).Status = membership.StatusActive
	}

	return &Server{
		config:           cfg,
		logger:           logger,
		memberList:       ml,
		peerClients:      make(map[string]*client.Client),
		broadcastTrigger: make(chan struct{}, 1),
		broadcastStop:    make(chan struct{}),
	}
}

// refusingPeer is a real gRPC peer whose ConfigSync is refused for a while and
// then accepted, standing in for defect #31: a peer whose listener is cycling
// under a config-sync storm refuses everything for seconds at a time, against a
// daemon that never restarted. Refusing rather than dropping the connection keeps
// the test deterministic; the sender cannot tell the difference — either way the
// peer stays outstanding.
type refusingPeer struct {
	rpc.UnimplementedServerServer

	refuseUntil time.Time

	mu       sync.Mutex
	refused  int
	accepted int
}

func (p *refusingPeer) ConfigSync(_ context.Context, _ *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Now().Before(p.refuseUntil) {
		p.refused++
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	p.accepted++
	return &rpc.ConfigSyncResponse{Success: true, Message: "applied"}, nil
}

func (p *refusingPeer) counts() (refused, accepted int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused, p.accepted
}

// startRefusingPeer serves a refusingPeer on an ephemeral loopback port.
func startRefusingPeer(t *testing.T, refuseFor time.Duration) (*refusingPeer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	peer := &refusingPeer{refuseUntil: time.Now().Add(refuseFor)}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// Regression for docs/TEST-PLAN.md defect #43: a burst of config mutations commits
// locally and never propagates.
//
// A broadcast gets four attempts inside ~1.75s of backoff. A peer that refuses for
// longer than that used to end the story: the broadcaster logged a warning
// deferring to "the periodic reconcile", which runs only on the coordinator, so a
// mutation taken anywhere else stayed on one node until the next successful
// mutation happened to carry the backlog. On whitecrane that left node-1 at 286
// against 267/268/268 with all four nodes healthy, stable for 135s+, after 20
// add-ip calls that every one of them reported rc=0.
//
// The node that owns the change has to be the node that retries it.
func TestUnpropagatedConfigIsRetriedUntilEveryPeerAccepts(t *testing.T) {
	// Comfortably past the in-pass attempts, so nothing inside a single broadcast
	// can reach this peer and only a retry outside one can.
	peer, addr := startRefusingPeer(t, 3*time.Second)

	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	// A mutation, shaped as the real ones are: config change and markConfigDirty
	// under s.Lock().
	s.Lock()
	s.config.Groups["group1"] = append(s.config.Groups["group1"], "10.0.0.50/24")
	s.markConfigDirty()
	s.Unlock()

	// Generous: the first retry lands a few seconds after the pass gives up.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, accepted := peer.counts(); accepted > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	refused, accepted := peer.counts()
	if accepted == 0 {
		t.Fatalf("the peer never received the config: %d refused pushes, 0 accepted. "+
			"A broadcast that outlives its in-pass attempts must be retried by the "+
			"node holding the change, not left to a reconcile only the coordinator "+
			"runs (defect #43)", refused)
	}
	if refused == 0 {
		t.Fatalf("the peer accepted %d pushes without refusing any; the test did not "+
			"exercise a failed broadcast at all", accepted)
	}
}

// The retry schedule itself: it has to back off, it has to have a ceiling, and it
// has to stop. A repair that re-pushes forever at a fixed interval is its own
// config-sync storm, which is how #31 amplifies in the first place.
func TestPropagationRetryBacksOffAndStops(t *testing.T) {
	s := &Server{logger: log.New(io.Discard)}
	stamp := configStamp{version: 7, origin: "local-node"}

	first := s.recordUnpropagated(stamp, []string{"peer-0"})
	if first != propagationRetryBase {
		t.Errorf("first retry in %s, want %s", first, propagationRetryBase)
	}
	if second := s.recordUnpropagated(stamp, []string{"peer-0"}); second != 2*propagationRetryBase {
		t.Errorf("second retry in %s, want %s (the interval has to widen while a "+
			"peer stays unreachable)", second, 2*propagationRetryBase)
	}

	// A peer down for a long time must settle at the ceiling rather than growing
	// without bound and effectively giving up.
	for i := 0; i < 20; i++ {
		s.recordUnpropagated(stamp, []string{"peer-0"})
	}
	outstanding, ok := s.pendingPropagation()
	if !ok {
		t.Fatal("no retry outstanding after 22 failed broadcasts")
	}
	if outstanding.backoff != propagationRetryMax {
		t.Errorf("backoff = %s, want it capped at %s", outstanding.backoff, propagationRetryMax)
	}
	if outstanding.attempts != 22 {
		t.Errorf("attempts = %d, want 22", outstanding.attempts)
	}

	attempts, wasOutstanding := s.clearUnpropagated()
	if !wasOutstanding || attempts != 22 {
		t.Errorf("clearUnpropagated() = (%d, %v), want (22, true)", attempts, wasOutstanding)
	}
	if _, ok := s.pendingPropagation(); ok {
		t.Error("a retry is still outstanding after every peer accepted the config; " +
			"the broadcaster would re-push forever")
	}
}

// Regression for defect #31, the amplifier under #43 and #37: a ConfigSync cycles
// the receiver's gRPC listener.
//
// Every full ConfigSync spawns Reconfigure, which used to GracefulStop the cluster
// listener and bind a fresh one unconditionally — even though the overwhelming
// majority of config changes do not touch the bind address. Peers then saw
// `connection refused` from a daemon that never restarted: in run 23 that refused
// 56 of 60 peer bring-up RPCs during a 20-add burst, and starved the config
// broadcast's own retries.
func TestReconfigureKeepsTheListenerWhenTheBindAddressIsUnchanged(t *testing.T) {
	s := newPropagationTestServer(t)

	localNode, err := s.config.GetLocalNode()
	if err != nil {
		t.Fatalf("GetLocalNode: %v", err)
	}
	if err := s.startClusterListener(localNode); err != nil {
		t.Fatalf("startClusterListener: %v", err)
	}

	s.Lock()
	before := s.grpcServer
	s.Unlock()
	if before == nil {
		t.Fatal("no cluster gRPC server after startClusterListener")
	}

	if err := s.Reconfigure(); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	s.Lock()
	after := s.grpcServer
	s.Unlock()
	if after != before {
		t.Error("Reconfigure stopped the cluster listener and bound a new one although " +
			"the bind address did not change; every config broadcast then refuses " +
			"inbound RPCs on the receiver for the length of the rebind (defect #31)")
	}
}

// The skip must be narrow: a config change that genuinely moves the bind address
// still has to rebind, or the node serves the wrong endpoint until it restarts.
func TestReconfigureRebindsWhenTheBindAddressChanges(t *testing.T) {
	s := newPropagationTestServer(t)

	localNode, err := s.config.GetLocalNode()
	if err != nil {
		t.Fatalf("GetLocalNode: %v", err)
	}
	if err := s.startClusterListener(localNode); err != nil {
		t.Fatalf("startClusterListener: %v", err)
	}

	s.Lock()
	before := s.grpcServer
	localID, _ := s.config.GetLocalNodeUUID()
	s.config.Nodes[localID].Port = freeLoopbackPort(t)
	newAddr := net.JoinHostPort("127.0.0.1", s.config.Nodes[localID].Port)
	s.Unlock()

	if err := s.Reconfigure(); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	s.Lock()
	after := s.grpcServer
	s.Unlock()
	if after == before {
		t.Error("Reconfigure kept the old cluster listener although the bind address moved")
	}

	conn, err := net.DialTimeout("tcp", newAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s after Reconfigure: %v; the listener did not move to the "+
			"new bind address", newAddr, err)
	}
	conn.Close()
}

// freeLoopbackPort returns a loopback port that was free a moment ago. Binding
// uses SO_REUSEADDR, so the brief gap between the probe closing and the real bind
// is not a problem in practice.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return port
}
