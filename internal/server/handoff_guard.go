package server

import (
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/pulselock"
)

// handoffGuard marks an explicit Active-to-Active handoff in progress.
//
// A verified transfer demotes the previous Active first, then spends seconds
// acquiring on the target before it broadcasts the new state. For that whole
// window no member looks Active, so the election coordinator — very often the
// node that was just demoted, as the lowest-ID Passive — starts an election,
// every voter grants it, and the coordinator takes the group back (#266).
//
// The guard is held by the node orchestrating the handoff and by the node it
// demoted. Both report it in their HealthCheck replies and refuse to accept an
// election while it lasts. Every accepting voter directly probes every member
// other than the candidate, so whichever node the coordinator proposes, at
// least one guard holder is probed and the acceptance is refused — on a cluster
// of any size, not only where the two holders form a blocking minority.
//
// Bounded: a handoff that dies part-way must not block elections for good, so
// the guard lapses after handoffWindow.
type handoffGuard struct {
	mu pulselock.Mutex
	// target is the node the group is being handed to; empty when idle.
	target string
	until  time.Time
	// orchestrating is set on the node running the handoff, which clears the
	// guard itself when it finishes. The demoted node cannot know how the
	// handoff ended, so it clears its guard once it sees another member Active.
	// Not itself: it is still Active while it records the guard, and if an
	// aborted handoff reinstates it, its own Active reply already stops votes.
	orchestrating bool
	// gen tells a finishing handoff apart from a newer one that replaced it.
	gen uint64
}

// handoffWindow bounds a handoff: the demotion, the source release and the
// destination acquisition are each sized to the group by DemotionTimeoutFor.
func (s *Server) handoffWindow() time.Duration {
	return 3 * s.demotionTimeout()
}

// beginHandoff marks this node as orchestrating a handoff to target. The
// returned function ends it, unless a newer handoff has since replaced it.
func (s *Server) beginHandoff(target string) func() {
	window := s.handoffWindow()
	g := &s.handoff
	g.mu.Lock()
	g.gen++
	gen := g.gen
	g.target, g.until, g.orchestrating = target, time.Now().Add(window), true
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		if g.gen == gen {
			g.target, g.until, g.orchestrating = "", time.Time{}, false
		}
		g.mu.Unlock()
	}
}

// noteDemotedForHandoff records that this node has just released the group as
// the first step of a handoff to target.
func (s *Server) noteDemotedForHandoff(target string) {
	window := s.handoffWindow()
	g := &s.handoff
	g.mu.Lock()
	g.gen++
	g.target, g.until, g.orchestrating = target, time.Now().Add(window), false
	g.mu.Unlock()
}

// handoffInProgress returns the target of a live handoff this node is part of,
// or "" when there is none.
func (s *Server) handoffInProgress() string {
	// Read membership before taking the guard's lock; nothing below nests.
	anyActive := false
	if s.memberList != nil {
		for _, m := range s.memberList.MembersSnapshot() {
			if !m.IsLocal() && m.GetStatus() == membership.StatusActive {
				anyActive = true
				break
			}
		}
	}
	g := &s.handoff
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.target == "" {
		return ""
	}
	if !time.Now().Before(g.until) || (!g.orchestrating && anyActive) {
		g.target, g.until, g.orchestrating = "", time.Time{}, false
		return ""
	}
	return g.target
}
