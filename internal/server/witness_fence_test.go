package server

import (
	"context"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// splitPair is the cluster-link-only partition after both nodes promoted: a and
// b both Active, neither seeing the other, the Witness seeing both and with b
// as the last Elected Node it saw. Demotions and announcements are recorded
// rather than performed.
type splitPair struct {
	a, b, w            *Server
	demoted, announced map[string]int
}

func newSplitPair(t *testing.T) *splitPair {
	t.Helper()
	p := &splitPair{demoted: map[string]int{}, announced: map[string]int{}}
	p.a, p.b, p.w = newWitnessTrio(t, "")
	for id, s := range map[string]*Server{"a": p.a, "b": p.b} {
		id, s := id, s
		s.config.Pulse.Mode = "active-passive"
		s.memberList.GetMemberByID(id).SetStatus(membership.StatusActive)
		s.demoteSelf = func(context.Context) error {
			p.demoted[id]++
			s.memberList.GetMemberByID(id).SetClaim(membership.Claim{Status: membership.StatusPassive})
			return nil
		}
		s.announceSelf = func(string) error { p.announced[id]++; return nil }
	}
	p.a.memberList.GetMemberByID("b").SetStatus(membership.StatusUnknown)
	p.b.memberList.GetMemberByID("a").SetStatus(membership.StatusUnknown)
	p.w.config.Pulse.Mode = "active-passive"
	p.w.witnessHistory.record("active-passive", []*rpc.MemberClaim{passiveClaim("a"), activeClaim("b")})
	return p
}

// The whole cycle: the keeper notes the contest, the loser stands down, and the
// keeper re-announces only once it sees the release.
func TestARulingEndsTheSplit(t *testing.T) {
	p := newSplitPair(t)

	p.b.witnessLoopTick()
	if p.demoted["b"] != 0 || p.announced["b"] != 0 {
		t.Fatalf("keeper acted before the loser released: demoted %d, announced %d", p.demoted["b"], p.announced["b"])
	}

	p.a.witnessLoopTick()
	if p.demoted["a"] != 1 || !p.a.witnessFence.isFenced() {
		t.Fatalf("loser: demoted %d, Fenced %v; want stood down and Fenced", p.demoted["a"], p.a.witnessFence.isFenced())
	}

	p.b.witnessLoopTick()
	if p.announced["b"] != 1 {
		t.Fatalf("keeper re-announced %d times after the release, want 1", p.announced["b"])
	}
	p.b.witnessLoopTick()
	if p.announced["b"] != 1 || p.demoted["b"] != 0 {
		t.Errorf("keeper after the split ended: announced %d, demoted %d; want once and never", p.announced["b"], p.demoted["b"])
	}
}

// Silence never fences. A node that cannot reach the Witness keeps serving --
// tier 3, ADR-0002 unchanged.
func TestWitnessSilenceNeverFences(t *testing.T) {
	p := newSplitPair(t)
	silenceWitness(t, p.a)
	p.a.witnessLoopTick()
	if p.demoted["a"] != 0 || p.a.witnessFence.isFenced() {
		t.Errorf("a silent Witness fenced a serving node")
	}
}

// With the peer visible nobody asks the Witness, whatever it would rule.
func TestNoOneAsksWhileThePeerIsVisible(t *testing.T) {
	p := newSplitPair(t)
	p.a.memberList.GetMemberByID("b").SetStatus(membership.StatusActive)
	p.a.witnessLoopTick()
	if p.demoted["a"] != 0 {
		t.Errorf("a node that can see its peer stood down on a Ruling")
	}
}

// The Witness disappears between the Ruling and the release. The keeper still
// re-announces, on the age-out, so the address cannot be left pointed at the
// node that let it go.
func TestTheReannouncementAgesOut(t *testing.T) {
	p := newSplitPair(t)
	p.b.witnessLoopTick() // ruled for: contested
	silenceWitness(t, p.b)

	p.b.witnessLoopTick()
	if p.announced["b"] != 0 {
		t.Fatal("re-announced before the age-out")
	}
	p.b.witnessFence.mu.Lock()
	p.b.witnessFence.contested.since = time.Now().Add(-2 * p.b.witnessHoldDown())
	p.b.witnessFence.mu.Unlock()
	p.b.witnessLoopTick()
	if p.announced["b"] != 1 {
		t.Errorf("announced %d times after the age-out, want 1", p.announced["b"])
	}
}

// silenceWitness points s at a Witness address nothing listens on, and drops the
// connection s already has, which witnessPathClient would otherwise go on reusing.
func silenceWitness(t *testing.T, s *Server) {
	t.Helper()
	s.config.Nodes["c"].WitnessAddress = freeAddr(t)
	s.witnessClientsMu.Lock()
	for k, c := range s.witnessClients {
		c.Close()
		delete(s.witnessClients, k)
	}
	s.witnessClientsMu.Unlock()
}
