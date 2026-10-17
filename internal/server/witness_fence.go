package server

import (
	"context"
	"errors"
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
}

func (f *witnessFence) answered(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAnswer = at
}

func (f *witnessFence) fence(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced = true
	f.lastAnswer = at
}

func (f *witnessFence) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced = false
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

// rulingAgainst returns the first Ruling in which localID is a contender and
// not the keeper, or nil.
func rulingAgainst(rulings []*rpc.Ruling, localID string) *rpc.Ruling {
	for _, r := range rulings {
		if r.GetKeeper() != localID && contains(r.GetContenders(), localID) {
			return r
		}
	}
	return nil
}

// contested is, on the node a Ruling kept, what it is waiting for the losers to
// release before it re-announces. Kept beside the fence because the two are the
// same arbitration from opposite sides.
type contested struct {
	since  time.Time
	losers []string
}

// witnessWatch is a serving node's tick while it cannot see a peer: ask the
// Witness, and act on the Ruling.
//
// Ruled against: Fenced -- demote and release. Ruled for: note the losers, and
// re-announce once a later report shows them no longer claiming, because the
// network follows whoever announced last and that is usually the loser, which
// promoted into the partition. Re-announcing on the Ruling itself would race the
// loser's own last announcement; on the release it cannot (ADR-0006).
//
// Witness silence never fences. It only lets a pending re-announcement age out,
// so a Witness that disappears between the Ruling and the release still cannot
// leave the address pointed at a node that has let go of it.
func (s *Server) witnessWatch(ctx context.Context, localID string) {
	now := time.Now()
	resp, err := s.askWitness(ctx)
	if err != nil {
		if c := s.takeContestedOlderThan(now, s.witnessHoldDown()); c != nil {
			s.logger.Warn("WITNESS: no report since the Ruling; re-announcing on the age-out",
				"losers", c.losers, "since", c.since)
			s.reannounce(localID)
		}
		return
	}
	s.witnessFence.answered(now)

	if r := rulingAgainst(resp.GetRulings(), localID); r != nil {
		s.logger.Warn("WITNESS: ruled against; standing down", "keeper", r.GetKeeper(), "contenders", r.GetContenders())
		if err := s.demoteForRuling(ctx, localID); err != nil {
			// Still Active and still contested; the next tick asks again.
			s.logger.Error("WITNESS: could not stand down after a Ruling against this node", "error", err)
			return
		}
		s.witnessFence.fence(now)
		s.clearContested()
		return
	}

	for _, r := range resp.GetRulings() {
		if r.GetKeeper() == localID {
			s.noteContested(now, r.GetContenders(), localID)
		}
	}
	if c := s.contestedSnapshot(); c != nil && losersReleased(resp.GetPeers(), c.losers) {
		s.logger.Info("WITNESS: the node ruled against has released; re-announcing", "losers", c.losers)
		s.clearContested()
		s.reannounce(localID)
	}
}

// losersReleased reports whether a report shows every loser visible and no
// longer claiming Active. A loser the Witness cannot see has not been seen to
// release anything; that waits for the age-out.
func losersReleased(views []*rpc.PeerView, losers []string) bool {
	for _, id := range losers {
		released := false
		for _, v := range views {
			if v.GetNodeId() == id && v.GetVisible() && statusOf(v.GetClaim()) != membership.StatusActive {
				released = true
			}
		}
		if !released {
			return false
		}
	}
	return true
}

func (s *Server) noteContested(now time.Time, contenders []string, localID string) {
	s.witnessFence.mu.Lock()
	defer s.witnessFence.mu.Unlock()
	if s.witnessFence.contested != nil {
		return // keep the first Ruling's time, which is what the age-out counts from
	}
	var losers []string
	for _, id := range contenders {
		if id != localID {
			losers = append(losers, id)
		}
	}
	s.witnessFence.contested = &contested{since: now, losers: losers}
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
