package server

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// incumbentPeer answers the health check with a chosen role and records every
// MakePassive it is sent.
type incumbentPeer struct {
	rpc.UnimplementedServerServer

	id    string
	token string
	role  rpc.MemberStatusEnum

	mu          sync.Mutex
	makePassive int
}

func (p *incumbentPeer) HealthCheck(_ context.Context, _ *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	return &rpc.HealthCheckResponse{Success: true, ClusterToken: p.token, NodeId: p.id, Status: p.role}, nil
}

func (p *incumbentPeer) MakePassive(_ context.Context, _ *rpc.MakePassiveRequest) (*rpc.MakePassiveResponse, error) {
	p.mu.Lock()
	p.makePassive++
	p.mu.Unlock()
	return &rpc.MakePassiveResponse{Success: true, Message: "released"}, nil
}

func (p *incumbentPeer) demotions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.makePassive
}

const (
	selfPromoteLocal = "node-b"
	selfPromotePeer  = "node-a"
	selfPromoteToken = "token-276"
)

// newSelfPromotionServer is node-4 one second after its restart in #276: a
// two-node active-passive cluster, the local node Passive, and the peer — which
// is in fact Active and answering — still Unknown because its role has not been
// learned yet.
func newSelfPromotionServer(t *testing.T, role rpc.MemberStatusEnum) (*Server, *incumbentPeer) {
	t.Helper()
	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	peer := &incumbentPeer{id: selfPromotePeer, token: selfPromoteToken, role: role}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-passive",
			LocalNode:           selfPromoteLocal,
			ClusterToken:        selfPromoteToken,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{},
		Nodes: map[string]*config.Node{
			selfPromoteLocal: {Hostname: "node-4", IP: "127.0.0.1", Port: "1"},
			selfPromotePeer:  {Hostname: "node-5", IP: host, Port: port},
		},
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id, st := range map[string]membership.MemberStatus{
		selfPromoteLocal: membership.StatusPassive,
		selfPromotePeer:  membership.StatusUnknown,
	} {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		ml.GetMemberByID(id).SetStatus(st)
	}
	return &Server{config: cfg, logger: logger, memberList: ml}, peer
}

// Regression for GitHub #276, link 2. Step 1b of performPromotionAsync treated an
// Unknown peer as unreachable and told it to go Passive "to confirm it released".
// On a restart that peer was the healthy Active: it answered and released all
// 118 addresses to a node one second into its life. A peer that answers and says
// it is Active is the Active; the election was wrong, not the incumbent.
func TestStep1bDoesNotDemoteAnIncumbentThatReportsActive(t *testing.T) {
	s, peer := newSelfPromotionServer(t, rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE)

	released, _, err := s.confirmPeerReleasedIPs(context.Background(), selfPromotePeer)

	if released {
		t.Error("released = true for a peer that reports itself Active")
	}
	if !errors.Is(err, errIncumbentIsActive) {
		t.Errorf("err = %v, want errIncumbentIsActive", err)
	}
	if n := peer.demotions(); n != 0 {
		t.Errorf("sent %d MakePassive to a reachable Active, want 0", n)
	}
}

// The path Step 1b exists for still works: an incumbent that answers but is not
// Active is asked to release, and its confirmation counts.
func TestStep1bStillConfirmsTheReleaseOfAnIncumbentThatIsNotActive(t *testing.T) {
	for _, role := range []rpc.MemberStatusEnum{
		rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE,
		rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN,
	} {
		s, peer := newSelfPromotionServer(t, role)

		released, _, err := s.confirmPeerReleasedIPs(context.Background(), selfPromotePeer)

		if !released || err != nil {
			t.Errorf("role %v: released=%v err=%v, want a confirmed release", role, released, err)
		}
		if n := peer.demotions(); n != 1 {
			t.Errorf("role %v: sent %d MakePassive, want 1", role, n)
		}
	}
}

// The whole of link 2, end to end: the election-driven self-promotion aborts,
// the local node stays Passive, and the peer is recorded as the Active it said
// it was — so the next health check finds an Active and does not elect again.
func TestASelfPromotionOverAReachableActiveAborts(t *testing.T) {
	s, peer := newSelfPromotionServer(t, rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE)

	s.performPromotionAsync(selfPromoteLocal, nil, false)

	if got := s.memberList.GetMemberByID(selfPromoteLocal).GetStatus(); got == membership.StatusActive {
		t.Error("the local node promoted itself over a reachable Active")
	}
	if got := s.memberList.GetMemberByID(selfPromotePeer).GetStatus(); got != membership.StatusActive {
		t.Errorf("peer status after the abort = %v, want Active: it told us so", got)
	}
	if n := peer.demotions(); n != 0 {
		t.Errorf("sent %d MakePassive to the reachable Active, want 0", n)
	}
}

// Regression for GitHub #276, link 4. A promotion whose node was demoted while
// its address transfer ran still broadcast itself Active when the transfer
// finished — 11s after the demotion, at a fresh epoch — and re-asserted a
// promotion the cluster had already overruled.
func TestAPromotionOverruledDuringItsTransferDoesNotBroadcastItselfActive(t *testing.T) {
	s, _ := newSelfPromotionServer(t, rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE)
	local := s.memberList.GetMemberByID(selfPromoteLocal)
	s.afterPromotionTransfer = func() {
		// The ConfigSync demotion landing while the transfer was still running.
		local.SetStatus(membership.StatusPassive)
	}
	before := s.GetClusterEpoch()

	s.performPromotionAsync(selfPromoteLocal, nil, false)

	if after := s.GetClusterEpoch(); after != before {
		t.Errorf("cluster epoch %d -> %d: the overruled promotion broadcast its final state", before, after)
	}
	if got := local.GetStatus(); got == membership.StatusActive {
		t.Error("local node is Active again after its promotion was overruled")
	}
}

// Its negative control: a promotion nobody overruled still broadcasts.
func TestAPromotionThatStillStandsBroadcastsItsFinalState(t *testing.T) {
	s, _ := newSelfPromotionServer(t, rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE)
	before := s.GetClusterEpoch()

	s.performPromotionAsync(selfPromoteLocal, nil, false)

	if after := s.GetClusterEpoch(); after <= before {
		t.Errorf("cluster epoch %d -> %d: a standing promotion must broadcast its final state", before, after)
	}
	if got := s.memberList.GetMemberByID(selfPromoteLocal).GetStatus(); got != membership.StatusActive {
		t.Errorf("local status = %v, want Active", got)
	}
}
