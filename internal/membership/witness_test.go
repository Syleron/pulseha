package membership

import (
	"io"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

func newWitnessMember(id string, status MemberStatus) *Member {
	m := newAATestMember(id, id, status, nil)
	m.witness = true
	return m
}

// Whatever anyone writes, a Witness is Witness or Unknown and holds nothing.
// This is "a peer cannot override it in either direction", held at the one place
// status is written (ADR-0006).
func TestAWitnessIsOnlyEverWitnessOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*Member)
		want  MemberStatus
	}{
		{"SetStatus Active", func(m *Member) { m.SetStatus(StatusActive) }, StatusWitness},
		{"SetStatus Passive", func(m *Member) { m.SetStatus(StatusPassive) }, StatusWitness},
		{"SetStatus Unknown", func(m *Member) { m.SetStatus(StatusUnknown) }, StatusUnknown},
		{"SetClaim Active with addresses", func(m *Member) {
			m.SetClaim(Claim{Status: StatusActive, ActiveIPs: []string{"10.0.0.1/24"}})
		}, StatusWitness},
		{"UpdateClaim to Active", func(m *Member) {
			m.UpdateClaim(func(c Claim) (Claim, bool) {
				c.Status = StatusActive
				return c.WithAddresses("10.0.0.1/24"), true
			})
		}, StatusWitness},
		{"EnterMaintenance", func(m *Member) { _ = m.EnterMaintenance() }, StatusWitness},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newWitnessMember("w", StatusWitness)
			tc.write(m)
			if got := m.Claim(); got.Status != tc.want || len(got.ActiveIPs) != 0 {
				t.Errorf("claim after %s = %+v, want status %s holding nothing", tc.name, got, StatusToString(tc.want))
			}
		})
	}
}

func TestAWitnessNeverBringsAnAddressUp(t *testing.T) {
	m := newWitnessMember("w", StatusWitness)
	if err := m.BringUpIPs([]string{"10.0.0.1/24"}); err == nil {
		t.Fatal("BringUpIPs on the Witness succeeded")
	}
}

// The role comes from config, and a Witness is never parked in Maintenance on
// join -- there is nothing to verify before it becomes eligible, because it never
// does.
func TestTheRoleComesFromConfig(t *testing.T) {
	cfg := &config.Config{Nodes: map[string]*config.Node{
		"w": {Hostname: "w", Witness: true},
		"a": {Hostname: "a"},
	}}
	ml := NewMemberList(cfg, log.New(io.Discard))
	for _, id := range []string{"w", "a"} {
		if err := ml.AddMember(id, id, "127.0.0.1", "1"); err != nil {
			t.Fatal(err)
		}
	}
	if w := ml.GetMemberByID("w"); !w.IsWitness() || w.GetStatus() != StatusUnknown {
		t.Errorf("joined Witness: role %v, status %s; want the role, starting Unknown",
			w.IsWitness(), StatusToString(w.GetStatus()))
	}
	if a := ml.GetMemberByID("a"); a.IsWitness() || a.GetStatus() != StatusMaintenance {
		t.Errorf("joined node: role %v, status %s; want no role, parked in Maintenance",
			a.IsWitness(), StatusToString(a.GetStatus()))
	}

	// A member that existed before the config naming it arrived picks the role up.
	late := NewMemberList(&config.Config{Nodes: map[string]*config.Node{"w": {Hostname: "w"}}}, log.New(io.Discard))
	if err := late.AddMember("w", "w", "127.0.0.1", "1"); err != nil {
		t.Fatal(err)
	}
	late.GetMemberByID("w").SetClaim(Claim{Status: StatusActive, ActiveIPs: []string{"10.0.0.1/24"}})
	late.UpdateConfig(&config.Config{Nodes: map[string]*config.Node{"w": {Hostname: "w", Witness: true}}})
	if got := late.GetMemberByID("w").Claim(); got.Status != StatusWitness || len(got.ActiveIPs) != 0 {
		t.Errorf("member after its config named it the Witness = %+v, want Witness holding nothing", got)
	}
}

// Every decision path reads the candidates snapshot. Each case below is built
// so that the Witness would win if it could be seen -- lowest ID, freshest
// response -- and is asserted with the Witness down as well as up, because down
// reads StatusUnknown, which these paths otherwise accept.
func TestDecisionPathsNeverSeeTheWitness(t *testing.T) {
	for _, witnessStatus := range []MemberStatus{StatusWitness, StatusUnknown} {
		t.Run(StatusToString(witnessStatus), func(t *testing.T) {
			newCluster := func() (*HealthChecker, *Member) {
				// "0w" sorts before every other ID, so any lowest-ID choice that
				// could see it would pick it.
				w := newWitnessMember("0w", witnessStatus)
				w.LastHCResponse = time.Now()
				w.Latency = "0.10ms"
				a := newAATestMember("a", "a", StatusActive, nil)
				b := newAATestMember("b", "b", StatusUnknown, nil)
				h, _ := newAPTestChecker("a", a, b, w)
				h.members.Config().Pulse.FailOverLimit = 60000
				h.members.Config().Nodes["0w"].Witness = true
				return h, w
			}

			h, w := newCluster()
			if _, ok := h.members.CandidatesSnapshot()[w.ID]; ok {
				t.Error("CandidatesSnapshot contains the Witness")
			}
			if _, ok := h.members.MembersSnapshot()[w.ID]; !ok {
				t.Error("MembersSnapshot lost the Witness; the health checker has to keep watching it")
			}
			if got := h.selectBestCandidate(); got == w {
				t.Error("selectBestCandidate chose the Witness")
			}
			if got := h.findElectionCoordinator(); got == w.ID {
				t.Error("findElectionCoordinator chose the Witness")
			}
			if got := h.Coordinator(); got == w.ID {
				t.Error("Coordinator chose the Witness")
			}
			h.members.RLock()
			available := h.members.getAvailableNodes()
			h.members.RUnlock()
			for _, m := range available {
				if m == w {
					t.Error("getAvailableNodes offered the Witness for placement")
				}
			}
			if got := ConsolidationTarget(h.members.CandidatesSnapshot(), w.ID); got == w {
				t.Error("ConsolidationTarget chose the Witness even when named as leader")
			}
		})
	}
}

// An address is orphaned when no node this one can see holds it. What the
// Witness says a peer is still serving is only cut off from here, and taking it
// would make an overlap for the Witness to rule on (ADR-0006).
func TestRedistributionLeavesWhatTheWitnessSaysIsServed(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}})
	cfg.Pulse.FailOverLimit = 10000
	cut := newAATestMember("node-a", "host-a", StatusUnknown, []string{"10.0.0.1/24", "10.0.0.2/24"})
	cut.LastHCResponse = time.Now().Add(-30 * time.Second)
	here := newAATestMember("node-b", "host-b", StatusActive, nil)
	ml := newAATestMemberList(cfg, cut, here)
	cfg.Nodes["node-a"] = &config.Node{Hostname: "host-a"}
	cfg.Nodes["node-b"] = &config.Node{Hostname: "host-b", IPGroups: map[string][]string{"eth0": {"group1"}}}
	h := NewHealthChecker(ml, log.New(io.Discard))
	h.server = &stubServer{members: ml, heldBack: map[string]bool{"10.0.0.1/24": true}}

	h.redistributeOrphanedIPs(ml.Members)
	got := here.Claim().ActiveIPs
	if len(got) != 1 || got[0] != "10.0.0.2/24" {
		t.Errorf("redistributed %v; want only 10.0.0.2/24, the address nobody is serving", got)
	}
}
