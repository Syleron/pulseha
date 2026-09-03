package server

import (
	"context"
	"time"

	"github.com/syleron/pulseha/rpc"
)

// configRepairInterval floors how often a node will pull a repair. Envelope syncs
// arrive every few seconds, so an unrepairable mismatch -- a hash false positive
// nobody foresaw, or a coordinator that cannot serve its config -- would otherwise
// become a pull per envelope, per peer, forever. At most two attempts a minute
// keeps a wrong detector from becoming an outage of its own.
const configRepairInterval = 30 * time.Second

// peerHashFreshness bounds how long a peer's last reported hash counts toward the
// tally. A node that has gone away must stop voting: its final hash is a claim
// about a config that may since have changed several times, and letting it stand
// forever would let a dead node decide which of two live ones is authoritative.
// Excluding a stale peer shrinks the electorate, which makes a majority harder to
// reach and so errs toward doing nothing.
const peerHashFreshness = 60 * time.Second

// peerHash is one peer's last reported fingerprint and when it arrived.
type peerHash struct {
	hash string
	seen time.Time
}

// recordPeerConfigHash remembers what a peer last reported, which is what makes
// the tally in noteConfigDivergence possible: every envelope already carries a
// hash, so a node can see the whole cluster's view without asking anyone.
func (s *Server) recordPeerConfigHash(senderID, hash string) {
	if senderID == "" || hash == "" {
		return
	}
	s.peerHashMu.Lock()
	defer s.peerHashMu.Unlock()
	if s.peerHashes == nil {
		s.peerHashes = map[string]peerHash{}
	}
	s.peerHashes[senderID] = peerHash{hash: hash, seen: time.Now()}
}

// noteConfigDivergence is the detector: it decides whether an incoming hash means
// this node is diverged, and starts one repair if so.
//
// Four conditions, and each is load-bearing:
//
//   - The hashes must both be present. An absent hash means a peer that cannot
//     fingerprint its config or is running an older binary, not agreement, and
//     comparing against "" would call every such peer diverged.
//   - They must differ. Equal hashes are the whole point of the mechanism.
//   - The sender must be the coordinator *by this node's own reckoning*. The
//     coordinator is the one copy the cluster has agreed to treat as
//     authoritative, and clusterCoordinator is what agrees it; trusting whoever
//     spoke last would have two diverged nodes pull from each other forever.
//   - The coordinator's hash must hold a majority of the nodes this one can
//     currently account for, itself included.
//
// That last condition is the safety property, and it is not the pull direction --
// a mismatch is symmetric, so "disagree with the coordinator, therefore adopt the
// coordinator's copy" adopts a diverged coordinator's config exactly as a push
// would, turning one wrong node into a wrong cluster. What actually distinguishes
// the diverged node from the healthy one is the rest of the cluster, and the tally
// is how this node reads it. A node in the minority repairs itself; a node in the
// majority does nothing however loudly the coordinator disagrees, so a diverged
// coordinator is contradicted rather than obeyed.
//
// With no majority either way -- most importantly a two-node cluster, where there
// can never be one -- this degrades to detect-and-report and repairs nothing. That
// is the honest outcome: with one peer disagreeing with you and nothing else to
// consult, there is no evidence about which copy is right, and picking one anyway
// is how a cluster loses a config nobody meant to discard. See
// docs/adr/0002-two-node-availability.md for the same reasoning about quorum.
func (s *Server) noteConfigDivergence(senderID, incomingHash string) {
	if senderID == "" || incomingHash == "" {
		return
	}
	s.recordPeerConfigHash(senderID, incomingHash)

	localHash, err := sharedConfigHash(s.currentConfig())
	if err != nil || localHash == "" {
		return
	}
	if localHash == incomingHash {
		return
	}

	coordinator := s.clusterCoordinatorID()
	if coordinator == "" || coordinator != senderID {
		// Worth a line either way: a mismatch against a non-coordinator peer is
		// still divergence somewhere in the cluster, and knowing which pairs
		// disagree is most of diagnosing it. This is the observability half of
		// docs/TEST-PLAN.md #103 -- the lab's divergence was invisible for hours.
		s.logger.Warn("CONFIG_DIVERGENCE: config differs from a peer",
			"peer", senderID, "peerHash", shortHash(incomingHash),
			"localHash", shortHash(localHash), "coordinator", coordinator,
			"action", "none, the peer is not the coordinator")
		return
	}

	forCoordinator, accounted := s.tallyConfigHashes(localHash, incomingHash)
	if forCoordinator*2 <= accounted {
		s.logger.Warn("CONFIG_DIVERGENCE: this node differs from the coordinator, but "+
			"the coordinator is not in the majority; not repairing",
			"coordinator", senderID, "coordinatorHash", shortHash(incomingHash),
			"localHash", shortHash(localHash), "agreeingWithCoordinator", forCoordinator,
			"nodesAccountedFor", accounted,
			"action", "none, the coordinator may be the diverged node")
		return
	}

	s.logger.Warn("CONFIG_DIVERGENCE: config differs from the coordinator and the "+
		"cluster agrees with the coordinator, pulling a repair",
		"coordinator", senderID, "coordinatorHash", shortHash(incomingHash),
		"localHash", shortHash(localHash), "agreeingWithCoordinator", forCoordinator,
		"nodesAccountedFor", accounted)
	s.startConfigRepair(senderID)
}

// tallyConfigHashes counts how many nodes hold the coordinator's hash, and how
// many this node can account for at all.
//
// The electorate is this node plus every peer whose last report is still fresh --
// deliberately *not* the nodes in the local config, which would break the tally on
// the exact case it exists for. A node diverged by a missed join is missing a
// member; filtering the tally through its own config would discard that member's
// vote and leave a three-node cluster unable to reach a majority about a node two
// of its three members can see. A peer that reaches this node's ConfigSync is a
// cluster member whatever the local config has lost -- it is already trusted to
// push a config here, so counting its report is not a new grant.
//
// A peer that has never reported a hash counts in neither tally: its view is
// unknown, and reading unknown as agreement with either side would invent
// evidence. The one case this is wrong about is a node removed from the cluster
// that keeps broadcasting, which would go on voting -- but a removed node still
// speaking ConfigSync can already push a whole config here, so its vote is the
// smaller half of that problem.
func (s *Server) tallyConfigHashes(localHash, coordinatorHash string) (forCoordinator, accounted int) {
	s.RLock()
	localID, _ := s.config.GetLocalNodeUUID()
	s.RUnlock()

	// This node, voting for what it holds.
	accounted = 1
	if localHash == coordinatorHash {
		forCoordinator++
	}

	s.peerHashMu.Lock()
	defer s.peerHashMu.Unlock()
	for id, ph := range s.peerHashes {
		if id == localID || id == "" {
			continue
		}
		if time.Since(ph.seen) > peerHashFreshness {
			continue
		}
		accounted++
		if ph.hash == coordinatorHash {
			forCoordinator++
		}
	}
	return forCoordinator, accounted
}

// startConfigRepair runs at most one repair at a time, no more often than
// configRepairInterval, in its own goroutine so an RPC handler never waits on a
// round trip to another node.
func (s *Server) startConfigRepair(coordinatorID string) {
	if !s.repairInFlight.CompareAndSwap(false, true) {
		return
	}

	last := s.lastConfigRepair.Load()
	if last != nil && time.Since(*last) < configRepairInterval {
		s.repairInFlight.Store(false)
		return
	}
	now := time.Now()
	s.lastConfigRepair.Store(&now)

	go func() {
		defer s.repairInFlight.Store(false)
		s.repairConfigFrom(coordinatorID)
	}()
}

// repairConfigFrom fetches the coordinator's config and applies it to this node.
//
// The payload comes back carrying `repair_for` set to this node's id, which is what
// lets it be applied at a generation this node already holds -- the case the
// generation guard exists to refuse, and the case #103 is made of. The exemption is
// addressed: a node only honours `repair_for` naming itself, so this payload cannot
// be replayed at a third node to force a config on it.
func (s *Server) repairConfigFrom(coordinatorID string) {
	s.RLock()
	localID, _ := s.config.GetLocalNodeUUID()
	node := s.config.Nodes[coordinatorID]
	s.RUnlock()

	if localID == "" || node == nil {
		s.logger.Warn("CONFIG_REPAIR: cannot pull, the coordinator is not in the local config",
			"coordinator", coordinatorID)
		return
	}

	client, err := s.getPeerClient(coordinatorID, node)
	if err != nil {
		s.logger.Warn("CONFIG_REPAIR: cannot reach the coordinator to pull a repair",
			"coordinator", coordinatorID, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := client.Server().FetchConfig(ctx, &rpc.FetchConfigRequest{RequesterId: localID})
	cancel()
	if err != nil {
		s.logger.Warn("CONFIG_REPAIR: FetchConfig failed", "coordinator", coordinatorID,
			"error", err)
		return
	}
	if resp == nil || !resp.Success || len(resp.Config) == 0 {
		message := "no config returned"
		if resp != nil && resp.Message != "" {
			message = resp.Message
		}
		s.logger.Warn("CONFIG_REPAIR: the coordinator would not serve its config",
			"coordinator", coordinatorID, "message", message)
		return
	}

	// Applied through the ordinary inbound path rather than a second copy of it:
	// preserving local identity and settings, honouring a carried group's absence
	// (docs/TEST-PLAN.md #43) and persisting the result are all one function, and a
	// repair that skipped any of them would be a new way to diverge.
	applyCtx, applyCancel := context.WithTimeout(context.Background(),
		configSyncTimeoutFor(len(resp.Config)))
	defer applyCancel()
	applied, err := s.ConfigSync(applyCtx, &rpc.ConfigSyncRequest{Config: resp.Config})
	switch {
	case err != nil:
		s.logger.Error("CONFIG_REPAIR: applying the coordinator's config failed",
			"coordinator", coordinatorID, "error", err)
	case applied == nil || !applied.Success:
		message := ""
		if applied != nil {
			message = applied.Message
		}
		s.logger.Error("CONFIG_REPAIR: the coordinator's config was not applied",
			"coordinator", coordinatorID, "message", message)
	default:
		after, hashErr := sharedConfigHash(s.currentConfig())
		if hashErr != nil {
			s.logger.Warn("CONFIG_REPAIR: applied, but could not re-fingerprint",
				"coordinator", coordinatorID, "error", hashErr)
			return
		}
		s.logger.Info("CONFIG_REPAIR: repaired this node's config from the coordinator",
			"coordinator", coordinatorID, "localHash", shortHash(after))
	}
}

// FetchConfig serves this node's config to a peer that has established its own has
// diverged. It is a read: nothing about the asking node changes here, and a node
// that has not asked is never sent anything.
func (s *Server) FetchConfig(ctx context.Context, req *rpc.FetchConfigRequest) (*rpc.FetchConfigResponse, error) {
	if req == nil || req.RequesterId == "" {
		return &rpc.FetchConfigResponse{
			Success: false,
			Message: "no requester id provided",
		}, nil
	}

	s.RLock()
	localID, _ := s.config.GetLocalNodeUUID()
	stamp := s.loadConfigStamp()
	states := s.memberStatesForBroadcast()
	payload, err := buildRepairConfigPayload(s.config, states, s.clusterEpoch, s.leaderID,
		localID, stamp, req.RequesterId)
	s.RUnlock()

	if err != nil {
		s.logger.Error("CONFIG_REPAIR: failed to build a repair payload",
			"requester", req.RequesterId, "error", err)
		return &rpc.FetchConfigResponse{
			Success: false,
			Message: "failed to build config payload",
		}, nil
	}

	s.logger.Info("CONFIG_REPAIR: serving this node's config to a diverged peer",
		"requester", req.RequesterId, "version", stamp.version)
	return &rpc.FetchConfigResponse{Success: true, Config: payload}, nil
}

// clusterCoordinatorID asks the health checker who the coordinator is, so the
// detector and the periodic reconcile cannot disagree about it -- they read the
// same function over the same member list.
//
// The seam is here because who the coordinator is decides everything this file
// does, and a test that had to stand up a health checker and age a member's
// LastHCResponse to move the role would be testing clusterCoordinator again
// rather than the detector. Same idiom as IPMonitor.enforce and the deep-check
// seam in membership.
func (s *Server) clusterCoordinatorID() string {
	if s.coordinatorID != nil {
		return s.coordinatorID()
	}
	if s.healthCheck == nil {
		return ""
	}
	return s.healthCheck.Coordinator()
}
