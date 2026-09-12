package network

import (
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
)

// fakeNMCLI answers the queries BuildNMView makes from a captured transcript,
// keyed by the join of the arguments. A query with no entry fails, which is how
// the "nmcli cannot answer" paths are driven.
func fakeNMCLI(t *testing.T, responses map[string]string) {
	t.Helper()

	prev := nmcliRunner
	t.Cleanup(func() { nmcliRunner = prev })
	nmcliRunner = func(args ...string) (string, error) {
		key := strings.Join(args, " ")
		if out, ok := responses[key]; ok {
			return out, nil
		}
		return "", errors.New("nmcli: unknown query " + key)
	}
}

// The transcript below is real output from MC-LB-3-node-1 (2026-09-12), an
// appliance whose management address is a manual NM profile and whose floating
// IP is PulseHA's.
func liveTranscript() map[string]string {
	return map[string]string{
		"--version": "nmcli tool, version 1.56.0-2.el10_2\n",
		"-g GENERAL.CONNECTION device show enp1s0":    "Network-1\n",
		"-g ipv4.method connection show Network-1":    "manual\n",
		"-g ipv4.addresses connection show Network-1": "10.20.70.21/24\n",
		"-g ipv6.method connection show Network-1":    "disabled\n",
		"-g ipv6.addresses connection show Network-1": "\n",
	}
}

func TestNMViewClaimsOnlyWhatTheProfileSpellsOut(t *testing.T) {
	fakeNMCLI(t, liveTranscript())

	view, err := BuildNMView([]string{"enp1s0"})
	if err != nil {
		t.Fatalf("BuildNMView: %v", err)
	}

	// The node's own management address: NM's, and known to be.
	claimed, known := view.Claims("enp1s0", "10.20.70.21/24")
	if !known || !claimed {
		t.Errorf("10.20.70.21 claimed=%v known=%v, want both true", claimed, known)
	}

	// The floating IP is on the same interface and in NM's *device* listing, but
	// not in its profile. This is the distinction the whole feature rests on: ask
	// `nmcli device show` and NM reports the VIP as its own, because that command
	// reports kernel state.
	claimed, known = view.Claims("enp1s0", "10.20.70.67/24")
	if !known {
		t.Fatal("the profile is manual, so the answer is known")
	}
	if claimed {
		t.Error("10.20.70.67 read as NetworkManager's; it is PulseHA's floating IP " +
			"and PulseHA would never be able to move it again")
	}
}

func TestAMaskMismatchDoesNotChangeWhoseAddressItIs(t *testing.T) {
	fakeNMCLI(t, liveTranscript())

	view, _ := BuildNMView([]string{"enp1s0"})
	// NM writes /24; ask with a bare address and with a different prefix.
	for _, form := range []string{"10.20.70.21", "10.20.70.21/32", "10.20.70.21/24"} {
		if claimed, known := view.Claims("enp1s0", form); !claimed || !known {
			t.Errorf("%s claimed=%v known=%v, want NM's under any spelling", form, claimed, known)
		}
	}
}

// The outage case. A DHCP profile lists no addresses, so the address the box is
// reachable on is absent from NM's intent and looks exactly like an unclaimed
// one. The claim must come back unknown, never "NM claims nothing".
func TestADHCPProfileIsNotEnumerable(t *testing.T) {
	responses := liveTranscript()
	responses["-g ipv4.method connection show Network-1"] = "auto\n"
	delete(responses, "-g ipv4.addresses connection show Network-1")
	fakeNMCLI(t, responses)

	view, err := BuildNMView([]string{"enp1s0"})
	if err != nil {
		t.Fatalf("BuildNMView: %v", err)
	}
	if view["enp1s0"].Enumerable {
		t.Fatal("a DHCP profile reported as enumerable: every address on the " +
			"interface would then read as PulseHA's, including the node's own")
	}
	if _, known := view.Claims("enp1s0", "10.20.70.21"); known {
		t.Error("Claims reported a known answer for a non-enumerable interface")
	}
	if view["enp1s0"].Reason == "" {
		t.Error("no reason recorded; an operator asking why nothing was reclaimed has nothing to read")
	}
}

func TestAnUnmanagedDeviceIsNotEnumerable(t *testing.T) {
	responses := liveTranscript()
	responses["-g GENERAL.CONNECTION device show enp1s0"] = "--\n"
	fakeNMCLI(t, responses)

	view, _ := BuildNMView([]string{"enp1s0"})
	if view["enp1s0"].Enumerable {
		t.Fatal("a device with no NM profile reported as enumerable; it may be " +
			"configured by ifcfg, systemd-networkd or by hand, and NM's silence says nothing")
	}
}

// A disabled family intends nothing, and that is a real answer rather than an
// unknown one — the common case for an appliance's unused IPv6.
func TestADisabledFamilyIsAnEnumerableEmptySet(t *testing.T) {
	fakeNMCLI(t, liveTranscript())

	view, _ := BuildNMView([]string{"enp1s0"})
	if !view["enp1s0"].Enumerable {
		t.Fatal("ipv6.method=disabled made the whole interface unknown")
	}
	if claimed, known := view.Claims("enp1s0", "fd00::1"); claimed || !known {
		t.Errorf("fd00::1 claimed=%v known=%v, want a known no", claimed, known)
	}
}

func TestNoNetworkManagerIsAnError(t *testing.T) {
	fakeNMCLI(t, map[string]string{})

	if _, err := BuildNMView([]string{"enp1s0"}); !errors.Is(err, ErrNoNetworkManager) {
		t.Fatalf("err = %v, want ErrNoNetworkManager so the caller falls back", err)
	}
}

func TestSplitNMAddresses(t *testing.T) {
	cases := map[string][]string{
		"10.0.0.1/24":               {"10.0.0.1/24"},
		"10.0.0.1/24, 10.0.0.2/24":  {"10.0.0.1/24", "10.0.0.2/24"},
		"10.0.0.1/24,10.0.0.2/24\n": {"10.0.0.1/24", "10.0.0.2/24"},
		"\n":                        nil,
		"":                          nil,
	}
	for input, want := range cases {
		got := splitNMAddresses(input)
		if len(got) != len(want) {
			t.Errorf("splitNMAddresses(%q) = %v, want %v", input, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitNMAddresses(%q) = %v, want %v", input, got, want)
				break
			}
		}
	}
}

// AddressesOn reads addresses back out of the inventory, which is keyed by
// ipKey's family-prefixed form — so it has to undo that encoding. It did not, and
// every address it returned failed net.ParseIP in its caller, which turned the
// whole reclaim pass into a silent no-op on a live appliance.
//
// Built from ipKey rather than from literals: a test that hardcodes "4|..." would
// keep passing if the encoding changed, which is the same blind spot again.
func TestAddressesOnReturnsAddressesNotInventoryKeys(t *testing.T) {
	inv := &IPInventory{ipToIface: map[string]string{
		ipKey(mustIP("10.20.70.21")): "enp1s0",
		ipKey(mustIP("10.20.70.78")): "enp1s0",
		ipKey(mustIP("fd00::1")):     "enp1s0",
		ipKey(mustIP("192.168.9.9")): "enp2s0",
	}}

	got := inv.AddressesOn("enp1s0")
	want := []string{"10.20.70.21", "10.20.70.78", "fd00::1"}
	if len(got) != len(want) {
		t.Fatalf("AddressesOn = %v, want %v", got, want)
	}
	for _, addr := range got {
		if net.ParseIP(addr) == nil {
			t.Errorf("AddressesOn returned %q, which is not an address any caller can parse", addr)
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("AddressesOn = %v, missing %s", got, w)
		}
	}
	if inv.AddressesOn("enp3s0") != nil {
		t.Error("AddressesOn returned addresses for an interface holding none")
	}
}

func mustIP(s string) net.IP {
	ip, _ := normalizeIP(net.ParseIP(s))
	return ip
}
