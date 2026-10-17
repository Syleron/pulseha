package server

import (
	"context"
	"sort"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/rpc"
)

// witnessHistory is what the Witness last saw held without contest: the
// Elected Node in active-passive, and each address's holder in active-active.
// It decides a Ruling's keeper and nothing else. It is never reported as
// liveness -- a peer's presence in it says only that it once held something.
//
// Recorded by the Witness from its own observations, never taken from the
// cluster's state broadcasts. During a partition both sides broadcast
// themselves as leader under epochs they bumped independently, so the
// broadcast leader would be whichever side changed its mind last -- the node
// that promoted into the partition, not the one that served throughout
// (ADR-0006, "the tie-break is history the Witness saw").
type witnessHistory struct {
	mu      pulselock.Mutex
	elected string
	holders map[string]string
}

// record updates the history from one round of claims, but only from a round
// in which nothing overlaps. A round with an overlap is the thing a Ruling
// settles; letting it into the history would let the contest decide itself.
func (h *witnessHistory) record(mode string, claims []*rpc.MemberClaim) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if mode == "active-active" {
		seen := make(map[string]string)
		for _, c := range claims {
			if statusOf(c) != membership.StatusActive {
				continue
			}
			for _, ip := range c.GetActiveIps() {
				if other, dup := seen[ip]; dup && other != c.GetNodeId() {
					return
				}
				seen[ip] = c.GetNodeId()
			}
		}
		if h.holders == nil {
			h.holders = make(map[string]string, len(seen))
		}
		for ip, holder := range seen {
			h.holders[ip] = holder
		}
		return
	}

	active := ""
	for _, c := range claims {
		if statusOf(c) != membership.StatusActive {
			continue
		}
		if active != "" {
			return
		}
		active = c.GetNodeId()
	}
	if active != "" {
		h.elected = active
	}
}

func (h *witnessHistory) snapshot() (string, map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	holders := make(map[string]string, len(h.holders))
	for ip, holder := range h.holders {
		holders[ip] = holder
	}
	return h.elected, holders
}

// computeRulings is the Witness's decision on every overlap among claims, given
// what it last saw held without contest. Pure.
//
// Active-passive: the conflict is two Active Claims, whatever addresses they
// list, and the ruling covers every address. Active-active: two Actives is the
// healthy state, and the conflict is an address in more than one Claim; each
// such address is ruled on by itself, so a contender ruled against gives back
// only what it took and keeps what it held before (ADR-0006).
//
// The keeper is the historical holder when that holder is among the contenders,
// and the lowest node ID otherwise -- which is only the case when the Witness has
// no observation to go on, as after its own restart in the middle of a partition.
func computeRulings(mode string, claims []*rpc.MemberClaim, elected string, holders map[string]string) []*rpc.Ruling {
	if mode == "active-active" {
		claimants := make(map[string][]string)
		for _, c := range claims {
			if statusOf(c) != membership.StatusActive {
				continue
			}
			for _, ip := range c.GetActiveIps() {
				claimants[ip] = appendUnique(claimants[ip], c.GetNodeId())
			}
		}
		// Group the contested addresses by (keeper, contenders), so one Ruling
		// names each distinct decision once.
		type key struct{ keeper, contenders string }
		grouped := make(map[key]*rpc.Ruling)
		for ip, ids := range claimants {
			if len(ids) < 2 {
				continue
			}
			sort.Strings(ids)
			keeper := ids[0]
			if h := holders[ip]; contains(ids, h) {
				keeper = h
			}
			k := key{keeper, joinIDs(ids)}
			if grouped[k] == nil {
				grouped[k] = &rpc.Ruling{Keeper: keeper, Contenders: ids}
			}
			grouped[k].Addresses = append(grouped[k].Addresses, ip)
		}
		rulings := make([]*rpc.Ruling, 0, len(grouped))
		for _, r := range grouped {
			sort.Strings(r.Addresses)
			rulings = append(rulings, r)
		}
		sort.Slice(rulings, func(i, j int) bool { return rulings[i].Addresses[0] < rulings[j].Addresses[0] })
		return rulings
	}

	var actives []string
	for _, c := range claims {
		if statusOf(c) == membership.StatusActive {
			actives = appendUnique(actives, c.GetNodeId())
		}
	}
	if len(actives) < 2 {
		return nil
	}
	sort.Strings(actives)
	keeper := actives[0]
	if contains(actives, elected) {
		keeper = elected
	}
	return []*rpc.Ruling{{Keeper: keeper, Contenders: actives}}
}

// rulingsFor is the Witness's Rulings for a report: the asker's own Claim, which
// it carried, and every peer the Witness saw for this report. A peer that was not
// visible has no Claim to weigh -- it is not a contender, whatever it held.
func (s *Server) rulingsFor(mode string, asker *rpc.MemberClaim, views []*rpc.PeerView) []*rpc.Ruling {
	claims := make([]*rpc.MemberClaim, 0, len(views)+1)
	if asker != nil {
		claims = append(claims, asker)
	}
	for _, v := range views {
		if v.GetVisible() {
			claims = append(claims, v.GetClaim())
		}
	}
	elected, holders := s.witnessHistory.snapshot()
	return computeRulings(mode, claims, elected, holders)
}

// observeForHistory is the Witness's own round: every peer, asked there and
// then, recorded if nothing overlaps. Run on the Witness's tick.
func (s *Server) observeForHistory(ctx context.Context) {
	cfg := s.clusterSnapshot()()
	if cfg == nil {
		return
	}
	cfg.Lock()
	localID := cfg.Pulse.LocalNode
	token := cfg.Pulse.ClusterToken
	mode := cfg.Pulse.Mode
	peers := make(map[string]config.Node, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if id != localID && node != nil {
			peers[id] = *node
		}
	}
	cfg.Unlock()

	ctx, cancel := context.WithTimeout(ctx, witnessAskTimeout)
	defer cancel()
	var claims []*rpc.MemberClaim
	for _, v := range s.observePeers(ctx, localID, token, peers) {
		if v.GetVisible() {
			claims = append(claims, v.GetClaim())
		}
	}
	s.witnessHistory.record(mode, claims)
}

// startWitnessLoop starts, once, the loop that does this node's part of
// arbitration on every tick: the Witness records what it sees. Stopped by Stop.
func (s *Server) startWitnessLoop(interval time.Duration) {
	s.witnessLoopOnce.Do(func() {
		stop := make(chan struct{})
		s.witnessLoopStop = stop
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					s.witnessLoopTick()
				}
			}
		}()
	})
}

func (s *Server) stopWitnessLoop() {
	if s.witnessLoopStop == nil {
		return
	}
	select {
	case <-s.witnessLoopStop:
	default:
		close(s.witnessLoopStop)
	}
}

func (s *Server) witnessLoopTick() {
	cfg := s.clusterSnapshot()()
	if cfg == nil {
		return
	}
	cfg.Lock()
	localID := cfg.Pulse.LocalNode
	localIsWitness := cfg.Nodes[localID].IsWitness()
	mode := cfg.Pulse.Mode
	cfg.Unlock()
	if localIsWitness {
		s.observeForHistory(context.Background())
		return
	}

	// A serving node that cannot see its peer is the one moment, besides a
	// promotion, that asks the Witness. With the peer visible nobody asks, and
	// the cluster behaves exactly as it does without a Witness.
	if mode == "active-passive" && !s.seesEveryPeer(localID) {
		if local := s.memberList.GetMemberByID(localID); local != nil && local.GetStatus() == membership.StatusActive {
			s.witnessWatch(context.Background(), localID)
		}
	}
	if s.witnessFence.isFenced() && s.seesEveryPeer(localID) {
		// The partition is over. Normal reconvergence takes it from here, and a
		// Fenced node that kept its hold-down past that would wait out a
		// silence that no longer means anything.
		s.logger.Info("WITNESS: peer visible again; no longer Fenced")
		s.witnessFence.clear()
	}
}

// seesEveryPeer reports whether every other candidate is answering this node.
func (s *Server) seesEveryPeer(localID string) bool {
	for id, m := range s.memberList.CandidatesSnapshot() {
		if id != localID && m.GetStatus() == membership.StatusUnknown {
			return false
		}
	}
	return true
}

// statusOf is a Claim's status as a membership status. MemberStatusEnum's
// values are MemberStatus's wire ordinals (TestMemberStatusOrdinalsMatchTheProto).
func statusOf(c *rpc.MemberClaim) membership.MemberStatus {
	return membership.MemberStatus(c.GetStatus())
}

func appendUnique(ids []string, id string) []string {
	if contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func joinIDs(ids []string) string {
	out := ""
	for _, id := range ids {
		out += id + "\x00"
	}
	return out
}
