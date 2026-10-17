package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/rpc"
)

// witnessAnswer is what a node about to promote over an unreachable peer
// learned from the Witness, in the terms canPromoteWithoutConfirmedRelease
// decides on.
type witnessAnswer int

const (
	// witnessSilent: no Witness, or none that answered as itself. Today's
	// behaviour, deliberately kept (ADR-0002).
	witnessSilent witnessAnswer = iota
	// witnessSilentWhileFenced: as above, but this node is Fenced and the
	// Witness has been quiet for less than the hold-down. A Witness that drops
	// one packet in ten must not swing the service across once a cycle.
	witnessSilentWhileFenced
	// witnessPeerNotVisible: the Witness asked the peer and got no answer. The
	// peer is gone, not merely cut off from this node.
	witnessPeerNotVisible
	// witnessPeerIdle: the peer answered the Witness and is not claiming Active.
	witnessPeerIdle
	// witnessPeerServing: the peer answered the Witness claiming Active. Cut off
	// from this node, not dead.
	witnessPeerServing
)

func (a witnessAnswer) String() string {
	switch a {
	case witnessSilentWhileFenced:
		return "silent-while-fenced"
	case witnessPeerNotVisible:
		return "peer-not-visible"
	case witnessPeerIdle:
		return "peer-idle"
	case witnessPeerServing:
		return "peer-serving"
	default:
		return "silent"
	}
}

// witnessFence is this node's side of arbitration: whether it is Fenced, and
// when the Witness last answered it. In memory only, and cleared by a restart:
// a persisted Fenced could outlive the situation that justified it (ADR-0006).
type witnessFence struct {
	mu         pulselock.Mutex
	fenced     bool
	lastAnswer time.Time
	contested  *contested
	// ruledOut is what a Ruling took from this node in active-active, held back
	// from redistribution for as long as the hold-down lasts.
	ruledOut []string
	// peerServing is what the Witness last said a peer this node cannot see is
	// serving, in active-active.
	peerServing map[string]bool
	// reannounced is the Rulings this node, as keeper, has already re-announced
	// for. A settled Ruling keeps arriving for a hold-down; the announcement it
	// calls for is made once.
	reannounced map[string]time.Time
}

func (f *witnessFence) answered(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAnswer = at
}

func (f *witnessFence) fence(at time.Time, ruledOut []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced = true
	f.lastAnswer = at
	f.ruledOut = append([]string(nil), ruledOut...)
}

func (f *witnessFence) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced = false
	f.ruledOut = nil
	f.peerServing = nil
}

func (f *witnessFence) isFenced() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fenced
}

// heldDown reports whether a Fenced node is still inside its hold-down: the
// Witness answered it less than holdDown ago.
func (f *witnessFence) heldDown(now time.Time, holdDown time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fenced && now.Sub(f.lastAnswer) < holdDown
}

// witnessHoldDown is how long a Fenced node waits out Witness silence before
// falling back to both-Active: longer than peer-failure detection, so a Witness
// that has really gone is distinguishable from one that dropped a reply.
func (s *Server) witnessHoldDown() time.Duration {
	cfg := s.clusterSnapshot()()
	limit := time.Duration(0)
	if cfg != nil {
		cfg.Lock()
		limit = time.Duration(cfg.Pulse.FailOverLimit) * time.Millisecond
		cfg.Unlock()
	}
	return max(2*limit, 2*witnessAskTimeout)
}

// witnessAnswerFor asks the Witness about peerID on behalf of a promotion.
func (s *Server) witnessAnswerFor(ctx context.Context, peerID string) witnessAnswer {
	now := time.Now()
	resp, err := s.askWitness(ctx)
	if err != nil {
		if !errors.Is(err, errNoWitness) {
			s.logger.Warn("WITNESS: no usable report before promoting", "peer", peerID, "error", err)
		}
		if s.witnessFence.heldDown(now, s.witnessHoldDown()) {
			return witnessSilentWhileFenced
		}
		return witnessSilent
	}
	s.witnessFence.answered(now)

	for _, v := range resp.GetPeers() {
		if v.GetNodeId() != peerID {
			continue
		}
		switch {
		case !v.GetVisible():
			// The peer is gone: whatever this node was ruled out of, there is
			// nobody left to keep it from.
			s.witnessFence.clear()
			return witnessPeerNotVisible
		case statusOf(v.GetClaim()) == membership.StatusActive:
			return witnessPeerServing
		default:
			return witnessPeerIdle
		}
	}
	// A report that does not mention the peer at all is not evidence about it.
	return witnessSilent
}

// contested is, on the node a Ruling kept, what it is waiting for the losers to
// release before it re-announces. Kept beside the fence because the two are the
// same arbitration from opposite sides. addresses is nil in active-passive,
// where the Ruling covers everything.
type contested struct {
	since     time.Time
	losers    []string
	addresses []string
}

// witnessWatch is a serving node's tick while it cannot see a peer: ask the
// Witness, and act on the Ruling.
//
// Ruled against: Fenced. In active-passive it stands down; in active-active it
// gives back only the addresses ruled on and keeps serving the rest. Ruled for:
// it notes the losers, and re-announces once a later report shows them no
// longer claiming, because the network follows whoever announced last and that
// is usually the loser, which promoted into the partition. Re-announcing on the
// Ruling itself would race the loser's own last announcement; on the release it
// cannot (ADR-0006).
//
// In active-active it also records which addresses a peer it cannot see is
// still serving to the Witness, so that orphan redistribution leaves them alone
// rather than taking them and making an overlap to rule on.
//
// Witness silence never fences. It forgets what the Witness last said the peer
// was serving -- silence is tier 3 -- and lets a pending re-announcement age
// out, so a Witness that disappears between the Ruling and the release still
// cannot leave the address pointed at a node that has let go of it.
func (s *Server) witnessWatch(ctx context.Context, localID, mode string) {
	now := time.Now()
	resp, err := s.askWitness(ctx)
	if err != nil {
		s.setPeerServing(nil)
		if c := s.takeContestedOlderThan(now, s.witnessHoldDown()); c != nil {
			s.logger.Warn("WITNESS: no report since the Ruling; re-announcing on the age-out",
				"losers", c.losers, "since", c.since)
			s.reannounce(localID)
		}
		return
	}
	s.witnessFence.answered(now)

	if mode == "active-active" {
		serving := make(map[string]bool)
		for _, v := range resp.GetPeers() {
			if v.GetVisible() && statusOf(v.GetClaim()) == membership.StatusActive {
				for _, ip := range v.GetClaim().GetActiveIps() {
					serving[ip] = true
				}
			}
		}
		s.setPeerServing(serving)
	}

	if against := rulingsAgainst(resp.GetRulings(), localID); len(against) > 0 {
		addrs := rulingAddresses(against)
		s.logger.Warn("WITNESS: ruled against; standing down", "keeper", against[0].GetKeeper(),
			"contenders", against[0].GetContenders(), "addresses", len(addrs))
		var err error
		if mode == "active-active" {
			err = s.releaseForRuling(ctx, localID, addrs)
		} else {
			err = s.demoteForRuling(ctx, localID)
		}
		if err != nil {
			// Still claiming and still contested; the next tick asks again.
			s.logger.Error("WITNESS: could not stand down after a Ruling against this node", "error", err)
			return
		}
		s.witnessFence.fence(now, addrs)
		if mode != "active-active" {
			// Stood down entirely: there is nothing left to have kept.
			s.clearContested()
			return
		}
		// In active-active the same report can rule this node out of the
		// addresses it took and in on the ones it held all along -- each side
		// took the other's groups -- so carry on to what it kept.
	}

	for _, r := range resp.GetRulings() {
		if r.GetKeeper() == localID && s.markReannounced(now, r) {
			s.noteContested(now, r.GetContenders(), r.GetAddresses(), localID)
		}
	}
	if c := s.contestedSnapshot(); c != nil && losersReleased(resp.GetPeers(), c.losers, c.addresses) {
		s.logger.Info("WITNESS: the node ruled against has released; re-announcing", "losers", c.losers)
		s.clearContested()
		s.reannounce(localID)
	}
}

// rulingsAgainst returns every Ruling in which localID is a contender and not
// the keeper.
func rulingsAgainst(rulings []*rpc.Ruling, localID string) []*rpc.Ruling {
	var out []*rpc.Ruling
	for _, r := range rulings {
		// Never a settled one: it was decided for an earlier report, and this
		// node may since have taken over legitimately.
		if !r.GetSettled() && r.GetKeeper() != localID && contains(r.GetContenders(), localID) {
			out = append(out, r)
		}
	}
	return out
}

// rulingAddresses is every address the Rulings name; nil when any of them
// covers everything, which is active-passive.
func rulingAddresses(rulings []*rpc.Ruling) []string {
	var addrs []string
	for _, r := range rulings {
		if len(r.GetAddresses()) == 0 {
			return nil
		}
		for _, ip := range r.GetAddresses() {
			addrs = appendUnique(addrs, ip)
		}
	}
	return addrs
}

// losersReleased reports whether a report shows every loser visible and no
// longer claiming what it was ruled out of: Active at all in active-passive,
// any of the addresses in active-active. A loser the Witness cannot see has not
// been seen to release anything; that waits for the age-out.
func losersReleased(views []*rpc.PeerView, losers, addresses []string) bool {
	for _, id := range losers {
		released := false
		for _, v := range views {
			if v.GetNodeId() != id || !v.GetVisible() {
				continue
			}
			if len(addresses) == 0 {
				released = statusOf(v.GetClaim()) != membership.StatusActive
				continue
			}
			released = true
			for _, ip := range v.GetClaim().GetActiveIps() {
				if contains(addresses, ip) {
					released = false
				}
			}
		}
		if !released {
			return false
		}
	}
	return true
}

// markReannounced reports whether r is a Ruling this node has not yet taken up
// as keeper, and records that it now has. Entries older than the hold-down are
// forgotten, matching how long the Witness goes on sending a settled Ruling.
func (s *Server) markReannounced(now time.Time, r *rpc.Ruling) bool {
	key := r.GetKeeper() + "\x01" + joinIDs(r.GetContenders()) + "\x01" + joinIDs(r.GetAddresses())
	window := s.witnessHoldDown()
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	for k, at := range s.witnessFence.reannounced {
		if now.Sub(at) >= window {
			delete(s.witnessFence.reannounced, k)
		}
	}
	if _, done := s.witnessFence.reannounced[key]; done {
		return false
	}
	if s.witnessFence.reannounced == nil {
		s.witnessFence.reannounced = make(map[string]time.Time)
	}
	s.witnessFence.reannounced[key] = now
	return true
}

func (s *Server) noteContested(now time.Time, contenders, addresses []string, localID string) {
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	c := s.witnessFence.contested
	if c == nil {
		// The first Ruling's time is what the age-out counts from.
		c = &contested{since: now}
		s.witnessFence.contested = c
	}
	for _, id := range contenders {
		if id != localID {
			c.losers = appendUnique(c.losers, id)
		}
	}
	for _, ip := range addresses {
		c.addresses = appendUnique(c.addresses, ip)
	}
}

func (s *Server) contestedSnapshot() *contested {
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	if s.witnessFence.contested == nil {
		return nil
	}
	c := *s.witnessFence.contested
	return &c
}

func (s *Server) takeContestedOlderThan(now time.Time, age time.Duration) *contested {
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	c := s.witnessFence.contested
	if c == nil || now.Sub(c.since) < age {
		return nil
	}
	s.witnessFence.contested = nil
	return c
}

func (s *Server) clearContested() {
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	s.witnessFence.contested = nil
}

func (s *Server) setPeerServing(serving map[string]bool) {
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	s.witnessFence.peerServing = serving
}

// WitnessHeldBack is the set of addresses orphan redistribution must leave
// alone on this node: what the Witness last said a peer this node cannot see
// is serving, and what a Ruling took from this node while it is held down. Never
// blocks -- the health-check tick reads it -- and empty when the Witness is
// silent, which is today's behaviour.
func (s *Server) WitnessHeldBack() map[string]bool {
	now, holdDown := time.Now(), s.witnessHoldDown()
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	held := make(map[string]bool, len(s.witnessFence.peerServing)+len(s.witnessFence.ruledOut))
	for ip := range s.witnessFence.peerServing {
		held[ip] = true
	}
	if s.witnessFence.fenced && now.Sub(s.witnessFence.lastAnswer) < holdDown {
		for _, ip := range s.witnessFence.ruledOut {
			held[ip] = true
		}
	}
	return held
}

// releaseForRuling gives back exactly the addresses a Ruling took from this
// node, and keeps serving the rest: in active-active the node ruled against
// took the other's groups during the partition, and the Ruling returns those,
// not what it held before. Brought down while still claimed, as MakePassive
// does, then dropped from the claim, then verified against the interfaces.
func (s *Server) releaseForRuling(ctx context.Context, localID string, addrs []string) error {
	if s.releaseSelf != nil {
		return s.releaseSelf(ctx, addrs)
	}
	local := s.memberList.GetMemberByID(localID)
	if local == nil {
		return errors.New("local member missing")
	}
	if err := local.BringDownIPs(addrs); err != nil {
		return err
	}
	local.UpdateClaim(func(c membership.Claim) (membership.Claim, bool) {
		kept := c.ActiveIPs[:0:0]
		for _, ip := range c.ActiveIPs {
			if !contains(addrs, ip) {
				kept = append(kept, ip)
			}
		}
		c.ActiveIPs = kept
		return c, true
	})
	remaining, err := stillHeldIPs(addrs)
	if err != nil {
		return fmt.Errorf("cannot verify the release: %w", err)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("%d address(es) still up after the release", len(remaining))
	}
	return nil
}

// demoteForRuling stands this node down through the same local MakePassive a
// peer's demotion uses, which releases every address and verifies the release
// against the interfaces before it reports success.
func (s *Server) demoteForRuling(ctx context.Context, localID string) error {
	if s.demoteSelf != nil {
		return s.demoteSelf(ctx)
	}
	resp, err := s.MakePassive(ctx, &rpc.MakePassiveRequest{NodeId: localID})
	if err != nil {
		return err
	}
	if !resp.GetSuccess() {
		return errors.New(resp.GetMessage())
	}
	return nil
}

func (s *Server) reannounce(localID string) {
	announce := s.AnnounceNodeIPs
	if s.announceSelf != nil {
		announce = s.announceSelf
	}
	if err := announce(localID); err != nil {
		s.logger.Warn("WITNESS: re-announcement failed", "error", err)
	}
}
