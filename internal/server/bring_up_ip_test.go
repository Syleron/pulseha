// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package server

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// heldOnIface turns a map of address->interface into the snapshot lookup the
// placement loop takes, so a test can say what the node is holding and where
// without a kernel.
func heldOnIface(placement map[string]string) func(string) (bool, string) {
	return func(ip string) (bool, string) {
		iface, ok := placement[ip]
		return ok, iface
	}
}

// failBringUp is a bring-up that must never be called.
func failBringUp(t *testing.T) func(string, string) error {
	return func(iface, ip string) error {
		t.Errorf("bring-up called for %s on %s; the interface already holds it", ip, iface)
		return nil
	}
}

// TestPlaceRequestedIPsSkipsAddressesTheInterfaceAlreadyHolds is defect #64's
// core requirement: a whole-share re-place must cost nothing for the addresses
// the node already has. Run 32's node-4 was handed its own 62-address share 17
// times over and paid a full bring-up and a full netlink dump for every one.
func TestPlaceRequestedIPsSkipsAddressesTheInterfaceAlreadyHolds(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"}
	held := map[string]string{
		"10.0.0.1/32": "eth0",
		"10.0.0.2/32": "eth0",
		"10.0.0.3/32": "eth0",
	}

	attempts := placeRequestedIPs("eth0", requested, heldOnIface(held), nil,
		func(iface, ip string) error { return nil }, failBringUp(t))

	if len(attempts) != len(requested) {
		t.Fatalf("expected an outcome for every requested address, got %d of %d", len(attempts), len(requested))
	}
	for _, attempt := range attempts {
		if attempt.Outcome != upAlreadyHeld {
			t.Errorf("ip %s: expected upAlreadyHeld, got %v", attempt.IP, attempt.Outcome)
		}
	}

	summary := summarizeUpAttempts(attempts)
	if summary.AlreadyHeld != 3 || summary.Placed != 0 || summary.Failed != 0 {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

// TestPlaceRequestedIPsPlacesOnlyTheMissingSubset is the mixed case a real
// re-place arrives as: most of the share already up, a few genuinely new.
func TestPlaceRequestedIPsPlacesOnlyTheMissingSubset(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"}
	held := map[string]string{
		"10.0.0.1/32": "eth0",
		"10.0.0.3/32": "eth0",
	}

	var placed []string
	attempts := placeRequestedIPs("eth0", requested, heldOnIface(held), nil,
		func(iface, ip string) error { return nil },
		func(iface, ip string) error {
			placed = append(placed, ip)
			return nil
		})

	want := []string{"10.0.0.2/32", "10.0.0.4/32"}
	if !reflect.DeepEqual(placed, want) {
		t.Fatalf("expected only the missing addresses to be brought up, got %v want %v", placed, want)
	}

	summary := summarizeUpAttempts(attempts)
	if summary.Placed != 2 || summary.AlreadyHeld != 2 {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

// TestPlaceRequestedIPsAttemptsEverythingWhenSnapshotIsUnknown: a filter that
// cannot see kernel state must not turn a bring-up into a silent no-op. Same
// rule releaseRequestedIPs follows for a failed interface dump.
func TestPlaceRequestedIPsAttemptsEverythingWhenSnapshotIsUnknown(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32"}

	var placed []string
	attempts := placeRequestedIPs("eth0", requested, nil, nil,
		func(iface, ip string) error { return nil },
		func(iface, ip string) error {
			placed = append(placed, ip)
			return nil
		})

	if !reflect.DeepEqual(placed, requested) {
		t.Fatalf("expected every address attempted with no snapshot, got %v", placed)
	}
	for _, attempt := range attempts {
		if attempt.Outcome != upPlaced {
			t.Errorf("ip %s: expected upPlaced, got %v", attempt.IP, attempt.Outcome)
		}
	}
}

// TestPlaceRequestedIPsMovesAnAddressHeldElsewhere preserves the handler's
// existing behaviour for an address sitting on the wrong interface: take it down
// there first, then place it here.
func TestPlaceRequestedIPsMovesAnAddressHeldElsewhere(t *testing.T) {
	held := map[string]string{"10.0.0.1/32": "eth1"}

	var downCalls, upCalls []string
	attempts := placeRequestedIPs("eth0", []string{"10.0.0.1/32"}, heldOnIface(held), nil,
		func(iface, ip string) error {
			downCalls = append(downCalls, iface+" "+ip)
			return nil
		},
		func(iface, ip string) error {
			upCalls = append(upCalls, iface+" "+ip)
			return nil
		})

	if want := []string{"eth1 10.0.0.1/32"}; !reflect.DeepEqual(downCalls, want) {
		t.Fatalf("expected the address taken down on the interface holding it, got %v", downCalls)
	}
	if want := []string{"eth0 10.0.0.1/32"}; !reflect.DeepEqual(upCalls, want) {
		t.Fatalf("expected the address placed on the requested interface, got %v", upCalls)
	}
	if attempts[0].Outcome != upMoved {
		t.Fatalf("expected upMoved, got %v", attempts[0].Outcome)
	}
}

// TestPlaceRequestedIPsClassifiesTheResidualRace is defect #45 on this path: an
// add that fails for an address the kernel does in fact hold is a no-op, not a
// fault, and the check that says so has to be live rather than the snapshot.
func TestPlaceRequestedIPsClassifiesTheResidualRace(t *testing.T) {
	// EEXIST needs no live check at all — the kernel already said the address is
	// on this link with this prefix.
	attempts := placeRequestedIPs("eth0", []string{"10.0.0.1/32"}, heldOnIface(nil), nil,
		func(iface, ip string) error { return nil },
		func(iface, ip string) error { return os.ErrExist })
	if attempts[0].Outcome != upSatisfied {
		t.Fatalf("EEXIST: expected upSatisfied, got %v (err %v)", attempts[0].Outcome, attempts[0].Err)
	}

	// Any other failure is asked about, live.
	liveCalls := 0
	attempts = placeRequestedIPs("eth0", []string{"10.0.0.1/32"}, heldOnIface(nil),
		func(ip string) bool {
			liveCalls++
			return true
		},
		func(iface, ip string) error { return nil },
		func(iface, ip string) error { return errors.New("netlink busy") })
	if attempts[0].Outcome != upSatisfied {
		t.Fatalf("live-held: expected upSatisfied, got %v", attempts[0].Outcome)
	}
	if liveCalls != 1 {
		t.Fatalf("expected exactly one live re-check per failing address, got %d", liveCalls)
	}
}

// TestPlaceRequestedIPsAbandonsTheRequestOnGenuineFailure keeps the handler's
// contract: a failure on an address that is genuinely not up stops the request
// and is the line worth reading — and everything attempted before it is still
// returned, because those addresses are on the interface and an unannounced
// address is a silent partial outage (#33).
func TestPlaceRequestedIPsAbandonsTheRequestOnGenuineFailure(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"}
	boom := errors.New("no such device")

	attempts := placeRequestedIPs("eth0", requested, heldOnIface(nil),
		func(ip string) bool { return false },
		func(iface, ip string) error { return nil },
		func(iface, ip string) error {
			if ip == "10.0.0.2/32" {
				return boom
			}
			return nil
		})

	if len(attempts) != 2 {
		t.Fatalf("expected the request abandoned at the second address, got %d attempts", len(attempts))
	}
	if attempts[1].Outcome != upFailed || !errors.Is(attempts[1].Err, boom) {
		t.Fatalf("expected the failure reported with its error, got %+v", attempts[1])
	}
	if want := []string{"10.0.0.1/32", "10.0.0.2/32"}; !reflect.DeepEqual(attemptedIPs(attempts), want) {
		t.Fatalf("expected everything attempted to be announced, got %v", attemptedIPs(attempts))
	}
}

// TestAttemptedIPsIncludesAddressesNoSyscallWasMadeFor is #33's residual half
// meeting #64's skip: skipping the bring-up for an address must not skip its
// announcement, or a re-place leaves it live under a holder that never announced.
func TestAttemptedIPsIncludesAddressesNoSyscallWasMadeFor(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32"}
	held := map[string]string{"10.0.0.1/32": "eth0"}

	attempts := placeRequestedIPs("eth0", requested, heldOnIface(held), nil,
		func(iface, ip string) error { return nil },
		func(iface, ip string) error { return nil })

	if !reflect.DeepEqual(attemptedIPs(attempts), requested) {
		t.Fatalf("expected every requested address announced, got %v", attemptedIPs(attempts))
	}
}

// TestMissingOnIfaceUsesOneSnapshot is the other half of #64's cost: the
// whole-share rescan that produced those re-places asked the kernel once per
// expected address.
func TestMissingOnIfaceUsesOneSnapshot(t *testing.T) {
	expected := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"}
	held := map[string]string{
		"10.0.0.1/32": "eth0",
		"10.0.0.2/32": "eth1", // present, wrong interface: still missing here
		"10.0.0.4/32": "eth0",
	}

	got, invalid := missingOnIface("eth0", expected, heldOnIface(held))
	if want := []string{"10.0.0.2/32", "10.0.0.3/32"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if len(invalid) != 0 {
		t.Errorf("reported %v as unparseable, want none", invalid)
	}

	// No snapshot: everything is reported missing rather than silently satisfied.
	got, _ = missingOnIface("eth0", expected, nil)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("with no snapshot expected every address missing, got %v", got)
	}

	// An unparseable entry is still skipped rather than attempted — but it is now
	// reported rather than dropped on the floor. normalizeUpRequest rejects the whole
	// request for the same input, and one path being loud while this one was silent
	// meant a malformed config entry looked like an address that would not come up.
	got, invalid = missingOnIface("eth0", []string{"not-an-ip", "10.0.0.9/32"}, nil)
	if want := []string{"10.0.0.9/32"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected the unparseable address skipped and the rest kept, got %v", got)
	}
	if want := []string{"not-an-ip"}; !reflect.DeepEqual(invalid, want) {
		t.Fatalf("invalid = %v, want %v; a skipped address the caller cannot see is a "+
			"floating IP that silently never gets placed", invalid, want)
	}
}

func TestNormalizeUpRequest(t *testing.T) {
	normalized, invalid := normalizeUpRequest([]string{
		"10.0.0.1/32", "10.0.0.2", "fd00::1", "fd00::2/128", "nonsense",
	})

	want := []string{"10.0.0.1/32", "10.0.0.2/32", "fd00::1/128", "fd00::2/128"}
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("got %v want %v", normalized, want)
	}
	if !reflect.DeepEqual(invalid, []string{"nonsense"}) {
		t.Fatalf("expected the unparseable address separated, got %v", invalid)
	}
}
