package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// convergence is clusterEpoch and leaderID as one immutable value, so the
// Witness Report can read the pair without the server lock and without
// straddling an adopt that moves one and not yet the other.
type convergence struct {
	epoch  int64
	leader string
}

// publishConvergenceLocked republishes clusterEpoch and leaderID. Called, with
// the server lock held, after every write to either.
func (s *Server) publishConvergenceLocked() {
	s.published.Store(&convergence{epoch: s.clusterEpoch, leader: s.leaderID})
}

// reportedConvergence is the epoch and Elected Node this node would report,
// read without the server lock. The vote floor is folded in exactly as
// GetClusterEpoch folds it.
func (s *Server) reportedConvergence() (int64, string) {
	var c convergence
	if p := s.published.Load(); p != nil {
		c = *p
	}
	return max(c.epoch, s.voteEpochFloor.Load()), c.leader
}

const (
	// witnessAskTimeout bounds a node's question to the Witness. The Witness
	// spends at most half of what is left of it on each peer, so a peer that
	// does not answer comes back as not visible, inside the asker's deadline,
	// rather than as a Witness that did not answer at all -- which is a
	// different tier (ADR-0006).
	witnessAskTimeout = 2 * time.Second

	// witnessPeerTimeoutDefault is the Witness's budget for a peer when the
	// asker sent no deadline at all.
	witnessPeerTimeoutDefault = witnessAskTimeout / 2
)

// witnessPeerBudget is how long the Witness may wait on one peer, given the
// asker's context: strictly inside what remains of the asker's deadline.
func witnessPeerBudget(ctx context.Context, now time.Time) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return witnessPeerTimeoutDefault
	}
	return min(deadline.Sub(now)/2, witnessPeerTimeoutDefault)
}

// ownClaim is this node's Claim as it would report it. It takes the local
// member's lock and nothing else: the member list's read lock to find the
// member, and the published convergence pair for the epoch.
func (s *Server) ownClaim(localID string) *rpc.MemberClaim {
	epoch, elected := s.reportedConvergence()
	out := &rpc.MemberClaim{NodeId: localID, ClusterEpoch: epoch, ElectedNode: elected}
	if local := s.memberList.GetMemberByID(localID); local != nil {
		c := local.Claim()
		out.Status = rpc.MemberStatusEnum(c.Status)
		out.ActiveIps = c.ActiveIPs
	}
	return out
}

// WitnessReport answers what this node can see.
//
// Every member answers with its own Claim; that is what the Witness's round-trip
// to a peer receives, and why the proof that a peer is alive is the peer
// reporting itself. The cluster's Witness additionally asks every configured
// member other than itself and the asker, there and then, and reports each as
// visible with its Claim or as not visible.
//
// Deliberately takes no server lock, because the node answering may be in the
// middle of its own election: an answer that waited behind that would turn a
// live node into a "not visible" one (ADR-0003/0004, risk #5 on END-2631).
func (s *Server) WitnessReport(ctx context.Context, req *rpc.WitnessReportRequest) (*rpc.WitnessReportResponse, error) {
	cfg := s.clusterSnapshot()()
	if cfg == nil {
		return &rpc.WitnessReportResponse{Success: false, Message: "no cluster configured"}, nil
	}
	cfg.Lock()
	localID := cfg.Pulse.LocalNode
	token := cfg.Pulse.ClusterToken
	askerKnown := cfg.Nodes[req.GetNodeId()] != nil
	localIsWitness := cfg.Nodes[localID].IsWitness()
	mode := cfg.Pulse.Mode
	peers := make(map[string]config.Node, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if id != localID && id != req.GetNodeId() && node != nil {
			peers[id] = *node
		}
	}
	cfg.Unlock()

	if req.GetClusterToken() == "" || !tokensEqual(req.GetClusterToken(), token) {
		return &rpc.WitnessReportResponse{Success: false, Message: "cluster token mismatch"}, nil
	}
	if !askerKnown {
		return &rpc.WitnessReportResponse{Success: false, Message: "asker is not a member of this cluster"}, nil
	}

	resp := &rpc.WitnessReportResponse{
		Success:                true,
		Self:                   s.ownClaim(localID),
		FromWitness:            localIsWitness,
		WitnessProtocolVersion: witnessProtocolVersion,
	}
	if !localIsWitness {
		return resp, nil
	}
	resp.Peers = s.observePeers(ctx, localID, token, peers)
	now := time.Now()
	current := s.rulingsFor(mode, req.GetClaim(), resp.Peers)
	s.witnessHistory.remember(now, current)
	resp.Rulings = append(current, s.witnessHistory.settledFor(now, s.witnessHoldDown(), req.GetNodeId(), current)...)
	return resp, nil
}

// observePeers asks each peer for its own Claim, concurrently, each within the
// budget the asker's deadline leaves. A peer is visible only if it answered,
// successfully and under its own ID.
//
// Nothing here falls back on what the health checker last saw. That memory
// would still describe a SIGSTOPped Active as serving, and a report built from
// it would keep the survivor from promoting while the address answers nobody.
func (s *Server) observePeers(ctx context.Context, localID, token string, peers map[string]config.Node) []*rpc.PeerView {
	budget := witnessPeerBudget(ctx, time.Now())
	ids := make([]string, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	views := make([]*rpc.PeerView, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string, node config.Node) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			claim, err := s.askPeerForClaim(pctx, localID, token, id, &node)
			if err != nil {
				views[i] = &rpc.PeerView{NodeId: id, Reason: err.Error()}
				return
			}
			views[i] = &rpc.PeerView{NodeId: id, Visible: true, Claim: claim}
		}(i, id, peers[id])
	}
	wg.Wait()
	return views
}

// askPeerForClaim is one round-trip: the peer's own Claim, or why there is none.
func (s *Server) askPeerForClaim(ctx context.Context, localID, token, id string, node *config.Node) (*rpc.MemberClaim, error) {
	peer, err := s.getPeerClient(id, node)
	if err != nil {
		return nil, err
	}
	resp, err := peer.Server().WitnessReport(ctx, &rpc.WitnessReportRequest{NodeId: localID, ClusterToken: token})
	switch {
	case err != nil:
		return nil, err
	case !resp.GetSuccess():
		return nil, fmt.Errorf("peer refused: %s", resp.GetMessage())
	case resp.GetSelf().GetNodeId() != id:
		// Whatever answered on that address is not the member this view names.
		return nil, fmt.Errorf("answered as %q", resp.GetSelf().GetNodeId())
	}
	return resp.GetSelf(), nil
}

// errNoWitness is askWitness's answer for a cluster that has none configured,
// which is not a failure: it is tier 3, today's behaviour.
var errNoWitness = errors.New("no Witness configured")

// askWitness asks the configured Witness what it can see, within
// witnessAskTimeout. An error means there is no usable report -- no Witness, or
// one that did not answer or did not answer as itself -- and the caller must
// treat it as the Witness being silent.
func (s *Server) askWitness(ctx context.Context) (*rpc.WitnessReportResponse, error) {
	cfg := s.clusterSnapshot()()
	if cfg == nil {
		return nil, errNoWitness
	}
	cfg.Lock()
	localID := cfg.Pulse.LocalNode
	token := cfg.Pulse.ClusterToken
	witnessID := ""
	var witness config.Node
	for id, node := range cfg.Nodes {
		if node.IsWitness() {
			witnessID, witness = id, *node
		}
	}
	cfg.Unlock()
	if witnessID == "" || witnessID == localID {
		return nil, errNoWitness
	}

	ctx, cancel := context.WithTimeout(ctx, witnessAskTimeout)
	defer cancel()
	peer, err := s.getPeerClient(witnessID, &witness)
	if err != nil {
		return nil, err
	}
	resp, err := peer.Server().WitnessReport(ctx, &rpc.WitnessReportRequest{
		NodeId:       localID,
		ClusterToken: token,
		Claim:        s.ownClaim(localID),
	})
	switch {
	case err != nil:
		return nil, err
	case !resp.GetSuccess():
		return nil, fmt.Errorf("witness refused: %s", resp.GetMessage())
	case !resp.GetFromWitness() || resp.GetSelf().GetNodeId() != witnessID:
		return nil, fmt.Errorf("the configured Witness %s did not answer as the Witness", witnessID)
	}
	return resp, nil
}
