package membership

import (
	"errors"
	"testing"
)

// Regression for docs/TEST-PLAN.md defect #41. The release pass chose its
// surplus set from an inventory snapshot taken at the top of the enforce tick,
// before the Active branch's bring-up loop, so an address could have moved by
// the time the pass reached it. Releasing an address that is already gone fails
// with `cannot assign requested address` — a no-op reported as an error, ~30 a
// window on the live cluster, which is the noise that would hide a release that
// mattered.
//
// An address that has gone must therefore not be handed to the kernel at all.
func TestReleaseSkipsAnAddressThatHasAlreadyGone(t *testing.T) {
	surplus := map[string][]string{"eth0": {"10.0.0.1/24", "10.0.0.2/24"}}

	// .1 went somewhere else after the surplus set was computed.
	stillHeld := func(iface, ip string) bool { return ip != "10.0.0.1/24" }

	var attempted []string
	bringDown := func(iface, ip string) error {
		attempted = append(attempted, ip)
		return nil
	}

	attempts := releaseSurplusFloatingIPs(surplus, stillHeld, bringDown)

	if len(attempted) != 1 || attempted[0] != "10.0.0.2/24" {
		t.Errorf("brought down %v, want only 10.0.0.2/24 — an address that has "+
			"gone must not reach the kernel", attempted)
	}
	if len(attempts) != 2 {
		t.Fatalf("got %d outcomes, want one per surplus address", len(attempts))
	}
	if !attempts[0].Vanished || attempts[0].Err != nil {
		t.Errorf("10.0.0.1/24: Vanished=%v Err=%v, want Vanished with no error",
			attempts[0].Vanished, attempts[0].Err)
	}
	if attempts[1].Vanished || attempts[1].Err != nil {
		t.Errorf("10.0.0.2/24: Vanished=%v Err=%v, want a plain release",
			attempts[1].Vanished, attempts[1].Err)
	}
}

// The window between the check and the kernel call cannot be closed, only
// classified: an address released by another hand inside it fails the same way,
// and must still be reported as a no-op rather than an error.
func TestReleaseTreatsALostRaceAsANoOpRatherThanAFailure(t *testing.T) {
	surplus := map[string][]string{"eth0": {"10.0.0.1/24"}}

	// Present when checked, gone by the time the failure is classified.
	calls := 0
	stillHeld := func(iface, ip string) bool {
		calls++
		return calls == 1
	}
	bringDown := func(iface, ip string) error {
		return errors.New("unable to bring down ip: cannot assign requested address")
	}

	attempts := releaseSurplusFloatingIPs(surplus, stillHeld, bringDown)

	if len(attempts) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(attempts))
	}
	if !attempts[0].Vanished {
		t.Error("Vanished = false; a release that failed because the address had " +
			"already gone got what it wanted and is not an error")
	}
	if attempts[0].Err != nil {
		t.Errorf("Err = %v, want nil", attempts[0].Err)
	}
}

// The classification must not swallow a real failure: an address the node still
// holds after a failed bring-down was genuinely not released.
func TestReleaseReportsAFailureForAnAddressStillHeld(t *testing.T) {
	surplus := map[string][]string{"eth0": {"10.0.0.1/24"}}

	stillHeld := func(iface, ip string) bool { return true }
	wantErr := errors.New("permission denied")
	bringDown := func(iface, ip string) error { return wantErr }

	attempts := releaseSurplusFloatingIPs(surplus, stillHeld, bringDown)

	if len(attempts) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(attempts))
	}
	if attempts[0].Vanished {
		t.Error("Vanished = true for an address the node still holds")
	}
	if !errors.Is(attempts[0].Err, wantErr) {
		t.Errorf("Err = %v, want %v", attempts[0].Err, wantErr)
	}
}

// Regression for docs/TEST-PLAN.md defect #58. The release pass brought surplus
// addresses down with network.BringIPdown directly, bypassing the ActiveIPs
// bookkeeping that the BringDownIP RPC handler maintains, so an address released
// locally stayed on the member's assignment list forever. Live evidence: after
// RealTest was unassigned and deleted on 2026-07-31, all four nodes released
// their 72 addresses correctly and reported all 288 of them for the next three
// days — every node Active, holding nothing.
//
// An address that is no longer held must leave the list. An address whose
// release genuinely failed must stay on it: the node is still serving it, and
// dropping it would hide the address from the next pass.
func TestReleasedAddressesLeaveTheAssignmentList(t *testing.T) {
	attempts := []releaseAttempt{
		{Iface: "eth0", IP: "10.0.0.1/24"},                            // released
		{Iface: "eth0", IP: "10.0.0.2/24", Vanished: true},            // already gone
		{Iface: "eth0", IP: "10.0.0.3/24", Err: errors.New("failed")}, // still held
	}

	got := releasedForBookkeeping(attempts)

	want := map[string]bool{"10.0.0.1/24": true, "10.0.0.2/24": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want the released and vanished addresses only", got)
	}
	for _, ip := range got {
		if !want[ip] {
			t.Errorf("%s must not be dropped from the assignment list", ip)
		}
	}
}

// The member's own list is what pulsectl status prints and what placement reads
// as this node's load, so the drop has to reach it.
func TestRemoveActiveIPsDropsOnlyTheGivenAddresses(t *testing.T) {
	m := &Member{ActiveIPs: []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}}

	m.RemoveActiveIPs([]string{"10.0.0.1/24", "10.0.0.3/24"})

	got := m.GetActiveIPs()
	if len(got) != 1 || got[0] != "10.0.0.2/24" {
		t.Errorf("ActiveIPs = %v, want only the address still held", got)
	}
}

// A node that released everything must report an empty list, not a nil-vs-empty
// distinction that reads as "no information" — deriveMemberStatus reports
// Standby off an empty list, and that is the honest answer for a node serving
// nothing.
func TestRemoveActiveIPsCanEmptyTheList(t *testing.T) {
	m := &Member{ActiveIPs: []string{"10.0.0.1/24", "10.0.0.2/24"}}

	m.RemoveActiveIPs([]string{"10.0.0.1/24", "10.0.0.2/24"})

	if got := m.GetActiveIPs(); len(got) != 0 {
		t.Errorf("ActiveIPs = %v, want empty after releasing everything", got)
	}
}
