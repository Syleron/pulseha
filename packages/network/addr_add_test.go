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
)

// Regression for docs/TEST-PLAN.md defect #45, the mirror of #41 on the
// bring-up path. Adding an address a node already holds fails with EEXIST —
// `file exists` — which was logged at error level and returned as a failure.
// The enforce loop re-reported it as `ENFORCE: Failed to bring up IP on Active
// node` and OrchestrateIPFailover escalated it into `IP_FAILOVER: Some
// interfaces failed to bring up IPs`, so unlike #41 the noise did not stop at
// the log: a failover was reported broken because an address it wanted up was
// up already. Run 23 on whitecrane: 25 `failed to add addr` and 9
// `netlink.AddrAdd failed` across two nodes, then 15 failed-bring-up lines.

func TestEEXISTIsTheWantedStateNotAFailure(t *testing.T) {
	// The kernel only refuses with EEXIST when this exact address is already on
	// this exact link, which is what the caller was asking for.
	if !AddrAddSatisfied(syscall.EEXIST, func() bool { return false }) {
		t.Fatal("an add refused because the address is already there is satisfied, not failed")
	}
}

func TestEEXISTDoesNotNeedTheLiveCheck(t *testing.T) {
	// The live check is a full netlink walk. During a release storm this path
	// fires tens of times a window, and the kernel has already answered the
	// question, so nothing should ask it again.
	called := false
	AddrAddSatisfied(syscall.EEXIST, func() bool {
		called = true
		return true
	})
	if called {
		t.Fatal("EEXIST already says the address is there; the live check should not be consulted")
	}
}

func TestWrappedEEXISTIsStillRecognised(t *testing.T) {
	// netlink returns a syscall.Errno, and wraps it with the kernel's
	// extended-ack message when one is present, so the comparison has to go
	// through the error chain rather than test for equality.
	wrapped := fmt.Errorf("%w: Address already assigned", syscall.EEXIST)
	if !AddrAddSatisfied(wrapped, func() bool { return false }) {
		t.Fatal("an EEXIST carrying an extended-ack message is the same no-op")
	}
}

func TestAnAddressThatArrivedDuringTheCallIsSatisfied(t *testing.T) {
	// Any failure other than EEXIST has to be asked about: several writers add
	// addresses here, so the address may have arrived between whatever decided
	// to make this call and the syscall itself. That window cannot be closed,
	// only classified.
	other := errors.New("unable to bring IP up as netlink failed to do so")
	if !AddrAddSatisfied(other, func() bool { return true }) {
		t.Fatal("the address is up on the target interface, so the pass got the state it wanted")
	}
}

func TestAnAddressThatIsGenuinelyNotUpIsStillAFailure(t *testing.T) {
	// The line worth reading, and the one the noise would have hidden.
	if AddrAddSatisfied(syscall.EINVAL, func() bool { return false }) {
		t.Fatal("an address that is not up after a failed add is a real failure")
	}
}

func TestASuccessfulAddNeedsNoClassifying(t *testing.T) {
	if !AddrAddSatisfied(nil, nil) {
		t.Fatal("a nil error is satisfied without consulting anything")
	}
}
