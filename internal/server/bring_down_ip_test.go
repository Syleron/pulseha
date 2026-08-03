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
	"testing"
)

// heldSet turns a list of addresses into the heldHere predicate the release loop
// takes, so a test can say what the node is holding without a kernel.
func heldSet(ips ...string) func(string) bool {
	held := make(map[string]bool, len(ips))
	for _, ip := range ips {
		held[ip] = true
	}
	return func(ip string) bool { return held[ip] }
}

// TestReleaseRequestedIPsSkipsAddressesTheNodeDoesNotHold is defect #34's RPC
// half: a group-delete fans the whole group's address list out to every node, so
// a node holding none of them was asked to release all of them.
func TestReleaseRequestedIPsSkipsAddressesTheNodeDoesNotHold(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"}

	var attempted []string
	bringDown := func(iface, ip string) (bool, error) {
		attempted = append(attempted, ip)
		return false, nil
	}

	attempts := releaseRequestedIPs("eth0", requested, heldSet(), bringDown)

	if len(attempted) != 0 {
		t.Fatalf("expected no bring-down attempts for a node holding none of the addresses, got %v", attempted)
	}
	if len(attempts) != len(requested) {
		t.Fatalf("expected an outcome for every requested address, got %d of %d", len(attempts), len(requested))
	}
	for _, attempt := range attempts {
		if attempt.Outcome != downSkipped {
			t.Errorf("ip %s: expected downSkipped, got %v", attempt.IP, attempt.Outcome)
		}
		if attempt.Err != nil {
			t.Errorf("ip %s: a skipped address is not a failure, got error %v", attempt.IP, attempt.Err)
		}
	}

	summary := summarizeDownAttempts(attempts)
	if summary.Skipped != 3 || summary.Failed != 0 || summary.Released != 0 {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

// TestReleaseRequestedIPsReleasesOnlyTheHeldSubset is the mixed request: the
// filter must not become an excuse to skip work the node really has to do.
func TestReleaseRequestedIPsReleasesOnlyTheHeldSubset(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"}

	var attempted []string
	bringDown := func(iface, ip string) (bool, error) {
		if iface != "eth1" {
			t.Errorf("bring-down called for the wrong interface: %s", iface)
		}
		attempted = append(attempted, ip)
		return false, nil
	}

	attempts := releaseRequestedIPs("eth1", requested, heldSet("10.0.0.2/32", "10.0.0.4/32"), bringDown)

	if len(attempted) != 2 || attempted[0] != "10.0.0.2/32" || attempted[1] != "10.0.0.4/32" {
		t.Fatalf("expected only the held addresses to reach the kernel, got %v", attempted)
	}

	summary := summarizeDownAttempts(attempts)
	if summary.Released != 2 || summary.Skipped != 2 || summary.Failed != 0 {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

// TestReleaseRequestedIPsClassifiesTheResidualRace keeps defect #61: the
// pre-check cannot close the window between itself and the syscall, and an
// address that goes in that window is a no-op, not a failure.
func TestReleaseRequestedIPsClassifiesTheResidualRace(t *testing.T) {
	bringDown := func(iface, ip string) (bool, error) {
		return true, nil // already gone by the time the syscall ran
	}

	attempts := releaseRequestedIPs("eth0", []string{"10.0.0.1/32"}, heldSet("10.0.0.1/32"), bringDown)

	if len(attempts) != 1 {
		t.Fatalf("expected one outcome, got %d", len(attempts))
	}
	if attempts[0].Outcome != downVanished {
		t.Fatalf("expected downVanished, got %v", attempts[0].Outcome)
	}
	if summary := summarizeDownAttempts(attempts); summary.Failed != 0 || summary.Vanished != 1 {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

// TestReleaseRequestedIPsReportsGenuineFailures is the line that was always
// worth reading, and the one the noise would have hidden.
func TestReleaseRequestedIPsReportsGenuineFailures(t *testing.T) {
	boom := errors.New("unable to bring down ip")
	bringDown := func(iface, ip string) (bool, error) {
		return false, boom
	}

	attempts := releaseRequestedIPs("eth0", []string{"10.0.0.1/32"}, heldSet("10.0.0.1/32"), bringDown)

	if len(attempts) != 1 || attempts[0].Outcome != downFailed {
		t.Fatalf("expected one downFailed outcome, got %+v", attempts)
	}
	if !errors.Is(attempts[0].Err, boom) {
		t.Fatalf("expected the underlying error to survive, got %v", attempts[0].Err)
	}
	summary := summarizeDownAttempts(attempts)
	if summary.Failed != 1 || len(summary.FailedIPs) != 1 || summary.FailedIPs[0] != "10.0.0.1/32" {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

// TestReleaseRequestedIPsAttemptsEverythingWhenHeldSetIsUnknown covers the
// inventory read failing: unable to see what the node holds, the RPC must do the
// work rather than silently skip a release.
func TestReleaseRequestedIPsAttemptsEverythingWhenHeldSetIsUnknown(t *testing.T) {
	requested := []string{"10.0.0.1/32", "10.0.0.2/32"}

	var attempted []string
	bringDown := func(iface, ip string) (bool, error) {
		attempted = append(attempted, ip)
		return false, nil
	}

	// heldHere == nil is how the call site says "I could not read the kernel".
	attempts := releaseRequestedIPs("eth0", requested, nil, bringDown)

	if len(attempted) != 2 {
		t.Fatalf("expected every requested address to be attempted, got %v", attempted)
	}
	if summary := summarizeDownAttempts(attempts); summary.Released != 2 || summary.Skipped != 0 {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

func TestNormalizeDownRequest(t *testing.T) {
	normalized, invalid := normalizeDownRequest([]string{
		"10.0.0.1/32",
		"10.0.0.2",
		"fd00::1",
		"fd00::2/64",
		"not-an-ip",
		"",
	})

	want := []string{"10.0.0.1/32", "10.0.0.2/32", "fd00::1/128", "fd00::2/64"}
	if len(normalized) != len(want) {
		t.Fatalf("expected %v, got %v", want, normalized)
	}
	for i, ip := range want {
		if normalized[i] != ip {
			t.Fatalf("expected %v, got %v", want, normalized)
		}
	}
	if len(invalid) != 2 || invalid[0] != "not-an-ip" || invalid[1] != "" {
		t.Fatalf("expected the two unparseable entries to be reported, got %v", invalid)
	}
}
