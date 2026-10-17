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
