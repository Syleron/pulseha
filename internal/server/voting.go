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
	resp := &rpc.RequestVoteResponse{SessionId: req.SessionId, VoterId: cfg.Pulse.LocalNode, Epoch: req.Epoch, ProtocolVersion: 2, Phase: req.Phase, Ballot: req.Ballot}
	deny := func(reason string) *rpc.RequestVoteResponse { resp.Reason = reason; return resp }
	ids := votingMembers(cfg)
	if req.SessionId == "" || len(ids) < 3 || !slices.Equal(ids, req.MemberIds) || !slices.Contains(ids, cfg.Pulse.LocalNode) || !slices.Contains(ids, req.InitiatorId) {
		return deny("proposal electorate does not match this cluster")
	}
	if !time.Now().Before(time.UnixMilli(req.ExpiresAtUnixMilli)) {
		return deny("proposal expired")
	}
	if req.Ballot == 0 || (req.Phase != "prepare" && req.Phase != "accept") {
		return deny("v2 prepare/accept ballot required")
	}
	epoch, err := s.votingEpoch(cfg)
	if err != nil {
		return deny("durable vote state unavailable: " + err.Error())
	}
	if req.Epoch != epoch {
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
	if req.VoteType != string(quorum.VoteTypeNodeStatus) && req.VoteType != string(quorum.VoteTypeIPRedistribution) {
		return deny("unsupported proposal type")
	}
	if req.Phase == "accept" {
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
	}
	// No server/config locks are held across disk I/O. Serialize the durable
	// promise and acceptance before replying, including our own local ballot.
	s.voteMu.Lock()
	defer s.voteMu.Unlock()
	if err := s.loadVotesLocked(cfg); err != nil {
		return deny(err.Error())
	}
	if req.Epoch != s.GetClusterEpoch()+1 {
		return deny("cluster epoch changed during voting")
	}
	if err := s.voteSlotLocked(req.Epoch, ids); err != nil {
		return deny(err.Error())
	}
	slot := s.voteState.Slots[req.VoteType]
	fill := func() {
		resp.PromisedBallot, resp.PromisedBy = slot.Promise.Number, slot.Promise.Node
		resp.AcceptedBallot, resp.AcceptedBy, resp.AcceptedSubject = slot.Accepted.Number, slot.Accepted.Node, slot.Subject
	}
	fill()
	ballot := voteBallot{req.Ballot, req.InitiatorId}
	if ballot.less(slot.Promise) {
		return deny("ballot was superseded")
	}
	if req.Phase == "prepare" {
		slot.Promise = ballot
	} else {
		if ballot != slot.Promise {
			return deny("prepare required before acceptance")
		}
		if slot.Accepted == ballot && slot.Subject != req.Subject {
			return deny("ballot already accepted another proposal")
		}
		slot.Accepted, slot.Subject = ballot, req.Subject
	}
	s.voteState.Slots[req.VoteType] = slot
	if err := s.persistVotesLocked(); err != nil {
		return deny("cannot persist vote: " + err.Error())
	}
	fill()
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
	roundTimeout := 6 * time.Second
	if timeoutSeconds < 6 {
		roundTimeout = time.Duration(timeoutSeconds) * time.Second
	}
	roundDeadline := time.Now().Add(roundTimeout)
	if roundDeadline.Before(deadline) {
		deadline = roundDeadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	// One proposer round per daemon at a time. Remote proposers are ordered
	// by (counter, node ID), so concurrent counters do not identify one ballot.
	s.voteRoundMu.Lock()
	defer s.voteRoundMu.Unlock()
	epoch, err := s.votingEpoch(cfg)
	if err != nil {
		return err
	}
	if err := s.quorumManager.BindSessionEpoch(sessionID, epoch); err != nil {
		return err
	}
	ballot, err := s.nextVoteBallot(cfg)
	if err != nil {
		return err
	}
	req := &rpc.RequestVoteRequest{SessionId: sessionID, InitiatorId: cfg.Pulse.LocalNode,
		VoteType: voteType, Subject: subject, Description: description, MemberIds: session.MemberIDs,
		Epoch: epoch, ExpiresAtUnixMilli: session.EndTime.UnixMilli(), ClusterToken: cfg.Pulse.ClusterToken,
		Phase: "prepare", Ballot: ballot}
	promises := s.collectVotePhase(ctx, cfg, req)
	granted := 0
	var highest voteBallot
	recovered := subject
	var observed uint64
	for _, response := range promises {
		observed = max(observed, response.PromisedBallot)
		if !response.Granted {
			continue
		}
		granted++
		accepted := voteBallot{response.AcceptedBallot, response.AcceptedBy}
		if accepted.Number > 0 && highest.less(accepted) {
			highest, recovered = accepted, response.AcceptedSubject
		}
	}
	if err := s.observeVoteBallot(cfg, observed); err != nil {
		return err
	}
	if granted < len(session.MemberIDs)/2+1 {
		return fmt.Errorf("prepare did not obtain a configured majority")
	}
	if err := s.quorumManager.RecoverProposal(sessionID, recovered); err != nil {
		return err
	}
	req.Phase, req.Subject = "accept", recovered
	responses := s.collectVotePhase(ctx, cfg, req)
	for id, response := range responses {
		decision := quorum.VoteDecisionNo
		if response.Granted && response.AcceptedSubject == recovered && response.AcceptedBallot == ballot && response.AcceptedBy == cfg.Pulse.LocalNode {
			decision = quorum.VoteDecisionYes
		}
		_ = s.quorumManager.CastVote(sessionID, id, decision)
	}
	result, err := s.quorumManager.GetVotingSession(sessionID)
	if err != nil {
		return err
	}
	if result.Result == nil || !result.Result.Passed {
		return fmt.Errorf("accept did not obtain a configured majority")
	}
	return nil
}

// collectVotePhase counts only authenticated replies to this exact phase and
// ballot. Prepare responses are promises, never votes in the session result.
func (s *Server) collectVotePhase(ctx context.Context, cfg *config.Config, req *rpc.RequestVoteRequest) map[string]*rpc.RequestVoteResponse {
	phaseCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ctx = phaseCtx
	responses := make(map[string]*rpc.RequestVoteResponse)
	var wg sync.WaitGroup
	type reply struct {
		id       string
		response *rpc.RequestVoteResponse
	}
	replies := make(chan reply, len(req.MemberIds))
	for _, id := range req.MemberIds {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			var resp *rpc.RequestVoteResponse
			if ctx.Err() != nil {
				return
			}
			if id == cfg.Pulse.LocalNode {
				resp = s.decideVote(cfg, req)
			} else {
				node := cfg.Nodes[id]
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
				resp, err = c.Server().RequestVote(ctx, req, grpc.Peer(&remote))
				if err != nil {
					return
				}
				if cfg.Pulse.TLSRequired() {
					if err := verifyVoter(peer.NewContext(ctx, &remote), cfg, id, ""); err != nil {
						return
					}
				}
			}
			if resp == nil || resp.ProtocolVersion != 2 || resp.SessionId != req.SessionId || resp.VoterId != id || resp.Epoch != req.Epoch || resp.Phase != req.Phase || resp.Ballot != req.Ballot {
				return
			}
			if resp.Granted {
				if resp.PromisedBallot != req.Ballot || resp.PromisedBy != req.InitiatorId {
					return
				}
				accepted := voteBallot{resp.AcceptedBallot, resp.AcceptedBy}
				if (voteBallot{req.Ballot, req.InitiatorId}).less(accepted) {
					return
				}
				if accepted.Number > 0 && (resp.AcceptedSubject == "" || !slices.Contains(req.MemberIds, accepted.Node)) {
					return
				}
			}
			replies <- reply{id, resp}
		}(id)
	}
	go func() { wg.Wait(); close(replies) }()
	grants := 0
	for reply := range replies {
		responses[reply.id] = reply.response
		if reply.response.Granted {
			grants++
		}
		// Do not wait on dead peers once a majority has replied. Join workers
		// before the caller changes phase or consumes any shared state.
		if grants >= len(req.MemberIds)/2+1 {
			cancel()
		}
	}
	return responses
}
