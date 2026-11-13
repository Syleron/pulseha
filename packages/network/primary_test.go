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
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
)

// Regression for docs/TEST-PLAN.md defect #116: a floating IP that has become
// the primary address of its subnet sources every health check on that subnet,
// and in DR mode the real servers answer those to themselves.

func addr(cidr string, secondary bool) netlink.Addr {
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		panic(err)
	}
	if secondary {
		a.Flags |= ifaFlagSecondary
	}
	return *a
}

func floatingOnly(ips ...string) func(net.IP) bool {
	return floatingSet(ips)
}

func ipsOf(addrs []netlink.Addr) []string {
	var out []string
	for _, a := range addrs {
		out = append(out, a.IP.String())
	}
	return out
}

func TestAFloatingIPPrimaryOverASecondaryStaticIsInTheWay(t *testing.T) {
	// The exact listing off end-primary's enp2s0: the floating .100 first and
	// primary, the NetworkManager static .123 behind it as a secondary.
	addrs := []netlink.Addr{
		addr("10.10.91.100/24", false),
		addr("10.10.91.123/24", true),
	}
	got := ipsOf(floatingPrimaries(addrs, floatingOnly("10.10.91.100")))
	if len(got) != 1 || got[0] != "10.10.91.100" {
		t.Fatalf("got %v, want the floating IP that is sitting in front of the static", got)
	}
}

func TestTheHealthyOrderIsLeftAlone(t *testing.T) {
	// enp1s0 on the same box: static first, floating IP a secondary. Nothing to do.
	addrs := []netlink.Addr{
		addr("10.10.90.123/24", false),
		addr("10.10.90.212/24", true),
	}
	if got := floatingPrimaries(addrs, floatingOnly("10.10.90.212")); len(got) != 0 {
		t.Fatalf("got %v, want nothing: the static is already primary", ipsOf(got))
	}
}

func TestAFloatingIPAloneInItsSubnetIsLegitimatelyPrimary(t *testing.T) {
	// A floating IP in a subnet the interface has no address of its own in. It
	// is primary because nothing else could be, and must not be churned.
	addrs := []netlink.Addr{
		addr("10.10.90.123/24", false),
		addr("192.0.2.10/24", false),
	}
	if got := floatingPrimaries(addrs, floatingOnly("192.0.2.10")); len(got) != 0 {
		t.Fatalf("got %v, want nothing: there is no static in that prefix to put first", ipsOf(got))
	}
}

func TestFloatingIPsOverOtherFloatingIPsOnlyAreLeftAlone(t *testing.T) {
	// Two floating IPs in a subnet of their own. The secondary is floating too,
	// so there is no interface address being shadowed.
	addrs := []netlink.Addr{
		addr("192.0.2.10/24", false),
		addr("192.0.2.11/24", true),
	}
	if got := floatingPrimaries(addrs, floatingOnly("192.0.2.10", "192.0.2.11")); len(got) != 0 {
		t.Fatalf("got %v, want nothing: a secondary floating IP is not a static", ipsOf(got))
	}
}

func TestASurplusFloatingIPIsNotMistakenForAStatic(t *testing.T) {
	// .11 is configured but no longer expected on this node and is on its way
	// out. If the caller passed only the expected set, .11 would read as a
	// shadowed static and .10 would be reseated for nothing — which is why the
	// caller passes the whole configured set.
	addrs := []netlink.Addr{
		addr("192.0.2.10/24", false),
		addr("192.0.2.11/24", true),
	}
	if got := floatingPrimaries(addrs, floatingOnly("192.0.2.10")); len(got) != 1 {
		t.Fatalf("with only the expected set, .11 reads as a static: got %v", ipsOf(got))
	}
	if got := floatingPrimaries(addrs, floatingOnly("192.0.2.10", "192.0.2.11")); len(got) != 0 {
		t.Fatalf("with the configured set, nothing is in the way: got %v", ipsOf(got))
	}
}

func TestIPv6IsNotConsidered(t *testing.T) {
	// IPv6 has no primary/secondary distinction; the flag is never set, and
	// source selection is RFC 6724's business.
	addrs := []netlink.Addr{
		addr("2001:db8::100/64", false),
		addr("2001:db8::123/64", true),
	}
	if got := floatingPrimaries(addrs, floatingOnly("2001:db8::100")); len(got) != 0 {
		t.Fatalf("got %v, want nothing for IPv6", ipsOf(got))
	}
}

func TestADifferentPrefixLengthIsADifferentPrefix(t *testing.T) {
	// The kernel only groups addresses with the same prefix length, so a /32
	// floating IP never competes with a /24 static for primary.
	addrs := []netlink.Addr{
		addr("10.10.91.100/32", false),
		addr("10.10.91.123/24", true),
	}
	if got := floatingPrimaries(addrs, floatingOnly("10.10.91.100")); len(got) != 0 {
		t.Fatalf("got %v, want nothing across prefix lengths", ipsOf(got))
	}
}

// scriptedKernel holds one link's IPv4 addresses and applies the kernel's rules
// for them: an add in a held prefix lands as a secondary at the end; a delete of
// a primary promotes the first secondary in its prefix when promote_secondaries
// is on, and takes every secondary in the prefix down with it when it is off.
type scriptedKernel struct {
	addrs              []netlink.Addr
	promoteSecondaries bool
	dels, adds         []string
	failAdds           map[string]int // address -> failures still to return
	listErr            error
}

func (k *scriptedKernel) AddrList(netlink.Link, int) ([]netlink.Addr, error) {
	if k.listErr != nil {
		return nil, k.listErr
	}
	out := make([]netlink.Addr, len(k.addrs))
	copy(out, k.addrs)
	return out, nil
}

func (k *scriptedKernel) AddrDel(_ netlink.Link, a *netlink.Addr) error {
	k.dels = append(k.dels, a.IP.String())
	idx := -1
	for i := range k.addrs {
		if k.addrs[i].IP.Equal(a.IP) && samePrefix(k.addrs[i].IPNet, a.IPNet) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return syscall.EADDRNOTAVAIL
	}
	gone := k.addrs[idx]
	k.addrs = append(k.addrs[:idx:idx], k.addrs[idx+1:]...)
	if gone.Flags&ifaFlagSecondary != 0 {
		return nil
	}
	// A primary went: the kernel's inet_del_ifa.
	promoted := false
	kept := k.addrs[:0]
	for _, other := range k.addrs {
		if other.Flags&ifaFlagSecondary == 0 || !samePrefix(other.IPNet, gone.IPNet) {
			kept = append(kept, other)
			continue
		}
		if !k.promoteSecondaries {
			continue // taken down with the primary
		}
		if !promoted {
			other.Flags &^= ifaFlagSecondary
			promoted = true
		}
		kept = append(kept, other)
	}
	k.addrs = kept
	return nil
}

func (k *scriptedKernel) AddrAdd(_ netlink.Link, a *netlink.Addr) error {
	k.adds = append(k.adds, a.IP.String())
	if left := k.failAdds[a.IP.String()]; left > 0 {
		k.failAdds[a.IP.String()] = left - 1
		return errors.New("netlink: operation not permitted")
	}
	for _, other := range k.addrs {
		if other.IP.Equal(a.IP) {
			return syscall.EEXIST
		}
	}
	added := *a
	for _, other := range k.addrs {
		if other.Flags&ifaFlagSecondary == 0 && samePrefix(other.IPNet, a.IPNet) {
			added.Flags |= ifaFlagSecondary
			break
		}
	}
	k.addrs = append(k.addrs, added)
	return nil
}

func (k *scriptedKernel) primaryOf(prefix string) string {
	_, want, _ := net.ParseCIDR(prefix)
	for _, a := range k.addrs {
		if a.Flags&ifaFlagSecondary == 0 && samePrefix(a.IPNet, want) {
			return a.IP.String()
		}
	}
	return ""
}

var dummyLink = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp2s0"}}

func promoteOn(string) error  { return nil }
func promoteOff(string) error { return errors.New("promote_secondaries is off") }

func TestReseatPutsTheStaticBackInFront(t *testing.T) {
	k := &scriptedKernel{
		promoteSecondaries: true,
		addrs: []netlink.Addr{
			addr("10.10.91.100/24", false),
			addr("10.10.91.123/24", true),
		},
	}

	reseated, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100/24"}, k, promoteOn)
	if err != nil {
		t.Fatal(err)
	}
	if len(reseated) != 1 || reseated[0] != "10.10.91.100" {
		t.Errorf("reseated %v, want the one floating IP", reseated)
	}
	if got := k.primaryOf("10.10.91.0/24"); got != "10.10.91.123" {
		t.Errorf("primary is %s, want the static", got)
	}
	if len(k.dels) != 1 || len(k.adds) != 1 {
		t.Errorf("dels=%v adds=%v, want exactly one delete and one add", k.dels, k.adds)
	}
	if len(k.addrs) != 2 {
		t.Errorf("the link holds %v, want both addresses still up", ipsOf(k.addrs))
	}
}

func TestReseatWorksThroughAChainOfFloatingIPs(t *testing.T) {
	// Deleting the primary promotes the *first* secondary in the prefix, and
	// here that is another floating IP: .101 was added before NetworkManager put
	// the static back. One round is not enough; the loop has to look again.
	k := &scriptedKernel{
		promoteSecondaries: true,
		addrs: []netlink.Addr{
			addr("10.10.91.100/24", false),
			addr("10.10.91.101/24", true),
			addr("10.10.91.123/24", true),
		},
	}

	reseated, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100", "10.10.91.101"}, k, promoteOn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reseated, ",") != "10.10.91.100,10.10.91.101" {
		t.Errorf("reseated %v, want both floating IPs in turn", reseated)
	}
	if got := k.primaryOf("10.10.91.0/24"); got != "10.10.91.123" {
		t.Errorf("primary is %s, want the static", got)
	}
	if len(k.addrs) != 3 {
		t.Errorf("the link holds %v, want all three still up", ipsOf(k.addrs))
	}
}

func TestReseatDoesNothingAndTouchesNoSysctlWhenNothingIsInTheWay(t *testing.T) {
	k := &scriptedKernel{
		promoteSecondaries: true,
		addrs: []netlink.Addr{
			addr("10.10.90.123/24", false),
			addr("10.10.90.212/24", true),
		},
	}
	consulted := false
	promote := func(string) error { consulted = true; return nil }

	reseated, err := reseatFloatingIPs(dummyLink, []string{"10.10.90.212/24"}, k, promote)
	if err != nil || len(reseated) != 0 {
		t.Fatalf("reseated=%v err=%v, want nothing done", reseated, err)
	}
	if consulted {
		t.Error("promote_secondaries was consulted on a pass with nothing to reseat")
	}
	if len(k.dels)+len(k.adds) != 0 {
		t.Errorf("dels=%v adds=%v, want the kernel untouched", k.dels, k.adds)
	}
}

func TestReseatRefusesWithoutPromoteSecondaries(t *testing.T) {
	// Without promote_secondaries the delete of a primary takes every secondary
	// in the prefix with it — the static included. Refusing is the only safe
	// answer, and the refusal has to come before the delete.
	k := &scriptedKernel{
		promoteSecondaries: false,
		addrs: []netlink.Addr{
			addr("10.10.91.100/24", false),
			addr("10.10.91.123/24", true),
		},
	}

	reseated, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100"}, k, promoteOff)
	if err == nil {
		t.Fatal("want an error naming the refusal")
	}
	if len(reseated) != 0 || len(k.dels) != 0 {
		t.Errorf("reseated=%v dels=%v, want nothing touched", reseated, k.dels)
	}
	if len(k.addrs) != 2 {
		t.Errorf("the link holds %v, want both addresses still up", ipsOf(k.addrs))
	}
}

func TestReseatRetriesTheAddOnceAndReportsWhatStayedDown(t *testing.T) {
	// The one moment that matters: the floating IP is deleted and the add fails.
	// One retry, and if that fails too the caller is told which address is down.
	k := &scriptedKernel{
		promoteSecondaries: true,
		addrs: []netlink.Addr{
			addr("10.10.91.100/24", false),
			addr("10.10.91.123/24", true),
		},
		failAdds: map[string]int{"10.10.91.100": 1},
	}
	reseated, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100"}, k, promoteOn)
	if err != nil {
		t.Fatalf("one failed add is retried and must succeed here: %v", err)
	}
	if len(reseated) != 1 || len(k.adds) != 2 {
		t.Errorf("reseated=%v adds=%v, want the retry to have seated it", reseated, k.adds)
	}

	k = &scriptedKernel{
		promoteSecondaries: true,
		addrs: []netlink.Addr{
			addr("10.10.91.100/24", false),
			addr("10.10.91.123/24", true),
		},
		failAdds: map[string]int{"10.10.91.100": 2},
	}
	reseated, err = reseatFloatingIPs(dummyLink, []string{"10.10.91.100"}, k, promoteOn)
	if err == nil || !strings.Contains(err.Error(), "10.10.91.100") {
		t.Fatalf("err=%v, want the address that stayed down named", err)
	}
	if len(reseated) != 0 {
		t.Errorf("reseated=%v, want an address that is down not reported as reseated", reseated)
	}
	if got := k.primaryOf("10.10.91.0/24"); got != "10.10.91.123" {
		t.Errorf("primary is %s; even on failure the static is now first", got)
	}
}

func TestReseatTreatsEEXISTOnTheAddAsSeated(t *testing.T) {
	// The watcher's restore can land between the delete and the add. Either
	// add puts the address behind the now-primary static, so EEXIST is the
	// wanted state (defect #45's shape).
	k := &scriptedKernel{
		promoteSecondaries: true,
		addrs: []netlink.Addr{
			addr("10.10.91.100/24", false),
			addr("10.10.91.123/24", true),
		},
	}
	racing := &racingKernel{scriptedKernel: k}
	reseated, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100"}, racing, promoteOn)
	if err != nil || len(reseated) != 1 {
		t.Fatalf("reseated=%v err=%v, want the address counted as seated", reseated, err)
	}
	if got := k.primaryOf("10.10.91.0/24"); got != "10.10.91.123" {
		t.Errorf("primary is %s, want the static", got)
	}
}

// racingKernel is a scriptedKernel whose delete is immediately followed by
// another writer's add of the same address.
type racingKernel struct{ *scriptedKernel }

func (r *racingKernel) AddrDel(l netlink.Link, a *netlink.Addr) error {
	if err := r.scriptedKernel.AddrDel(l, a); err != nil {
		return err
	}
	return r.scriptedKernel.AddrAdd(l, &netlink.Addr{IPNet: a.IPNet})
}

func TestReseatIsBoundedAgainstAnotherWriter(t *testing.T) {
	// A kernel that keeps presenting the floating IP as primary — another
	// writer re-ordering the addresses under us — must not keep the pass
	// forever. One round per floating address plus one, then give up.
	k := &stubbornKernel{}
	_, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100"}, k, promoteOn)
	if err != nil {
		t.Fatal(err)
	}
	if k.dels > 2 {
		t.Errorf("%d deletes on one floating address, want the loop bounded", k.dels)
	}
}

type stubbornKernel struct{ dels int }

func (s *stubbornKernel) AddrList(netlink.Link, int) ([]netlink.Addr, error) {
	return []netlink.Addr{addr("10.10.91.100/24", false), addr("10.10.91.123/24", true)}, nil
}
func (s *stubbornKernel) AddrDel(netlink.Link, *netlink.Addr) error { s.dels++; return nil }
func (s *stubbornKernel) AddrAdd(netlink.Link, *netlink.Addr) error { return nil }

func TestReseatReportsAListingFailureRatherThanActingBlind(t *testing.T) {
	k := &scriptedKernel{listErr: errors.New("interface dump interrupted")}
	_, err := reseatFloatingIPs(dummyLink, []string{"10.10.91.100"}, k, promoteOn)
	if err == nil || len(k.dels) != 0 {
		t.Fatalf("err=%v dels=%v, want the failure reported and nothing deleted", err, k.dels)
	}
}

func TestReseatWithNoFloatingIPsReadsNothing(t *testing.T) {
	k := &scriptedKernel{listErr: errors.New("should not be read")}
	reseated, err := reseatFloatingIPs(dummyLink, nil, k, promoteOn)
	if err != nil || reseated != nil {
		t.Fatalf("reseated=%v err=%v, want a no-op", reseated, err)
	}
}

// The sysctl side, against a temporary /proc/sys.

func writeSysctl(t *testing.T, root, iface, value string) string {
	t.Helper()
	dir := filepath.Join(root, "net", "ipv4", "conf", iface)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "promote_secondaries")
	if err := os.WriteFile(file, []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestPromoteSecondariesOnForAllIsEnough(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func(old string) func() { procSysRoot = root; return func() { procSysRoot = old } }(procSysRoot))
	writeSysctl(t, root, "all", "1")
	ifaceFile := writeSysctl(t, root, "enp2s0", "0")

	if err := ensurePromoteSecondaries("enp2s0"); err != nil {
		t.Fatal(err)
	}
	// The effective value is the greater of the two, so the interface's own is
	// left as the operator set it.
	if raw, _ := os.ReadFile(ifaceFile); strings.TrimSpace(string(raw)) != "0" {
		t.Errorf("interface sysctl rewritten to %q, want left alone when `all` is on", raw)
	}
}

func TestPromoteSecondariesIsEnabledOnTheInterfaceWhenBothAreOff(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func(old string) func() { procSysRoot = root; return func() { procSysRoot = old } }(procSysRoot))
	writeSysctl(t, root, "all", "0")
	ifaceFile := writeSysctl(t, root, "enp2s0", "0")

	if err := ensurePromoteSecondaries("enp2s0"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(ifaceFile); strings.TrimSpace(string(raw)) != "1" {
		t.Errorf("interface sysctl is %q, want it switched on", raw)
	}
}

func TestPromoteSecondariesThatCannotBeReadIsARefusal(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func(old string) func() { procSysRoot = root; return func() { procSysRoot = old } }(procSysRoot))
	// No files at all: an interface the kernel does not know, or a /proc that
	// is not mounted. Guessing "on" here would risk the static.
	if err := ensurePromoteSecondaries("enp2s0"); err == nil {
		t.Fatal("want a refusal when the sysctl cannot be read")
	}
}
