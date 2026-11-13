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
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// ifaFlagSecondary is the kernel's IFA_F_SECONDARY. Spelled out rather than taken
// from x/sys/unix, which declares it for Linux only, and this package compiles on
// the developer machines (see addrHeldOnLink).
const ifaFlagSecondary = 0x01

// Why a floating IP must never be the primary address of its subnet
// (docs/adr/0006-a-floating-ip-is-never-the-primary-address-of-its-subnet.md;
// docs/TEST-PLAN.md defect #116 has the live run).
//
// The kernel marks the first address added in a given prefix on an interface as
// primary and every later one in that prefix as secondary. The primary is what
// outbound traffic to that subnet is sourced from: the kernel installs the prefix
// route with the primary as its preferred source, and a secondary gets no prefix
// route at all. So on a load balancer whose static address came up first, a
// floating IP lands as secondary and health checks leave from the static, which
// is right. In DR mode the real servers hold the floating IP on loopback, so a
// health check sourced from it is answered to themselves and the server is
// marked down.
//
// Nothing PulseHA does changes that order, but NetworkManager does: a profile
// re-apply deletes and re-adds the static. The appliance runs
// promote_secondaries=1, so the delete promotes the floating IP to primary rather
// than taking the static down with it, and the static comes back as a secondary
// behind it. From then on every health check on that subnet is sourced from the
// floating IP, until the floating IP is removed and re-added.
//
// That is what ReseatFloatingIPs does: for a floating IP found primary over a
// secondary address that is not a floating IP, delete it and add it straight
// back. The delete promotes the first secondary in the prefix; the add lands
// behind it. It is gated on promote_secondaries, because without it the delete
// would also take the static — the box's own address — off the interface.

// floatingPrimaries returns, in list order, every IPv4 floating address on a link
// that is primary in a prefix which also holds a *secondary* address that is not
// a floating IP.
//
// A floating IP that is primary alone in its prefix, or over other floating IPs
// only, is left as it is: the interface has no address of its own in that subnet
// for the kernel to prefer, so there is nothing to put first. IPv6 has no
// primary/secondary distinction and is not considered.
func floatingPrimaries(addrs []netlink.Addr, isFloating func(net.IP) bool) []netlink.Addr {
	var out []netlink.Addr
	for _, candidate := range addrs {
		if candidate.IPNet == nil || candidate.IP.To4() == nil {
			continue
		}
		if candidate.Flags&ifaFlagSecondary != 0 || !isFloating(candidate.IP) {
			continue
		}
		for _, other := range addrs {
			if other.IPNet == nil || other.IP.To4() == nil || other.Flags&ifaFlagSecondary == 0 {
				continue
			}
			if isFloating(other.IP) || !samePrefix(candidate.IPNet, other.IPNet) {
				continue
			}
			out = append(out, candidate)
			break
		}
	}
	return out
}

// samePrefix reports whether two addresses share the kernel's notion of a prefix:
// the same prefix length and the same network under it.
func samePrefix(a, b *net.IPNet) bool {
	aOnes, aBits := a.Mask.Size()
	bOnes, bBits := b.Mask.Size()
	if aOnes != bOnes || aBits != bBits {
		return false
	}
	return a.IP.Mask(a.Mask).Equal(b.IP.Mask(b.Mask))
}

// linkAddrOps is the slice of netlink a reseat needs, indirected so the loop can
// be driven against a scripted kernel off Linux.
type linkAddrOps interface {
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	AddrDel(link netlink.Link, addr *netlink.Addr) error
	AddrAdd(link netlink.Link, addr *netlink.Addr) error
}

type netlinkAddrOps struct{}

func (netlinkAddrOps) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return netlink.AddrList(link, family)
}
func (netlinkAddrOps) AddrDel(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrDel(link, addr)
}
func (netlinkAddrOps) AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrAdd(link, addr)
}

// ReseatFloatingIPs puts the floating IPs that have become the primary address
// of their subnet on iface back behind the interface's own address, and returns
// the ones it moved.
//
// floating is every address PulseHA could have placed on this interface — the
// whole configured set, not only what this node is expected to hold — because an
// address on its way out is still not the interface's own, and must not be
// mistaken for a static to put first.
//
// An error means at least one address could not be put back after its delete,
// and names it. The caller re-places such an address the way it places any
// missing one; this function has already retried once.
func ReseatFloatingIPs(iface string, floating []string) (reseated []string, err error) {
	exists, link := InterfaceExist(iface)
	if !exists {
		return nil, errors.New("unable to reseat floating IPs as the network interface does not exist")
	}
	return reseatFloatingIPs(link, floating, netlinkAddrOps{}, ensurePromoteSecondaries)
}

// reseatFloatingIPs is ReseatFloatingIPs with its kernel access injected.
//
// Iterative on purpose. Deleting a primary promotes the first secondary in the
// prefix, and that may be another floating IP rather than the static, so each
// round re-reads the link and moves the one address now in the way. Bounded by
// the number of floating addresses plus one: each round seats one floating IP
// behind the static for good, so a run that takes more rounds than that is
// fighting another writer and is better left to the next pass.
func reseatFloatingIPs(link netlink.Link, floating []string, ops linkAddrOps,
	promoteSecondaries func(iface string) error,
) (reseated []string, err error) {
	if len(floating) == 0 {
		return nil, nil
	}
	isFloating := floatingSet(floating)
	iface := link.Attrs().Name

	var failed []string
	for round := 0; round <= len(floating); round++ {
		addrs, listErr := ops.AddrList(link, unix.AF_INET)
		if listErr != nil {
			return reseated, fmt.Errorf("read addresses on %s: %w", iface, listErr)
		}
		inTheWay := floatingPrimaries(addrs, isFloating)
		if len(inTheWay) == 0 {
			break
		}

		// Only now, once there is something to do: a pass with nothing to reseat
		// must not touch sysctls.
		if promoteErr := promoteSecondaries(iface); promoteErr != nil {
			return reseated, fmt.Errorf("refusing to reseat %s on %s: %w",
				inTheWay[0].IP, iface, promoteErr)
		}

		held := inTheWay[0]
		// A fresh Addr rather than the listed one: the listing carries kernel
		// state (IFA_F_PERMANENT, the label, the cache info) that an add has no
		// business restating. The prefix length is kept, since that is what
		// decides which addresses the kernel groups it with.
		again := &netlink.Addr{IPNet: held.IPNet}
		if delErr := ops.AddrDel(link, &held); delErr != nil {
			return reseated, fmt.Errorf("delete %s on %s: %w", held.IPNet, iface, delErr)
		}
		if addErr := ops.AddrAdd(link, again); addErr != nil && !AddrAddSatisfied(addErr, nil) {
			// The address is down and serving nothing: this is the one moment in
			// the whole operation that matters, so try once more before handing
			// it to the caller.
			if retryErr := ops.AddrAdd(link, again); retryErr != nil && !AddrAddSatisfied(retryErr, nil) {
				failed = append(failed, held.IP.String())
				continue
			}
		}
		reseated = append(reseated, held.IP.String())
	}

	if len(failed) > 0 {
		return reseated, fmt.Errorf("floating IPs deleted but not put back on %s: %s",
			iface, strings.Join(failed, ", "))
	}
	return reseated, nil
}

// floatingSet builds the membership test for a list of floating addresses, given
// with or without prefix lengths; only the address identifies a floating IP.
func floatingSet(floating []string) func(net.IP) bool {
	set := make(map[string]bool, len(floating))
	for _, entry := range floating {
		ip := net.ParseIP(ipWithoutPrefix(entry))
		if ip == nil {
			continue
		}
		if normalized, ok := normalizeIP(ip); ok {
			set[ipKey(normalized)] = true
		}
	}
	return func(ip net.IP) bool {
		normalized, ok := normalizeIP(ip)
		return ok && set[ipKey(normalized)]
	}
}

// procSysRoot is where the kernel's sysctl tree is mounted, indirected for tests.
var procSysRoot = "/proc/sys"

// ensurePromoteSecondaries makes sure the kernel will promote a secondary when a
// primary is deleted on iface, and errors when it cannot be sure.
//
// The effective value is the greater of `all` and the interface's own
// (IN_DEV_MAXCONF), so either being 1 is enough. When neither is, the
// interface's own is set: this is what keeps a delete of a primary floating IP —
// which every failover performs — from taking the interface's static address
// down with it, and PulseHA runs as root precisely to manage addresses on the
// interfaces it is given.
func ensurePromoteSecondaries(iface string) error {
	ifaceFile := filepath.Join(procSysRoot, "net", "ipv4", "conf", iface, "promote_secondaries")
	allFile := filepath.Join(procSysRoot, "net", "ipv4", "conf", "all", "promote_secondaries")

	for _, file := range []string{allFile, ifaceFile} {
		on, err := sysctlOn(file)
		if err != nil {
			return err
		}
		if on {
			return nil
		}
	}

	if err := os.WriteFile(ifaceFile, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("promote_secondaries is off for %s and could not be enabled: %w", iface, err)
	}
	on, err := sysctlOn(ifaceFile)
	if err != nil {
		return err
	}
	if !on {
		return fmt.Errorf("promote_secondaries is off for %s and stayed off after enabling it", iface)
	}
	return nil
}

func sysctlOn(file string) (bool, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", file, err)
	}
	return strings.TrimSpace(string(raw)) != "0", nil
}
