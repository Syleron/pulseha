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

package network

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// docs/TEST-PLAN.md defect #61. Run 29 saw 14 of these in a single group delete
// across two nodes, one per address, for releases another path had already done.
//
// #108 then removed the errno fast path these tests were written around. The
// three that asserted "EADDRNOTAVAIL is satisfied without asking" are below in
// their corrected form: the errno means no address matched the exact tuple the
// request carried, and prefix length is part of that tuple, so it does not mean
// the address is gone. #61's noise is still suppressed — an address that really
// has gone still classifies as a no-op — but it is established by looking rather
// than by assuming.

func TestAnAddressAlreadyGoneIsSatisfied(t *testing.T) {
	// #61's case, and still the common one: the kernel refuses the delete and the
	// address is genuinely not on the interface.
	if !AddrDelSatisfied(unix.EADDRNOTAVAIL, func() bool { return false }) {
		t.Fatal("a delete refused because the address is not there is satisfied, not failed")
	}
}

// The defect the fast path hid, and the reason it is gone (#108).
//
// inet_rtm_deladdr walks the interface's addresses and skips any whose prefix
// length differs from the request's, so deleting 10.0.0.5/32 against a held
// 10.0.0.5/24 matches nothing and returns EADDRNOTAVAIL with the address still
// up. Reading that errno as proof of absence reported a release that did not
// happen — the same mismatch #104 reached from the config side, where a bare
// `--ip` was defaulted to /32 against a /24 entry.
func TestEADDRNOTAVAILIsNotProofTheAddressIsGone(t *testing.T) {
	if AddrDelSatisfied(unix.EADDRNOTAVAIL, func() bool { return true }) {
		t.Fatal("EADDRNOTAVAIL treated as success while the address is still up: " +
			"a prefix-length mismatch produces exactly this, and the address stays live")
	}
}

func TestEADDRNOTAVAILConsultsTheLiveCheck(t *testing.T) {
	// The companion to the above, pinning the mechanism rather than the verdict:
	// a test that stubbed the check out would pass either way.
	called := false
	AddrDelSatisfied(unix.EADDRNOTAVAIL, func() bool {
		called = true
		return false
	})
	if !called {
		t.Fatal("the live check was skipped; the errno alone cannot tell absence " +
			"from a tuple that did not match")
	}
}

func TestWrappedEADDRNOTAVAILIsStillRecognised(t *testing.T) {
	// netlink wraps the errno with the kernel's extended-ack message when one is
	// present. The classification no longer turns on the errno, but a wrapped one
	// must still reach the same verdict as a bare one for the same live state.
	wrapped := fmt.Errorf("setting address: %w", syscall.EADDRNOTAVAIL)
	if !AddrDelSatisfied(wrapped, func() bool { return false }) {
		t.Fatal("an EADDRNOTAVAIL carrying an extended-ack message is the same no-op")
	}
	if AddrDelSatisfied(wrapped, func() bool { return true }) {
		t.Fatal("wrapping the errno must not restore the fast path #108 removed")
	}
}

func TestAnAddressThatLeftDuringTheCallIsSatisfied(t *testing.T) {
	// Any failure other than EADDRNOTAVAIL has to be asked about: several writers
	// release addresses here (the enforce loop's surplus pass, the BringDownIP
	// RPC, a group delete's release fan-out), so the address may have left between
	// whatever decided to make this call and the syscall itself.
	other := errors.New("netlink refused the delete")
	if !AddrDelSatisfied(other, func() bool { return false }) {
		t.Fatal("the address is not on the interface, so the pass got the state it wanted")
	}
}

func TestAnAddressStillUpAfterAFailedDeleteIsAFailure(t *testing.T) {
	// The line worth reading, and the one the noise would have hidden: a release
	// that did not happen on an address that is still live.
	if AddrDelSatisfied(syscall.EPERM, func() bool { return true }) {
		t.Fatal("an address still up after a failed delete is a real failure")
	}
}

func TestASuccessfulDeleteNeedsNoClassifying(t *testing.T) {
	if !AddrDelSatisfied(nil, nil) {
		t.Fatal("a nil error is satisfied without consulting anything")
	}
}

func TestAFailureWithNothingToAskStaysAFailure(t *testing.T) {
	// With no way to check the live state, an unrecognised failure must not be
	// downgraded to success.
	if AddrDelSatisfied(syscall.EPERM, nil) {
		t.Fatal("an unclassifiable failure is still a failure")
	}
}

func TestBringIPdownKeepsItsSignature(t *testing.T) {
	// BringIPdown is called from the enforce pass (via a function value), the
	// group-delete fan-out and several server paths; the classified variant is
	// additive so those keep compiling unchanged.
	var _ func(string, string) error = BringIPdown
	var _ func(string, string) (bool, error) = BringIPdownClassified
}

// The retry #108 added, which is what turns a correctly-reported failure into a
// released address. Only a differing prefix length is worth retrying: a delete
// that failed against the very address it named failed for some other reason, and
// repeating the identical request would only log twice.
func TestMustRetryWithHeldPrefix(t *testing.T) {
	held := func(cidr string) *netlink.Addr {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("ParseAddr(%s): %v", cidr, err)
		}
		return addr
	}

	if !mustRetryWithHeldPrefix(32, held("10.0.0.5/24")) {
		t.Error("asked for /32 against a held /24 and did not retry: that is the " +
			"mismatch the kernel answers EADDRNOTAVAIL to, with the address still up")
	}
	if mustRetryWithHeldPrefix(24, held("10.0.0.5/24")) {
		t.Error("retried an identical request; the failure was not about the prefix")
	}
	if mustRetryWithHeldPrefix(24, nil) {
		t.Error("retried against an address the interface does not hold")
	}
}
