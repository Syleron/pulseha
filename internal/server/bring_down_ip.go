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

import "github.com/syleron/pulseha/packages/utils"

// downOutcome says what happened to one address of a BringDownIP request.
type downOutcome int

const (
	// downReleased: the node held the address and the bring-down took it down.
	downReleased downOutcome = iota
	// downSkipped: the node was not holding the address on the requested
	// interface, so no bring-down was attempted at all.
	downSkipped
	// downVanished: a bring-down was attempted and the address had already
	// gone. A no-op, not a failure — the window between reading kernel state
	// and the syscall cannot be closed, only classified (defects #41/#61).
	downVanished
	// downFailed: the address is still held and the bring-down failed. The only
	// outcome of the four worth an error in the log.
	downFailed
)

// downAttempt is the outcome of one requested address.
type downAttempt struct {
	IP      string
	Outcome downOutcome
	// Err is set only for downFailed, and carries the underlying netlink error.
	Err error
}

// downSummary counts a request's outcomes so the whole request can be reported
// in one line instead of one line per address.
type downSummary struct {
	Released  int
	Skipped   int
	Vanished  int
	Failed    int
	FailedIPs []string
}

// normalizeDownRequest puts every requested address into CIDR form and separates
// the ones that cannot be parsed, so the release loop below only ever sees
// addresses in the same form the expectation set and the kernel use.
func normalizeDownRequest(ips []string) (normalized, invalid []string) {
	normalized = make([]string, 0, len(ips))
	for _, ip := range ips {
		switch {
		case utils.IsCIDR(ip):
			normalized = append(normalized, ip)
		case utils.IsIPv4(ip):
			normalized = append(normalized, ip+"/32")
		case utils.IsIPv6(ip):
			normalized = append(normalized, ip+"/128")
		default:
			invalid = append(invalid, ip)
		}
	}
	return normalized, invalid
}

// releaseRequestedIPs brings down the addresses of a BringDownIP request that
// this node is actually holding on the requested interface, and reports what
// happened to each of the rest.
//
// docs/TEST-PLAN.md defect #34, RPC half. The RPC did not filter the request
// against what the node holds, and it cannot rely on its caller to: deleting a
// group fans the whole group's address list out to every node that has the
// interface, because no RPC exposes a peer's interface state to ask with (the
// same wall #54 hit, and the reason releaseGroupIPsOnTarget documents its
// peer's per-address failures as invisible). Run 17 caught the consequence —
// node-4 was sent `RPC BringDownIP for 201 IP(s)` for a group it held none of
// and produced 201 error lines, which is the noise that would hide a release
// that mattered. #61 later made those lines Debug rather than Error; the work
// and the log volume were still one per address. Filtering removes both.
//
// heldHere is a snapshot lookup, taken once for the whole request rather than
// per address, and nil means the call site could not read kernel state at all —
// in which case every address is attempted, because a filter that cannot see is
// not allowed to turn a release into a silent no-op.
//
// The pre-check does not close the race, and is not meant to: an address that
// comes up between the snapshot and the loop is skipped here, and the node's own
// enforce pass releases it on the next tick, since the expectation was already
// dropped before this ran. That is the same residual the enforce pass accepts in
// the other direction (#41), classified the same way.
func releaseRequestedIPs(iface string, ips []string,
	heldHere func(ip string) bool,
	bringDown func(iface, ip string) (alreadyGone bool, err error),
) []downAttempt {

	attempts := make([]downAttempt, 0, len(ips))
	for _, ip := range ips {
		if heldHere != nil && !heldHere(ip) {
			attempts = append(attempts, downAttempt{IP: ip, Outcome: downSkipped})
			continue
		}
		alreadyGone, err := bringDown(iface, ip)
		switch {
		case err != nil:
			attempts = append(attempts, downAttempt{IP: ip, Outcome: downFailed, Err: err})
		case alreadyGone:
			attempts = append(attempts, downAttempt{IP: ip, Outcome: downVanished})
		default:
			attempts = append(attempts, downAttempt{IP: ip, Outcome: downReleased})
		}
	}
	return attempts
}

// summarizeDownAttempts counts the outcomes of one request.
func summarizeDownAttempts(attempts []downAttempt) downSummary {
	var summary downSummary
	for _, attempt := range attempts {
		switch attempt.Outcome {
		case downReleased:
			summary.Released++
		case downSkipped:
			summary.Skipped++
		case downVanished:
			summary.Vanished++
		case downFailed:
			summary.Failed++
			summary.FailedIPs = append(summary.FailedIPs, attempt.IP)
		}
	}
	return summary
}
