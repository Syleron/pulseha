package server

import (
	"context"
	"slices"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

func activeClaim(id string, ips ...string) *rpc.MemberClaim {
	return &rpc.MemberClaim{NodeId: id, Status: rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE, ActiveIps: ips}
}

func passiveClaim(id string) *rpc.MemberClaim {
	return &rpc.MemberClaim{NodeId: id, Status: rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE}
}

func TestRulingsInActivePassive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claims  []*rpc.MemberClaim
		elected string
		keeper  string // "" means no ruling
	}{
		{"one Active is no contest", []*rpc.MemberClaim{activeClaim("a"), passiveClaim("b")}, "a", ""},
		{"two Actives: the Elected Node keeps", []*rpc.MemberClaim{activeClaim("a"), activeClaim("b")}, "b", "b"},
		// The Q2 counter-example: the node that promoted into the partition
		// has the higher epoch. The history, not the epoch, decides.
		{"the higher epoch does not win", []*rpc.MemberClaim{
			{NodeId: "a", Status: rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE, ClusterEpoch: 7},
			{NodeId: "b", Status: rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE, ClusterEpoch: 9},
		}, "a", "a"},
		{"no history: the lowest ID keeps", []*rpc.MemberClaim{activeClaim("b"), activeClaim("a")}, "", "a"},
		{"history names neither contender: the lowest ID keeps", []*rpc.MemberClaim{activeClaim("b"), activeClaim("a")}, "gone", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := computeRulings("active-passive", tc.claims, tc.elected, nil)
			if tc.keeper == "" {
				if len(got) != 0 {
					t.Fatalf("rulings = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Keeper != tc.keeper || len(got[0].Addresses) != 0 ||
				!slices.Equal(got[0].Contenders, []string{"a", "b"}) {
				t.Fatalf("rulings = %v, want %s keeping every address against a and b", got, tc.keeper)
			}
		})
	}
}

// Two Actives is the healthy state in active-active. Only addresses in two
// Claims are ruled on, so a node ruled against gives back what it took and keeps
// what it held before.
func TestRulingsInActiveActive(t *testing.T) {
	claims := []*rpc.MemberClaim{
		activeClaim("a", "10.0.0.1", "10.0.0.2", "10.0.0.3"),
		activeClaim("b", "10.0.0.3", "10.0.0.4", "10.0.0.1"),
	}
	holders := map[string]string{"10.0.0.1": "a", "10.0.0.3": "b"}
	got := computeRulings("active-active", claims, "", holders)
	if len(got) != 2 {
		t.Fatalf("rulings = %v, want one per distinct keeper", got)
	}
	if got[0].Keeper != "a" || !slices.Equal(got[0].Addresses, []string{"10.0.0.1"}) {
		t.Errorf("first ruling = %v, want a keeping 10.0.0.1", got[0])
	}
	if got[1].Keeper != "b" || !slices.Equal(got[1].Addresses, []string{"10.0.0.3"}) {
		t.Errorf("second ruling = %v, want b keeping 10.0.0.3", got[1])
	}

	if got := computeRulings("active-active", []*rpc.MemberClaim{
		activeClaim("a", "10.0.0.1"), activeClaim("b", "10.0.0.2"),
	}, "", nil); len(got) != 0 {
		t.Errorf("disjoint claims ruled on: %v", got)
	}
}

// The history only learns from a round with no overlap. A contest must not get
// to decide itself by being recorded first.
func TestHistoryIgnoresARoundWithAnOverlap(t *testing.T) {
	var h witnessHistory
	h.record("active-passive", []*rpc.MemberClaim{activeClaim("a"), passiveClaim("b")})
	h.record("active-passive", []*rpc.MemberClaim{activeClaim("a"), activeClaim("b")})
	h.record("active-passive", []*rpc.MemberClaim{passiveClaim("a")}) // no Active: nothing learned
	if elected, _ := h.snapshot(); elected != "a" {
		t.Errorf("elected = %q, want a, recorded before the contest", elected)
	}

	h.record("active-active", []*rpc.MemberClaim{activeClaim("a", "10.0.0.1"), activeClaim("b", "10.0.0.2")})
	h.record("active-active", []*rpc.MemberClaim{activeClaim("a", "10.0.0.1"), activeClaim("b", "10.0.0.1")})
	if _, holders := h.snapshot(); holders["10.0.0.1"] != "a" || holders["10.0.0.2"] != "b" {
		t.Errorf("holders = %v, want the uncontested round's", holders)
	}
}

// End to end through the report: the Witness weighs the asker's carried Claim
// against the peer it asked, and rules for the node it last saw serving.
func TestTheReportCarriesTheRuling(t *testing.T) {
	a, b, w := newWitnessTrio(t, "")
	w.config.Pulse.Mode = "active-passive"
	b.memberList.GetMemberByID("b").SetStatus(membership.StatusActive)
	w.witnessHistory.record("active-passive", []*rpc.MemberClaim{passiveClaim("a"), activeClaim("b")})
	a.memberList.GetMemberByID("a").SetStatus(membership.StatusActive)

	resp, err := a.askWitness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := resp.GetRulings()
	if len(r) != 1 || r[0].Keeper != "b" || !slices.Equal(r[0].Contenders, []string{"a", "b"}) {
		t.Fatalf("rulings = %v, want b keeping against a", r)
	}

	// A peer the Witness cannot see is not a contender, whatever it held.
	b.memberList.GetMemberByID("b").SetStatus(membership.StatusPassive)
	if resp, _ := a.askWitness(context.Background()); len(resp.GetRulings()) != 0 {
		t.Errorf("rulings with only one Active = %v, want none", resp.GetRulings())
	}
}

// The Witness's tick asks the peers itself and records what it sees, so its
// history comes from its own observations and not from anyone's broadcast.
func TestTheWitnessTickRecordsWhatItSees(t *testing.T) {
	_, b, w := newWitnessTrio(t, "")
	w.config.Pulse.Mode = "active-passive"
	b.memberList.GetMemberByID("b").SetStatus(membership.StatusActive)
	w.witnessLoopTick()
	if elected, _ := w.witnessHistory.snapshot(); elected != "b" {
		t.Errorf("elected after a tick = %q, want b", elected)
	}

	// A node that is not the Witness records nothing.
	b.witnessLoopTick()
	if elected, _ := b.witnessHistory.snapshot(); elected != "" {
		t.Errorf("a node that is not the Witness recorded %q", elected)
	}
}
