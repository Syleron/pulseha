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

package membership

import (
	"fmt"
	"net"
	"sort"
)

// nmClaims is the question this file asks NetworkManager, narrow enough that a
// test can answer it without an nmcli on the host. network.NMView satisfies it.
//
// The two returns are not interchangeable and the caller must not collapse them.
// A claim that is not *known* is the state of an interface NM configures by DHCP,
// where the address the box is reachable on is absent from every profile and
// therefore looks exactly like an address nothing owns.
type nmClaims interface {
	Claims(iface, addr string) (claimed bool, known bool)
}

// reclaimCandidates returns, per interface, the addresses this node holds that no
// configured group accounts for — before NetworkManager is consulted at all.
//
// Separated from the decision below so the common case costs nothing: on a
// converged node every address on a PulseHA interface is either a configured
// floating IP or the node's own, this returns empty, and no nmcli runs. It is
// also where the rails that need no external opinion live, because a rail that
// can be checked from the config is worth checking before one that depends on
// another daemon answering.
func reclaimCandidates(
	managed map[string][]string,
	groups map[string][]string,
	protected map[string]bool,
	addressesOn func(iface string) []string,
) map[string][]string {

	configured := make(map[string]bool)
	for _, ips := range groups {
		for _, ip := range ips {
			configured[ipWithoutMask(ip)] = true
		}
	}

	candidates := make(map[string][]string)
	for iface := range managed {
		var unaccounted []string
		for _, addr := range addressesOn(iface) {
			key := ipWithoutMask(addr)
			switch {
			case !isReclaimableScope(key):
				// Loopback, link-local, multicast and the unspecified address are
				// never floating IPs and never PulseHA's to remove.
			case configured[key]:
				// Named by a group, so the group paths account for it whether or
				// not this node should be holding it. Deciding about it here would
				// be a second opinion on a question surplusFloatingIPs already
				// answers with the expectation set.
			case protected[key]:
				// An address the cluster itself runs on. See reclaimProtectedSet.
			default:
				unaccounted = append(unaccounted, key)
			}
		}
		if len(unaccounted) > 0 {
			sort.Strings(unaccounted)
			candidates[iface] = unaccounted
		}
	}
	return candidates
}

// reclaimableIPs decides which of those candidates are PulseHA's to remove, by
// the rule that an address NetworkManager does not claim on an interface PulseHA
// manages is an address PulseHA placed and lost track of.
//
// The inversion is deliberate and it is the whole point: the allowlist rule —
// only ever touch what a group names — is safe but cannot recover a strand,
// because the moment an address leaves the config it leaves every set any pass
// can compute (docs/TEST-PLAN.md #59, #104, #105). Ownership by exclusion can.
//
// What makes the inversion survivable is that it refuses to answer rather than
// guess. `nmcli device show` reports live kernel state, so it lists PulseHA's own
// floating IPs as NetworkManager's; only the *profile* carries intent, and only a
// profile on `manual` spells its addresses out. On an interface whose profile is
// DHCP, unreadable, or absent, the node's own management address is
// indistinguishable from a strand — so such an interface is skipped entirely and
// the caller keeps today's conservative behaviour there. An unclaimed address
// costs one address; a wrongly claimed one costs the node.
//
// Every skip comes back with a reason. An operator asking why a strand was not
// reclaimed needs to read the answer, not infer it from silence.
func reclaimableIPs(candidates map[string][]string, nm nmClaims) (map[string][]string, []string) {
	reclaim := make(map[string][]string)
	var skipped []string

	ifaces := make([]string, 0, len(candidates))
	for iface := range candidates {
		ifaces = append(ifaces, iface)
	}
	sort.Strings(ifaces)

	for _, iface := range ifaces {
		var mine []string
		for _, addr := range candidates[iface] {
			claimed, known := nm.Claims(iface, addr)
			switch {
			case !known:
				skipped = append(skipped, fmt.Sprintf(
					"%s on %s: NetworkManager's intent for this interface cannot be "+
						"enumerated, so ownership is unknown", addr, iface))
			case claimed:
				skipped = append(skipped, fmt.Sprintf(
					"%s on %s: claimed by a NetworkManager connection profile", addr, iface))
			default:
				mine = append(mine, addr)
			}
		}
		if len(mine) > 0 {
			reclaim[iface] = mine
		}
	}
	return reclaim, skipped
}

// reclaimProtectedSet is every address the cluster itself runs on: each node's
// bind address, as the config records it.
//
// Belt to the NetworkManager braces, and it is the rail that matters most on the
// case NM cannot answer for. The address a node is reachable and quorate on must
// not be removable by an inference, however well founded — so it is excluded by
// name, from the one source that is not an opinion about the network.
func reclaimProtectedSet(nodes map[string]*nodeEndpoint) map[string]bool {
	protected := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if node == nil || node.IP == "" {
			continue
		}
		protected[ipWithoutMask(node.IP)] = true
	}
	return protected
}

// nodeEndpoint is the part of a config node this file needs, so the pure
// functions above do not pull the config package into their tests.
type nodeEndpoint struct{ IP string }

// isReclaimableScope reports whether an address is of a kind that could be a
// floating IP at all.
func isReclaimableScope(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified()
}
