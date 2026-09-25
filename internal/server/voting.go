package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// voteConfig snapshots the mutable config before doing any peer I/O.
func (s *Server) voteConfig() (*config.Config, error) {
	s.RLock()
	defer s.RUnlock()
	if s.config == nil {
		return nil, fmt.Errorf("no cluster configuration")
	}
	s.config.Lock()
	data, err := json.Marshal(s.config)
	s.config.Unlock()
	if err != nil {
		return nil, err
	}
	var cfg config.Config
	err = json.Unmarshal(data, &cfg)
	return &cfg, err
}

func votingMembers(cfg *config.Config) []string {
	ids := make([]string, 0, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if id != "" && node != nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// verifyVoter binds a claimed identity to the certificate in required mode.
// Permissive clusters use their shared secret, and retain that mode's trust
// limitations: the secret does not distinguish one trusted member from another.
func verifyVoter(ctx context.Context, cfg *config.Config, id, token string) error {
	if cfg.Nodes[id] == nil || id == "" {
		return status.Error(codes.PermissionDenied, "unknown voter")
	}
	raw := peerCertificates(ctx)
	if raw != nil || cfg.Pulse.TLSRequired() {
		set, err := clustertls.NewTrustSet(cfg.Nodes)
		if err != nil {
			return status.Error(codes.PermissionDenied, "invalid trust set")
		}
		actual, err := set.VerifyPeer(raw)
		if err != nil || actual != id {
			return status.Error(codes.PermissionDenied, "voter identity does not match certificate")
		}
		return nil
	}
	if token == "" || !tokensEqual(token, cfg.Pulse.ClusterToken) {
		return status.Error(codes.PermissionDenied, "invalid cluster credentials")
	}
	return nil
}

// RequestVote is a peer decision, not a request to cast a vote on somebody
// else's behalf. No remote handler can directly add YES votes to our session.
func (s *Server) RequestVote(ctx context.Context, req *rpc.RequestVoteRequest) (*rpc.RequestVoteResponse, error) {
	cfg, err := s.voteConfig()
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := verifyVoter(ctx, cfg, req.InitiatorId, req.ClusterToken); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.decideVote(cfg, req), nil
}

func (s *Server) decideVote(cfg *config.Config, req *rpc.RequestVoteRequest) *rpc.RequestVoteResponse {
	resp := &rpc.RequestVoteResponse{SessionId: req.SessionId, VoterId: cfg.Pulse.LocalNode, Epoch: req.Epoch}
	deny := func(reason string) *rpc.RequestVoteResponse { resp.Reason = reason; return resp }
	ids := votingMembers(cfg)
	if req.SessionId == "" || len(ids) < 3 || !slices.Equal(ids, req.MemberIds) || !slices.Contains(ids, cfg.Pulse.LocalNode) || !slices.Contains(ids, req.InitiatorId) {
		return deny("proposal electorate does not match this cluster")
	}
	if !time.Now().Before(time.UnixMilli(req.ExpiresAtUnixMilli)) {
		return deny("proposal expired")
	}
	// Epochs order observations here; this is not a durable consensus log or a
	// fencing lease. Keep the vote guard separate from cluster-state adoption.
	if req.Epoch != s.GetClusterEpoch()+1 {
		return deny("proposal epoch does not follow this node's cluster epoch")
	}
	if s.memberList == nil {
		return deny("membership unavailable")
	}
	members := s.memberList.MembersSnapshot()
	for _, id := range ids {
		if members[id] == nil {
			return deny("electorate is not initialized locally")
		}
	}
	local := members[cfg.Pulse.LocalNode]
	if local == nil || local.GetStatus() == membership.StatusUnknown {
		return deny("local member is not ready to vote")
	}
	switch quorum.VoteType(req.VoteType) {
	case quorum.VoteTypeNodeStatus:
		candidate := members[req.Subject]
		if candidate == nil || candidate.GetStatus() != membership.StatusPassive {
			return deny("candidate is not eligible")
		}
		for id, m := range members {
			if id != req.Subject && m.GetStatus() == membership.StatusActive {
				return deny("an active node is still known")
			}
		}
	case quorum.VoteTypeIPRedistribution:
		var ips []string
		if json.Unmarshal([]byte(req.Subject), &ips) != nil || len(ips) == 0 {
			return deny("proposal must name the addresses to redistribute")
		}
		known := make(map[string]bool)
		for _, group := range cfg.Groups {
			for _, ip := range group {
				known[ip] = true
			}
		}
		wanted := make(map[string]bool)
		for _, ip := range ips {
			if !known[ip] {
				return deny("proposal contains an unconfigured address")
			}
			wanted[ip] = true
		}
		grace := time.Duration(cfg.Pulse.FailOverLimit) * time.Millisecond
		for _, m := range members {
			health := m.GetHealthStatus()
			if health.Status == membership.StatusUnknown && time.Since(health.LastResponse) > grace {
				continue
			}
			for _, ip := range health.ActiveIPs {
				if wanted[ip] {
					return deny("a member still claims a proposed address")
				}
			}
		}
	default:
		// No remotely evaluable config-change proposal exists yet. Never approve an
		// arbitrary description as authority to change membership/configuration.
		return deny("unsupported proposal type")
	}
	// Serialize conflicting grants in one epoch, including this node's own
	// proposal. The guard is intentionally in-memory, like clusterEpoch; durable
	// epoch recovery and fencing remain separate ownership work.
	s.voteMu.Lock()
	defer s.voteMu.Unlock()
	if req.Epoch < s.voteEpoch {
		return deny("proposal is older than a prior vote")
	}
	if req.Epoch > s.voteEpoch {
		s.voteEpoch = req.Epoch
		s.voteDecisions = make(map[string]string)
	}
	key := req.VoteType
	if previous, ok := s.voteDecisions[key]; ok && previous != req.Subject {
		return deny("already granted a conflicting proposal in this epoch")
	}
	s.voteDecisions[key] = req.Subject
	resp.Granted = true
	return resp
}

// BroadcastVoteRequest gathers explicit decisions from a fixed electorate.
// Transport errors, malformed responses and legacy Unimplemented RPCs do not
// count. The session's deadline is authoritative even if the caller retries.
func (s *Server) BroadcastVoteRequest(sessionID, voteType, subject, description string, timeoutSeconds int64) error {
	if s.quorumManager == nil {
		return fmt.Errorf("quorum unavailable")
	}
	session, err := s.quorumManager.GetVotingSession(sessionID)
	if err != nil {
		return err
	}
	if string(session.Type) != voteType || session.Subject != subject || session.Description != description {
		return fmt.Errorf("proposal does not match voting session")
	}
	cfg, err := s.voteConfig()
	if err != nil {
		return err
	}
	if !slices.Equal(session.MemberIDs, votingMembers(cfg)) {
		return fmt.Errorf("membership changed since voting began")
	}
	if timeoutSeconds <= 0 {
		return fmt.Errorf("invalid vote timeout")
	}
	deadline := session.EndTime
	// Bound each round as well as the full session. Constructing a client does
	// not connect; the actual RPC must have a deadline for dead/blackholed peers.
	roundTimeout := 3 * time.Second
	if timeoutSeconds < 3 {
		roundTimeout = time.Duration(timeoutSeconds) * time.Second
	}
	roundDeadline := time.Now().Add(roundTimeout)
	if roundDeadline.Before(deadline) {
		deadline = roundDeadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	epoch := s.GetClusterEpoch() + 1
	if err := s.quorumManager.BindSessionEpoch(sessionID, epoch); err != nil {
		return err
	}
	req := &rpc.RequestVoteRequest{SessionId: sessionID, InitiatorId: cfg.Pulse.LocalNode,
		VoteType: voteType, Subject: subject, Description: description, MemberIds: session.MemberIDs,
		Epoch: epoch, ExpiresAtUnixMilli: session.EndTime.UnixMilli(), ClusterToken: cfg.Pulse.ClusterToken}
	local := s.decideVote(cfg, req)
	if !local.Granted {
		return fmt.Errorf("local vote refused: %s", local.Reason)
	}
	if err := s.quorumManager.CastVote(sessionID, cfg.Pulse.LocalNode, quorum.VoteDecisionYes); err != nil {
		return err
	}

	var wg sync.WaitGroup
	var mu pulselock.Mutex
	responses := 0
	for _, id := range session.MemberIDs {
		if id == cfg.Pulse.LocalNode {
			continue
		}
		node := cfg.Nodes[id]
		wg.Add(1)
		go func(id string, node *config.Node) {
			defer wg.Done()
			c, err := client.New()
			if err != nil {
				return
			}
			defer c.Close()
			creds, err := clustertls.ClientCredentials(s.certDir, func() *config.Config { return cfg })
			if err != nil {
				return
			}
			if err := c.Connect(node.IP, node.Port, creds); err != nil {
				return
			}
			var remote peer.Peer
			resp, err := c.Server().RequestVote(ctx, req, grpc.Peer(&remote))
			if err != nil {
				s.logger.Debug("Peer did not vote", "node", id, "error", err)
				return
			}
			if resp == nil || resp.SessionId != req.SessionId || resp.VoterId != id || resp.Epoch != req.Epoch {
				return
			}
			if cfg.Pulse.TLSRequired() {
				if err := verifyVoter(peer.NewContext(ctx, &remote), cfg, id, ""); err != nil {
					return
				}
			}
			s.logger.Debug("Peer returned a vote", "node", id, "session", sessionID, "granted", resp.Granted, "reason", resp.Reason)
			decision := quorum.VoteDecisionNo
			if resp.Granted {
				decision = quorum.VoteDecisionYes
			}
			if err := s.quorumManager.CastVote(sessionID, id, decision); err != nil {
				s.logger.Debug("Peer vote not recorded", "node", id, "error", err)
				return
			}
			mu.Lock()
			responses++
			mu.Unlock()
		}(id, node)
	}
	wg.Wait()
	if responses == 0 {
		return fmt.Errorf("no peer votes received")
	}
	return nil
}
