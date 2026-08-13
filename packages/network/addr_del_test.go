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

	"golang.org/x/sys/unix"
)

// docs/TEST-PLAN.md defect #61. Run 29 saw 14 of these in a single group delete
// across two nodes, one per address, for releases another path had already done.

func TestAnAddressAlreadyGoneIsSatisfied(t *testing.T) {
	// The kernel answering EADDRNOTAVAIL is the wanted state stated directly:
	// the address is not on the link, which is what the delete was asking for.
	if !AddrDelSatisfied(unix.EADDRNOTAVAIL, func() bool { return true }) {
		t.Fatal("a delete refused because the address is not there is satisfied, not failed")
	}
}

func TestEADDRNOTAVAILDoesNotNeedTheLiveCheck(t *testing.T) {
	// The live check is a full netlink walk, and this path fires once per address
	// during a release storm. The kernel has already answered.
	called := false
	AddrDelSatisfied(unix.EADDRNOTAVAIL, func() bool {
		called = true
		return false
	})
	if called {
		t.Fatal("EADDRNOTAVAIL already says the address is gone; the live check should not be consulted")
	}
}

func TestWrappedEADDRNOTAVAILIsStillRecognised(t *testing.T) {
	// netlink wraps the errno with the kernel's extended-ack message when one is
	// present, so the comparison has to go through the error chain.
	wrapped := fmt.Errorf("setting address: %w", syscall.EADDRNOTAVAIL)
	if !AddrDelSatisfied(wrapped, func() bool { return true }) {
		t.Fatal("an EADDRNOTAVAIL carrying an extended-ack message is the same no-op")
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
