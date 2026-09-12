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
	"os/exec"
	"strings"
)

// NMClaim is what NetworkManager intends to hold on one interface.
//
// Enumerable is the field that matters, and it is not the same question as "does
// Addresses have anything in it". NM's *intent* can be listed only for a profile
// that spells its addresses out — `ipv4.method manual`, or `disabled`, which
// intends nothing. A profile on `auto` holds whatever DHCP gave it, which no
// profile query can report, so its address is indistinguishable from an
// unclaimed one. Anything built on "not NM's, therefore mine" must refuse to
// answer for such an interface rather than guess, because the address it would
// be guessing about is the one the box is reachable on.
type NMClaim struct {
	// Addresses are the addresses NM's profile claims, keyed without a prefix
	// length: NM writes 10.0.0.1/24 where the kernel may hold the same address
	// under a different mask, and for ownership only the address matters.
	Addresses map[string]bool
	// Enumerable reports whether this interface's claim could be established at
	// all. False means unknown, never "NM claims nothing".
	Enumerable bool
	// Reason says why an interface is not enumerable, for the log line an
	// operator reads when a reclaim does not happen.
	Reason string
}

// NMView is the claim per interface name.
type NMView map[string]NMClaim

// Claims reports whether NM's profile for iface claims addr (given with or
// without a prefix length), and whether that answer is trustworthy.
//
// The two returns are deliberately separate. A caller deciding "is this address
// mine to remove" must treat unknown as "not mine": the failure modes are not
// symmetric, since leaving a stray address up costs an address and removing the
// wrong one costs the node.
func (v NMView) Claims(iface, addr string) (claimed bool, known bool) {
	claim, ok := v[iface]
	if !ok || !claim.Enumerable {
		return false, false
	}
	return claim.Addresses[ipWithoutPrefix(addr)], true
}

// nmcliRunner is how the queries below reach nmcli. A field so the parsing can
// be tested against captured output without a NetworkManager on the test host.
var nmcliRunner = func(args ...string) (string, error) {
	out, err := exec.Command("nmcli", args...).Output()
	return string(out), err
}

// ErrNoNetworkManager reports that nmcli could not be run at all, so nothing on
// this host can be attributed to NetworkManager.
var ErrNoNetworkManager = errors.New("nmcli is not available")

// BuildNMView asks NetworkManager what it intends to hold on each of the given
// interfaces.
//
// Queried per interface and per field rather than in one tabular call, because
// nmcli's `-t` output escapes its own separator and connection names are free
// text — parsing a table means getting that unescaping right for a value an
// operator chose. `-g` on a single field returns the value alone, and there is
// one call per interface plus three per connection, over a handful of
// interfaces, once per enforce tick at most.
//
// An error means NetworkManager itself could not be reached; a per-interface
// failure is not an error but a non-enumerable claim, which is the conservative
// answer and the one that makes the caller fall back.
func BuildNMView(ifaces []string) (NMView, error) {
	if _, err := nmcliRunner("--version"); err != nil {
		return nil, ErrNoNetworkManager
	}

	view := make(NMView, len(ifaces))
	for _, iface := range ifaces {
		view[iface] = nmClaimForIface(iface)
	}
	return view, nil
}

// nmClaimForIface resolves one interface's active connection profile and reads
// the addresses it spells out.
func nmClaimForIface(iface string) NMClaim {
	conn, err := nmcliRunner("-g", "GENERAL.CONNECTION", "device", "show", iface)
	if err != nil {
		return NMClaim{Reason: "nmcli could not describe the device: " + err.Error()}
	}
	name := strings.TrimSpace(conn)
	if name == "" || name == "--" {
		// NM has no profile on this device: it may be unmanaged, and an unmanaged
		// device is configured by something else — ifcfg, systemd-networkd, a
		// hand-run `ip addr add`. NM's silence says nothing about who owns what
		// there.
		return NMClaim{Reason: "no NetworkManager connection is active on this device"}
	}

	addresses := make(map[string]bool)
	for _, family := range []struct{ method, addrs string }{
		{"ipv4.method", "ipv4.addresses"},
		{"ipv6.method", "ipv6.addresses"},
	} {
		method, err := nmcliRunner("-g", family.method, "connection", "show", name)
		if err != nil {
			return NMClaim{Reason: "nmcli could not read " + family.method + " of profile " + name}
		}
		switch strings.TrimSpace(method) {
		case "disabled", "ignore":
			// NM intends no address of this family. An enumerable empty set.
			continue
		case "manual":
			// The only method whose addresses are in the profile.
		default:
			// auto, shared, link-local: the address exists but not in any form
			// that can be listed ahead of time.
			return NMClaim{Reason: "profile " + name + " uses " + family.method + "=" +
				strings.TrimSpace(method) + ", whose addresses cannot be listed from the profile"}
		}

		list, err := nmcliRunner("-g", family.addrs, "connection", "show", name)
		if err != nil {
			return NMClaim{Reason: "nmcli could not read " + family.addrs + " of profile " + name}
		}
		for _, addr := range splitNMAddresses(list) {
			addresses[ipWithoutPrefix(addr)] = true
		}
	}

	return NMClaim{Addresses: addresses, Enumerable: true}
}

// splitNMAddresses splits an `ipv4.addresses` value into its addresses. nmcli
// joins them with ", "; a single value arrives with no separator at all, and an
// empty setting as an empty line.
func splitNMAddresses(value string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ipWithoutPrefix returns an address with any prefix length stripped, so
// "10.0.0.1/24" and "10.0.0.1" are the same key. NM and the kernel do not always
// agree on the mask, and for the question "whose address is this" they need not.
func ipWithoutPrefix(ip string) string {
	if i := strings.Index(ip, "/"); i >= 0 {
		return ip[:i]
	}
	return ip
}
