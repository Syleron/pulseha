package server

import (
	"slices"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
)

// Regression for docs/TEST-PLAN.md defect #30. The one-shot VIP reconcile that
// loadInitialMembers spawns brought up every IP of every group mapped to the
// interface whenever the local node read Active, with no active-active
// filtering — and loadInitialMembers runs on every full ConfigSync, not just at
// startup. On whitecrane that meant each Active peer re-claiming all 201
// RealTest addresses 500ms after every sync, leaving releaseUnassignedIPs to
// undo it on the next enforce tick.
//
// The release direction is asserted here too, because it must NOT be narrowed
// the same way: a just-demoted node may hold addresses it was never assigned.
func TestReconcileVIPPlan(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"}

	cases := []struct {
		name      string
		mode      string
		status    membership.MemberStatus
		assigned  []string
		wantClaim bool
		wantIPs   []string
	}{
		{
			name:      "active-passive Active claims the whole group",
			mode:      "active-passive",
			status:    membership.StatusActive,
			wantClaim: true,
			wantIPs:   group,
		},
		{
			name:      "active-active Active claims only its assigned share",
			mode:      "active-active",
			status:    membership.StatusActive,
			assigned:  []string{"10.0.0.2/24", "10.0.0.3/24"},
			wantClaim: true,
			wantIPs:   []string{"10.0.0.2/24", "10.0.0.3/24"},
		},
		{
			// Awaiting a first assignment must claim nothing, rather than
			// falling back to the whole group.
			name:      "active-active Active with no assignments claims nothing",
			mode:      "active-active",
			status:    membership.StatusActive,
			wantClaim: true,
			wantIPs:   nil,
		},
		{
			name:      "Passive releases the whole group even in active-active",
			mode:      "active-active",
			status:    membership.StatusPassive,
			assigned:  []string{"10.0.0.2/24"},
			wantClaim: false,
			wantIPs:   group,
		},
		{
			name:      "Unknown releases the whole group",
			mode:      "active-passive",
			status:    membership.StatusUnknown,
			wantClaim: false,
			wantIPs:   group,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newExpectedIPsServer(tc.mode, group, tc.assigned)
			s.memberList.GetMemberByID("node-a").Status = tc.status

			groupIPs, activeActive := s.snapshotVIPGroups("node-a")
			plan, claim := s.reconcileVIPPlan("node-a", groupIPs, activeActive)
			if claim != tc.wantClaim {
				t.Errorf("claim = %v, want %v", claim, tc.wantClaim)
			}
			if !slices.Equal(plan["eth0"], tc.wantIPs) {
				t.Errorf("plan[eth0] = %v, want %v", plan["eth0"], tc.wantIPs)
			}
			// An empty set must not leave a stray interface entry for the
			// caller to issue an empty RPC against.
			wantLen := 1
			if len(tc.wantIPs) == 0 {
				wantLen = 0
			}
			if len(plan) != wantLen {
				t.Errorf("plan has %d interfaces, want %d: %v", len(plan), wantLen, plan)
			}
		})
	}
}

// A node absent from the member list or the config must produce no plan at all,
// and must not default to claiming.
func TestReconcileVIPPlanUnknownNode(t *testing.T) {
	s := newExpectedIPsServer("active-passive", []string{"10.0.0.1/24"}, nil)

	groupIPs, activeActive := s.snapshotVIPGroups("node-missing")
	if groupIPs != nil || activeActive {
		t.Errorf("snapshotVIPGroups(unknown) = %v, %v; want nil, false", groupIPs, activeActive)
	}

	plan, claim := s.reconcileVIPPlan("node-missing", groupIPs, activeActive)
	if plan != nil || claim {
		t.Errorf("reconcileVIPPlan(unknown) = %v, %v; want nil, false", plan, claim)
	}
}

// The snapshot must be taken before the reconcile goroutine sleeps: ConfigSync
// spawns Reconfigure() -> config.Reload(), which unmarshals over the live
// *Config, so any config read after the sleep races that rewrite. This pins the
// snapshot as self-contained — nothing it returns is a view into s.config that
// a later Reload could swap underneath the goroutine.
func TestSnapshotVIPGroupsDoesNotAliasConfig(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	s := newExpectedIPsServer("active-active", group, nil)

	groupIPs, activeActive := s.snapshotVIPGroups("node-a")
	if !activeActive {
		t.Fatal("activeActive = false, want true")
	}

	// Stand in for Reload replacing the config wholesale.
	s.config.Groups = map[string][]string{"group1": {"192.168.9.9/24"}}
	s.config.Pulse.Mode = "active-passive"

	if !slices.Equal(groupIPs["eth0"], group) {
		t.Errorf("snapshot changed after config replacement: %v, want %v", groupIPs["eth0"], group)
	}
}
