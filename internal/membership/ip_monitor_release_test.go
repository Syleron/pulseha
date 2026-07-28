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
