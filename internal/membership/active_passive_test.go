package membership

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// stubServer is a ServerReference that records the orchestration calls the
// health checker makes, and applies demotions to the member list the way the
// real MakePassive RPC does.
type stubServer struct {
	members *MemberList

	leaderID         string
	epoch            int64
	demoted          []string
	broadcastLeader  string
	broadcastEpoch   int64
	broadcastStates  map[string]MemberStatus
	makePassiveFails bool
	configReconciles int

	// makePassiveDelay stalls each demotion, standing in for the slow peer that
	// made consolidation take tens of seconds on the health-check tick. Set before
	// the pass starts and not written afterwards.
	makePassiveDelay time.Duration

	// announced records, in order, the nodes asked to re-announce their floating
	// IPs, and announceFails makes that request fail. Ordering against demoted
	// matters: an announcement that lands before the demotions is the last word on
	// nothing, because the demoted node's own bring-up already announced later.
	announced     []string
	announceFails bool

	// failovers records the node each promotion was orchestrated onto, in order, so a
	// selection that depends on Go's randomised map iteration can be caught.
	failovers []string

	// sequence interleaves demotions and announcements in call order, because
	// consolidation's correctness depends on which came last: an announcement made
	// before the demotions is overwritten on the segment by the demoted node's own
	// earlier bring-up, which is the fault being fixed rather than the fix.
	sequence []string
}

func (s *stubServer) GetQuorumManager() *quorum.QuorumManager { return nil }

func (s *stubServer) OrchestrateIPFailover(oldNodeID, newNodeID string, ips []string) error {
	s.failovers = append(s.failovers, newNodeID)
	return nil
}

func (s *stubServer) GetClusterEpoch() int64 { return s.epoch }

func (s *stubServer) BroadcastClusterState(memberStates map[string]MemberStatus, epoch int64,
	leaderID string, leases map[string]string) error {
	s.broadcastLeader = leaderID
	s.broadcastEpoch = epoch
	s.broadcastStates = make(map[string]MemberStatus, len(memberStates))
	for id, st := range memberStates {
		s.broadcastStates[id] = st
	}
	return nil
}

func (s *stubServer) GetLeaderID() string             { return s.leaderID }
func (s *stubServer) GetLeaderLeaseUntil() time.Time  { return time.Time{} }
func (s *stubServer) RefreshLocalMonitorExpectedIPs() {}

func (s *stubServer) RequestConfigReconcile() { s.configReconciles++ }

func (s *stubServer) BroadcastVoteRequest(sessionID string, voteType, subject, description string,
	timeoutSeconds int64) error {
	return nil
}

func (s *stubServer) Promote(ctx context.Context, req *rpc.PromoteRequest) (*rpc.PromoteResponse, error) {
	return &rpc.PromoteResponse{Success: true}, nil
}

func (s *stubServer) MakePassive(ctx context.Context, req *rpc.MakePassiveRequest) (*rpc.MakePassiveResponse, error) {
	if s.makePassiveDelay > 0 {
		select {
		case <-time.After(s.makePassiveDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.makePassiveFails {
		return &rpc.MakePassiveResponse{Success: false, Message: "node unreachable"}, nil
	}
	s.demoted = append(s.demoted, req.NodeId)
	s.sequence = append(s.sequence, "demote:"+req.NodeId)
	if member := s.members.GetMemberByID(req.NodeId); member != nil {
		member.Lock()
		member.Status = StatusPassive
		member.ActiveIPs = nil
		member.LoadFactor = 0
		member.Unlock()
	}
	return &rpc.MakePassiveResponse{Success: true}, nil
}

func (s *stubServer) AnnounceNodeIPs(nodeID string) error {
	if s.announceFails {
		return errors.New("announce failed")
	}
	s.announced = append(s.announced, nodeID)
	s.sequence = append(s.sequence, "announce:"+nodeID)
	return nil
}

// newAPTestChecker wires a health checker past its startup grace period with a
// stub server, so consolidation decisions are exercised directly.
func newAPTestChecker(localNodeID string, members ...*Member) (*HealthChecker, *stubServer) {
	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-passive", LocalNode: localNodeID},
		Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}},
		Nodes:  map[string]*config.Node{},
	}
	for _, m := range members {
		cfg.Nodes[m.ID] = &config.Node{Hostname: m.Hostname}
	}

	ml := newAATestMemberList(cfg, members...)
	h := NewHealthChecker(ml, log.New(io.Discard))
	h.reconcileCycles = reconcileGraceCycles

	stub := &stubServer{members: ml}
	h.server = stub
	return h, stub
}

// Regression: an active-passive cluster must never settle on two Active nodes.
// SetMode demoting at switch time can't hold the invariant on its own — a late
// BringUpIP re-promotes a node afterwards — so the health checker enforces it
// every cycle.
func TestEnforceSingleActive(t *testing.T) {
	t.Run("demotes every Active but the consolidation target", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24", "10.0.0.3/24"})
		c := newAATestMember("node-c", "host-c", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", a, b, c)

		if !h.enforceSingleActive(h.members.Members) {
			t.Fatal("expected consolidation to report a demotion")
		}

		// node-b is the most loaded Active, so it keeps the IPs and node-a goes.
		if len(stub.demoted) != 1 || stub.demoted[0] != "node-a" {
			t.Errorf("expected only node-a demoted, got %v", stub.demoted)
		}
		if a.Status != StatusPassive {
			t.Errorf("expected node-a Passive, got %s", StatusToString(a.Status))
		}
		if b.Status != StatusActive {
			t.Errorf("expected node-b to stay Active, got %s", StatusToString(b.Status))
		}
		if stub.broadcastLeader != "node-b" {
			t.Errorf("expected the surviving Active broadcast as leader, got %q", stub.broadcastLeader)
		}
		if stub.broadcastStates["node-a"] != StatusPassive {
			t.Errorf("expected the broadcast to carry node-a as Passive, got %v", stub.broadcastStates)
		}
	})

	t.Run("leaves a single Active alone", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)
		h, stub := newAPTestChecker("node-a", a, b)

		if h.enforceSingleActive(h.members.Members) {
			t.Error("expected no demotion with a single Active node")
		}
		if len(stub.demoted) != 0 {
			t.Errorf("expected no demotions, got %v", stub.demoted)
		}
	})

	t.Run("only the coordinator consolidates", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})
		// node-a is the lowest-ID healthy node, so node-b must not act.
		h, stub := newAPTestChecker("node-b", a, b)

		if h.enforceSingleActive(h.members.Members) {
			t.Error("expected a non-coordinator to leave consolidation alone")
		}
		if len(stub.demoted) != 0 {
			t.Errorf("expected no demotions from a non-coordinator, got %v", stub.demoted)
		}
	})

	t.Run("waits out the startup grace period", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})
		h, stub := newAPTestChecker("node-a", a, b)
		h.reconcileCycles = reconcileGraceCycles - 1

		if h.enforceSingleActive(h.members.Members) {
			t.Error("expected no consolidation before the grace period elapses")
		}
		if len(stub.demoted) != 0 {
			t.Errorf("expected no demotions during grace period, got %v", stub.demoted)
		}
	})

	t.Run("a rejected demotion is not reported as consolidated", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24", "10.0.0.3/24"})
		h, stub := newAPTestChecker("node-a", a, b)
		stub.makePassiveFails = true

		if h.enforceSingleActive(h.members.Members) {
			t.Error("expected a failed demotion to report no change so the next cycle retries")
		}
		if a.Status != StatusActive {
			t.Errorf("expected node-a to stay Active after a rejected demotion, got %s", StatusToString(a.Status))
		}
		if stub.broadcastLeader != "" {
			t.Error("expected no state broadcast when nothing was demoted")
		}
	})
}
