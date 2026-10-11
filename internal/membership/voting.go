package membership

import (
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/syleron/pulseha/internal/quorum"
)

// requestQuorumDecision returns the recovered proposal, which can differ from
// the request if an earlier ballot may already have obtained a majority.
func (h *HealthChecker) requestQuorumDecision(kind quorum.VoteType, subject, description string) (*quorum.VotingSession, bool) {
	if h.server == nil || h.server.GetQuorumManager() == nil {
		return nil, false
	}
	manager := h.server.GetQuorumManager()
	id, err := manager.StartVotingSession(kind, subject, description, 5*time.Second)
	if err != nil {
		return nil, false
	}
	if err = h.server.BroadcastVoteRequest(id, string(kind), subject, description, 5); err != nil {
		h.logger.Warn("Quorum round did not complete", "error", err)
		return nil, false
	}
	result, err := manager.GetVotingSession(id)
	if err != nil || result.Result == nil || !result.Result.Passed || !result.Result.QuorumMet || !time.Now().Before(result.EndTime) || result.Epoch != h.server.GetClusterEpoch()+1 {
		return nil, false
	}
	cfg := h.members.Config()
	if cfg == nil {
		return nil, false
	}
	cfg.Lock()
	ids := make([]string, 0, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if id != "" && node != nil {
			ids = append(ids, id)
		}
	}
	cfg.Unlock()
	sort.Strings(ids)
	return result, slices.Equal(ids, result.MemberIDs)
}

func (h *HealthChecker) approvedRedistribution(ips []string) ([]string, bool) {
	cfg := h.members.Config()
	if cfg == nil {
		return nil, false
	}
	cfg.Lock()
	size := len(cfg.Nodes)
	cfg.Unlock()
	if size > 0 && size < 3 {
		return ips, true
	}
	ordered := slices.Clone(ips)
	sort.Strings(ordered)
	encoded, _ := json.Marshal(ordered)
	decision, ok := h.requestQuorumDecision(quorum.VoteTypeIPRedistribution, string(encoded), "Redistribute orphaned addresses")
	if !ok {
		return nil, false
	}
	var approved []string
	if json.Unmarshal([]byte(decision.Subject), &approved) != nil || len(approved) == 0 {
		return nil, false
	}
	// A recovered proposal may be a subset. Never place an address that the
	// current reconciliation does not regard as orphaned.
	for _, ip := range approved {
		if !slices.Contains(ordered, ip) {
			return nil, false
		}
	}
	return approved, true
}

// votedElectionCandidate follows the recovered value rather than promoting the
// candidate initially requested. A successful round may have recovered a prior
// majority whose response was lost.
func (h *HealthChecker) votedElectionCandidate(candidate *Member) (*Member, bool) {
	if candidate == nil {
		return nil, false
	}
	cfg := h.members.Config()
	if cfg == nil {
		return nil, false
	}
	cfg.Lock()
	size := len(cfg.Nodes)
	configured := cfg.Nodes[candidate.ID] != nil
	cfg.Unlock()
	if !configured || candidate.GetStatus() != StatusPassive {
		return nil, false
	}
	if size > 0 && size < 3 {
		return candidate, true
	}
	decision, ok := h.requestQuorumDecision(quorum.VoteTypeNodeStatus, candidate.ID, "Elect active node")
	if !ok {
		return nil, false
	}
	recovered := h.members.GetMemberByID(decision.Subject)
	if recovered == nil || recovered.GetStatus() != StatusPassive {
		return nil, false
	}
	return recovered, true
}
