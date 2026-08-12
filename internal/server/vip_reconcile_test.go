package server

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// Regression for docs/TEST-PLAN.md defect #65, the frequency half.
//
// loadInitialMembers runs on every full ConfigSync, and it spawned this pass
// unconditionally — so on whitecrane a 40-address `group add-ip` burst, each add
// broadcasting the config, put 40 concurrent whole-share bring-ups on every peer
// that received it, each announcing through its own SendGARPBatch. garpFanout
// caps one batch at 32 and bounds nothing across batches, which is how run 33
// measured 255/268/258 concurrent arping against an enforce pass that ran 2-3
// batches.
//
// Three separate things are asserted because three different wrong versions pass
// the other two:
//   - how many pass goroutines the burst starts, which is the defect itself;
//   - that the burst still produces a follow-up pass, which drop-if-running —
//     the obvious guard to copy from startReconcilePassLocked — would not;
//   - that the follow-up carries the *newest* snapshot, which a first-wins queue
//     would get wrong while leaving the counts looking right.
//
// Counting entries to the window is what distinguishes a herd from one pass.
// Counting concurrent runs cannot: which snapshot a herd of goroutines happens to
// pick up, and how many of them find one at all, is a scheduling accident, so a
// herd can look identical to a single pass on any given run.
func TestVIPReconcileCoalescesABurstOfSchedules(t *testing.T) {
	window := make(chan struct{})
	started := make(chan vipReconcileSnapshot)
	release := make(chan struct{})

	var mu sync.Mutex
	var ran []string
	windows := 0
	windowCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return windows
	}

	r := newVIPReconciler(time.Hour, func(snapshot vipReconcileSnapshot) {
		mu.Lock()
		ran = append(ran, snapshot.localID)
		mu.Unlock()
		started <- snapshot
		<-release
	})
	// Both hooks are rendezvous rather than delays: the test decides when the
	// window closes and when a pass finishes, so nothing here depends on timing.
	r.sleep = func(time.Duration) {
		mu.Lock()
		windows++
		mu.Unlock()
		<-window
	}

	r.Schedule(vipReconcileSnapshot{localID: "sync-1"})
	window <- struct{}{}
	if first := awaitPass(t, started); first.localID != "sync-1" {
		t.Fatalf("first pass ran %q, want sync-1", first.localID)
	}

	// Nine more syncs land while that pass is still running.
	for i := 2; i <= 10; i++ {
		r.Schedule(vipReconcileSnapshot{localID: fmt.Sprintf("sync-%d", i)})
	}

	// A goroutine per schedule reaches the window as soon as it is spawned, so
	// give any that exist the time to do it before concluding none do.
	time.Sleep(200 * time.Millisecond)
	if got := windowCount(); got != 1 {
		t.Errorf("the burst opened %d windows, want 1: a pass goroutine per schedule", got)
	}

	release <- struct{}{}

	// The nine collapse into exactly one follow-up, and it acts on the newest
	// config rather than the oldest one still queued.
	window <- struct{}{}
	if second := awaitPass(t, started); second.localID != "sync-10" {
		t.Errorf("second pass ran %q, want sync-10 (the newest snapshot)", second.localID)
	}
	release <- struct{}{}

	// Nothing further is pending, so the next window finds an empty queue and the
	// pass goroutine retires instead of spinning.
	window <- struct{}{}
	waitForVIPReconcilerIdle(t, r)

	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 {
		t.Errorf("10 schedules ran %d passes (%v), want 2", len(ran), ran)
	}
	if windows != 3 {
		t.Errorf("the whole run opened %d windows, want 3 (one per loop iteration)", windows)
	}
}

// A schedule arriving after the queue drained has to start a fresh pass: the
// retired goroutine is not coming back for it.
func TestVIPReconcileSchedulesAgainAfterDraining(t *testing.T) {
	window := make(chan struct{})
	started := make(chan vipReconcileSnapshot)

	r := newVIPReconciler(time.Hour, func(snapshot vipReconcileSnapshot) { started <- snapshot })
	r.sleep = func(time.Duration) { <-window }

	r.Schedule(vipReconcileSnapshot{localID: "sync-1"})
	window <- struct{}{}
	awaitPass(t, started)
	window <- struct{}{}
	waitForVIPReconcilerIdle(t, r)

	r.Schedule(vipReconcileSnapshot{localID: "sync-2"})
	window <- struct{}{}
	if snapshot := awaitPass(t, started); snapshot.localID != "sync-2" {
		t.Errorf("ran %q, want sync-2", snapshot.localID)
	}
	window <- struct{}{}
	waitForVIPReconcilerIdle(t, r)
}

// awaitPass fails the test rather than deadlocking it when a pass that should
// have run does not — a queue that drops the follow-up is a plausible enough
// mistake to be worth failing fast on.
func awaitPass(t *testing.T, started <-chan vipReconcileSnapshot) vipReconcileSnapshot {
	t.Helper()
	select {
	case snapshot := <-started:
		return snapshot
	case <-time.After(5 * time.Second):
		t.Fatal("a pass that should have run never started")
		return vipReconcileSnapshot{}
	}
}

func waitForVIPReconcilerIdle(t *testing.T, r *vipReconciler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		idle := !r.running && r.pending == nil
		r.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("reconciler never went idle")
}

// Regression for docs/TEST-PLAN.md defect #65, the set-size half — the larger of
// the two.
//
// The claim plan is everything the node should hold, so on a converged node it is
// its whole assignment, and every address of it was handed to BringUpIP. That
// handler announces every address it is *asked* about rather than the ones it
// placed, on purpose (#33's residual half), which makes narrowing the caller's
// job — and every sibling caller already narrows. An add of one address therefore
// re-announced the other 71.
func TestVIPReconcileTargetsNarrowsAClaimToMissingAddresses(t *testing.T) {
	held := map[string]string{
		"10.0.0.1": "eth0",
		"10.0.0.2": "eth0",
		"10.0.0.9": "eth0", // held, but not on the interface it is planned for
		"10.0.1.1": "eth1",
	}
	heldOn := func(ip string) (bool, string) {
		ipOnly, _, _ := strings.Cut(ip, "/")
		iface, ok := held[ipOnly]
		return ok, iface
	}

	plan := map[string][]string{
		// Nothing missing: this interface must drop out entirely, so a converged
		// node issues no bring-up and announces nothing.
		"eth0": {"10.0.0.1/24", "10.0.0.2/24"},
		// One genuinely absent, one held on the wrong interface (which has to be
		// moved, so it counts as missing), one already correct.
		"eth1": {"10.0.1.1/24", "10.0.1.2/24", "10.0.0.9/24"},
	}

	targets, _ := vipReconcileTargets(plan, true, heldOn)

	if _, ok := targets["eth0"]; ok {
		t.Errorf("eth0 stayed in the plan with nothing missing: %v", targets["eth0"])
	}
	want := []string{"10.0.1.2/24", "10.0.0.9/24"}
	if !slices.Equal(targets["eth1"], want) {
		t.Errorf("targets[eth1] = %v, want %v", targets["eth1"], want)
	}
	if len(targets) != 1 {
		t.Errorf("targets has %d interfaces, want 1: %v", len(targets), targets)
	}
}

// The release direction must NOT be narrowed the same way, and this is the
// assertion that stops the fix above being applied to both: a node that has just
// been demoted may be holding addresses it was never assigned, and the point of
// that direction is to leave it holding none. Narrowing it to "what the plan says
// is missing" would skip exactly the addresses it exists to strip.
func TestVIPReconcileTargetsLeavesAReleaseWhole(t *testing.T) {
	heldOn := func(ip string) (bool, string) { return true, "eth0" }
	plan := map[string][]string{"eth0": {"10.0.0.1/24", "10.0.0.2/24"}}

	targets, _ := vipReconcileTargets(plan, false, heldOn)

	if !slices.Equal(targets["eth0"], plan["eth0"]) {
		t.Errorf("targets[eth0] = %v, want the whole plan %v", targets["eth0"], plan["eth0"])
	}
}

// A lookup that could not be built means kernel state is unreadable, and every
// address is then treated as missing. Narrowing must never turn a placement into
// a silent no-op on the strength of a check that did not run — the same
// direction missingOnIface, placeRequestedIPs and releaseRequestedIPs all take.
func TestVIPReconcileTargetsWithoutKernelStateClaimsEverything(t *testing.T) {
	plan := map[string][]string{"eth0": {"10.0.0.1/24", "10.0.0.2/24"}}

	targets, _ := vipReconcileTargets(plan, true, nil)

	if !slices.Equal(targets["eth0"], plan["eth0"]) {
		t.Errorf("targets[eth0] = %v, want the whole plan %v", targets["eth0"], plan["eth0"])
	}
}
