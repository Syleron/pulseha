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
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
)

// upOutcome says what happened to one address of a BringUpIP request.
type upOutcome int

const (
	// upPlaced: the interface did not have the address and this call put it there.
	upPlaced upOutcome = iota
	// upAlreadyHeld: the interface already had the address on the requested
	// interface, so no syscall was made for it at all. This is the outcome a
	// re-place needs to be cheap (docs/TEST-PLAN.md defect #64).
	upAlreadyHeld
	// upMoved: the address was on a different interface; it was taken down there
	// and placed on the requested one.
	upMoved
	// upSatisfied: the add reported failure but the interface holds the address
	// anyway — defect #45's race, a no-op rather than a fault.
	upSatisfied
	// upFailed: the add failed and the address is not held. The one outcome of
	// the five worth an error in the log, and the one that abandons the request.
	upFailed
)

// upAttempt is the outcome of one requested address.
type upAttempt struct {
	IP      string
	Outcome upOutcome
	// Err is set only for upFailed, and carries the underlying netlink error.
	Err error
}

// upSummary counts a request's outcomes so the whole request can be reported in
// one line instead of one line per address.
type upSummary struct {
	Placed      int
	AlreadyHeld int
	Moved       int
	Satisfied   int
	Failed      int
}

// normalizeUpRequest puts every requested address into CIDR form and separates
// the ones that cannot be parsed, so the placement loop below only ever sees
// addresses in the same form the expectation set and the kernel use.
//
// The counterpart of normalizeDownRequest, and it runs before anything else for
// the same reason that one does: the expectation set is registered once for the
// whole request, so the request has to be in its final form before any of it is
// applied. A request carrying an address that cannot be parsed is rejected
// whole, without touching the interface — every caller builds its list from
// validated config groups, so an unparseable entry is a bug on the sending side
// and half-applying it would hide that.
func normalizeUpRequest(ips []string) (normalized, invalid []string) {
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

// placeRequestedIPs brings up the addresses of a BringUpIP request that the
// requested interface is not already holding, and reports what happened to each
// of the rest.
//
// docs/TEST-PLAN.md defect #64, and the mirror of what #34 did to the release
// path. Two costs were paid per address rather than per request, and both of
// them scale with the group:
//
// The pre-check went through network.CheckIfIPExists, which builds a whole
// netlink inventory — every link, both families — on every call, and the failure
// path built two more. A node re-sent its own 62-address share therefore paid 62
// full interface dumps to discover it already held all 62. heldOn is one
// snapshot taken for the whole request instead, and an address already on the
// requested interface costs no syscall at all: that is what makes a redundant
// whole-share re-place cheap, which is the property run 32 needed and did not
// have when five correctly-batched requests from #37's new batcher timed out
// against a peer that could not service them.
//
// nil heldOn means the caller could not read kernel state, in which case every
// address is attempted — a filter that cannot see must not turn a bring-up into
// a silent no-op, the same rule releaseRequestedIPs follows.
//
// liveHeldOnIface classifies a failed add and must be a live check, not the
// snapshot: its whole purpose is to be newer than the syscall that just failed
// (network.AddrAddSatisfied, defect #45). The handler used to make that check
// twice, identically, which on a flood is two more whole-interface dumps per
// failing address.
//
// The loop abandons the request on the first genuine failure, as it always has,
// and returns the attempts it made up to and including that one — the caller
// announces exactly that set, because those addresses are already on the
// interface and unannounced ones are a silent partial outage (#33).
func placeRequestedIPs(iface string, ips []string,
	heldOn func(ip string) (bool, string),
	liveHeldOnIface func(ip string) bool,
	bringDown func(iface, ip string) error,
	bringUp func(iface, ip string) error,
) []upAttempt {

	attempts := make([]upAttempt, 0, len(ips))
	for _, ip := range ips {
		outcome := upPlaced

		if heldOn != nil {
			exists, onIface := heldOn(ip)
			switch {
			case exists && onIface == iface:
				attempts = append(attempts, upAttempt{IP: ip, Outcome: upAlreadyHeld})
				continue
			case exists:
				// Best-effort, exactly as before: if it cannot be taken off the
				// interface it is on, the add below reports the real problem.
				_ = bringDown(onIface, ip)
				outcome = upMoved
			}
		}

		err := bringUp(iface, ip)
		if err == nil {
			attempts = append(attempts, upAttempt{IP: ip, Outcome: outcome})
			continue
		}

		var heldByTarget func() bool
		if liveHeldOnIface != nil {
			heldByTarget = func() bool { return liveHeldOnIface(ip) }
		}
		if network.AddrAddSatisfied(err, heldByTarget) {
			attempts = append(attempts, upAttempt{IP: ip, Outcome: upSatisfied})
			continue
		}

		attempts = append(attempts, upAttempt{IP: ip, Outcome: upFailed, Err: err})
		return attempts
	}
	return attempts
}

// summarizeUpAttempts counts the outcomes of one request.
func summarizeUpAttempts(attempts []upAttempt) upSummary {
	var summary upSummary
	for _, attempt := range attempts {
		switch attempt.Outcome {
		case upPlaced:
			summary.Placed++
		case upAlreadyHeld:
			summary.AlreadyHeld++
		case upMoved:
			summary.Moved++
		case upSatisfied:
			summary.Satisfied++
		case upFailed:
			summary.Failed++
		}
	}
	return summary
}

// attemptedIPs returns the addresses a placement pass got as far as attempting,
// in request order, which is the set to announce.
//
// Every outcome belongs in it, including the ones no syscall was made for: the
// announcement re-reads each address against the kernel immediately before its
// own arping, so offering an address this node already held is what keeps a
// re-place from leaving it live and unannounced (#33's residual half). The
// kernel decides, at announce time.
func attemptedIPs(attempts []upAttempt) []string {
	ips := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		ips = append(ips, attempt.IP)
	}
	return ips
}

// missingOnIface returns the expected addresses that the given interface is not
// currently holding, decided from one snapshot rather than one netlink dump per
// address.
//
// The other half of defect #64's cost. This scan runs over a node's *whole*
// expected share, so on the 288-address topology it was 72 full interface dumps
// every time a role transition asked for it — and its output is a whole-share
// bring-up, which is how run 32's node-4 came to be handling 17 requests for the
// 62 addresses it already had.
//
// A nil snapshot means kernel state could not be read, and every expected
// address is reported missing: the same rule as placeRequestedIPs, and the same
// one the previous per-address code followed by accident, since it discarded
// CheckIfIPExists' error and read the false that came with it.
func missingOnIface(iface string, expected []string, heldOn func(ip string) (bool, string)) []string {
	var missing []string
	for _, ip := range expected {
		if ipOnly, _ := utils.GetCIDR(ip); ipOnly == nil {
			continue
		}
		if heldOn == nil {
			missing = append(missing, ip)
			continue
		}
		if exists, onIface := heldOn(ip); !exists || onIface != iface {
			missing = append(missing, ip)
		}
	}
	return missing
}

// ipInventoryLookup adapts an interface-address snapshot to the (exists, iface)
// lookup the helpers above take, and returns nil when the snapshot could not be
// built so those helpers fall back to attempting everything.
//
// An address the snapshot cannot parse is reported as not held, so it is
// attempted rather than skipped — the same direction as a nil snapshot.
func ipInventoryLookup(inventory *network.IPInventory) func(ip string) (bool, string) {
	if inventory == nil {
		return nil
	}
	return func(ip string) (bool, string) {
		ipOnly, _ := utils.GetCIDR(ip)
		if ipOnly == nil {
			return false, ""
		}
		exists, onIface, err := inventory.Exists(ipOnly.String())
		if err != nil {
			return false, ""
		}
		return exists, onIface
	}
}
