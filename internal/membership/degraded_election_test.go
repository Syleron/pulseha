package membership

import (
	"fmt"
	"testing"

	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
)

// Local observations of two survivors must not turn a configured cluster of
// three or four into a two-node cluster. Even a stale manager count is ignored.
func TestDegradedElectionRequiresExplicitMajority(t *testing.T) {
	for _, tc := range []struct {
		size, available, yes int
		want                 bool
	}{
		{3, 1, 1, false}, {3, 2, 1, false}, {3, 2, 2, true},
		{4, 2, 2, false}, {4, 3, 3, true}, {5, 2, 2, false},
	} {
		t.Run(fmt.Sprintf("%d_members_%d_available_%d_yes", tc.size, tc.available, tc.yes), func(t *testing.T) {
			var members []*Member
			for i := 0; i < tc.size; i++ {
				st := StatusUnknown
				if i < tc.available {
					st = StatusPassive
				}
				members = append(members, newAATestMember(fmt.Sprintf("node-%d", i), "host", st, nil))
			}
			h, stub := newAPTestChecker("node-0", members...)
			stub.quorum = quorum.NewQuorumManager(h.members.Config(), h.logger)
			stub.quorum.UpdateNodeCount(1)
			stub.vote = func(id string) error {
				for i := 0; i < tc.yes; i++ {
					if err := stub.quorum.CastVote(id, fmt.Sprintf("node-%d", i), quorum.VoteDecisionYes); err != nil {
						return err
					}
				}
				return nil
			}
			if got := h.tryAutomaticPromotion(members[0]); got != tc.want {
				t.Fatalf("authorized=%v want %v", got, tc.want)
			}
			if len(stub.promotions) > 0 && stub.promotions[0].ForceDemote {
				t.Fatal("automatic promotion used operator override")
			}
			if !tc.want && len(stub.promotions) != 0 {
				t.Fatal("unauthorized promotion reached RPC")
			}
			if members[0].GetStatus() != StatusPassive {
				t.Fatal("accepted request was treated as completed promotion")
			}
		})
	}
}

func TestAutomaticElectionFailsClosed(t *testing.T) {
	for _, path := range []string{"normal", "emergency"} {
		for _, failure := range []string{"no server", "no quorum manager", "no votes", "RPC refused"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				a := newAATestMember("a", "a", StatusPassive, nil)
				b := newAATestMember("b", "b", StatusUnknown, nil)
				c := newAATestMember("c", "c", StatusUnknown, nil)
				h, stub := newAPTestChecker("a", a, b, c)
				if failure == "no server" {
					h.server = nil
				}
				if failure == "no votes" || failure == "RPC refused" {
					stub.quorum = quorum.NewQuorumManager(h.members.Config(), h.logger)
				}
				if failure == "RPC refused" {
					stub.promoteFails = true
					stub.vote = func(id string) error {
						_ = stub.quorum.CastVote(id, "a", quorum.VoteDecisionYes)
						return stub.quorum.CastVote(id, "b", quorum.VoteDecisionYes)
					}
				}
				if path == "normal" {
					h.electNewActiveNode()
				} else {
					h.emergencyFallback()
				}
				if a.GetStatus() != StatusPassive {
					t.Fatal("failed election directly made candidate Active")
				}
				for _, req := range stub.promotions {
					if req.ForceDemote {
						t.Fatal("automatic request forced promotion")
					}
				}
			})
		}
	}
}

func TestConfiguredPairRetainsAvailabilityPolicy(t *testing.T) {
	for _, local := range []string{"a", "b"} {
		a := newAATestMember("a", "a", StatusUnknown, nil)
		b := newAATestMember("b", "b", StatusUnknown, nil)
		h, stub := newAPTestChecker(local, a, b)
		candidate := h.members.GetMemberByID(local)
		candidate.SetStatus(StatusPassive)
		if !h.tryAutomaticPromotion(candidate) || len(stub.promotions) != 1 || stub.promotions[0].ForceDemote {
			t.Fatal("configured pair must request non-forced promotion")
		}
		// A missing runtime member must not shrink the configured electorate.
		h.members.Config().Nodes["c"] = &config.Node{Hostname: "c"}
		stub.promotions = nil
		if h.tryAutomaticPromotion(candidate) || len(stub.promotions) != 0 {
			t.Fatal("degraded three-node cluster used pair exception")
		}
	}
}

func TestAutomaticPromotionConsumesRecoveredCandidate(t *testing.T) {
	a := newAATestMember("a", "a", StatusPassive, nil)
	b := newAATestMember("b", "b", StatusPassive, nil)
	c := newAATestMember("c", "c", StatusUnknown, nil)
	h, stub := newAPTestChecker("a", a, b, c)
	stub.quorum = quorum.NewQuorumManager(h.members.Config(), h.logger)
	stub.vote = func(id string) error {
		if err := stub.quorum.RecoverProposal(id, "b"); err != nil {
			return err
		}
		_ = stub.quorum.CastVote(id, "a", quorum.VoteDecisionYes)
		return stub.quorum.CastVote(id, "b", quorum.VoteDecisionYes)
	}
	if !h.tryAutomaticPromotion(a) {
		t.Fatal("recovered candidate was not promoted")
	}
	if len(stub.promotions) != 1 || stub.promotions[0].NodeId != "b" || stub.promotions[0].ForceDemote {
		t.Fatalf("did not consume recovered candidate: %+v", stub.promotions)
	}
}

func TestAutomaticPromotionRejectsDecisionInvalidatedDuringVote(t *testing.T) {
	for _, change := range []string{"epoch", "electorate", "active appeared", "candidate maintenance"} {
		t.Run(change, func(t *testing.T) {
			a := newAATestMember("a", "a", StatusPassive, nil)
			b := newAATestMember("b", "b", StatusPassive, nil)
			c := newAATestMember("c", "c", StatusUnknown, nil)
			h, stub := newAPTestChecker("a", a, b, c)
			stub.quorum = quorum.NewQuorumManager(h.members.Config(), h.logger)
			stub.vote = func(id string) error {
				_ = stub.quorum.CastVote(id, "a", quorum.VoteDecisionYes)
				_ = stub.quorum.CastVote(id, "b", quorum.VoteDecisionYes)
				switch change {
				case "epoch":
					stub.epoch++
				case "electorate":
					h.members.Config().Nodes["d"] = &config.Node{Hostname: "d"}
				case "active appeared":
					b.SetStatus(StatusActive)
				case "candidate maintenance":
					a.SetStatus(StatusMaintenance)
				}
				return nil
			}
			if h.tryAutomaticPromotion(a) || len(stub.promotions) != 0 {
				t.Fatal("stale authorization reached promotion")
			}
		})
	}
}

func TestRedistributionConsumesOnlyRecoveredOrphans(t *testing.T) {
	a := newAATestMember("a", "a", StatusActive, nil)
	b := newAATestMember("b", "b", StatusActive, nil)
	c := newAATestMember("c", "c", StatusUnknown, nil)
	h, stub := newAPTestChecker("a", a, b, c)
	stub.quorum = quorum.NewQuorumManager(h.members.Config(), h.logger)
	stub.vote = func(id string) error {
		if err := stub.quorum.RecoverProposal(id, `["10.0.0.1/24"]`); err != nil {
			return err
		}
		_ = stub.quorum.CastVote(id, "a", quorum.VoteDecisionYes)
		return stub.quorum.CastVote(id, "b", quorum.VoteDecisionYes)
	}
	approved, ok := h.approvedRedistribution([]string{"10.0.0.1/24", "10.0.0.2/24"})
	if !ok || len(approved) != 1 || approved[0] != "10.0.0.1/24" {
		t.Fatalf("applied requested instead of recovered addresses: %v %v", approved, ok)
	}
	if _, ok = h.approvedRedistribution([]string{"10.0.0.2/24"}); ok {
		t.Fatal("recovered address was not an orphan")
	}
}
