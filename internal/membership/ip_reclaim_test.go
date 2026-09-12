package membership

import (
	"slices"
	"strings"
	"testing"
)

// fakeNM answers Claims from a fixed picture: enumerable interfaces and, for
// each, the addresses NetworkManager's profile spells out.
type fakeNM struct {
	enumerable map[string]bool
	claimed    map[string]map[string]bool
}

func (f fakeNM) Claims(iface, addr string) (bool, bool) {
	if !f.enumerable[iface] {
		return false, false
	}
	return f.claimed[iface][ipWithoutMask(addr)], true
}

// The appliance this feature was built for: enp1s0 carries the node's own
// address (an NM `manual` profile), one configured floating IP, and one strand
// left by docs/TEST-PLAN.md #105.
func applianceFixture() (managed map[string][]string, groups map[string][]string,
	protected map[string]bool, on func(string) []string, nm fakeNM) {

	managed = map[string][]string{"enp1s0": {"Management"}}
	groups = map[string][]string{"Management": {"10.20.70.67/24"}}
	protected = reclaimProtectedSet(map[string]*nodeEndpoint{
		"6f1-5ed-3c6-a04": {IP: "10.20.70.21"},
	})
	on = func(iface string) []string {
		if iface != "enp1s0" {
			return nil
		}
		return []string{"10.20.70.21", "10.20.70.67", "10.20.70.78"}
	}
	nm = fakeNM{
		enumerable: map[string]bool{"enp1s0": true},
		claimed:    map[string]map[string]bool{"enp1s0": {"10.20.70.21": true}},
	}
	return managed, groups, protected, on, nm
}

func TestAStrandIsReclaimed(t *testing.T) {
	managed, groups, protected, on, nm := applianceFixture()

	candidates := reclaimCandidates(managed, groups, protected, on)
	reclaim, _ := reclaimableIPs(candidates, nm)

	if !slices.Equal(reclaim["enp1s0"], []string{"10.20.70.78"}) {
		t.Fatalf("reclaim = %v, want exactly the strand 10.20.70.78", reclaim)
	}
}

// The three rails that keep the node alive, each checked on its own so a change
// that removes one fails here rather than on an appliance.
func TestWhatIsNeverReclaimed(t *testing.T) {
	managed, groups, protected, on, nm := applianceFixture()

	candidates := reclaimCandidates(managed, groups, protected, on)
	reclaim, _ := reclaimableIPs(candidates, nm)
	got := reclaim["enp1s0"]

	if slices.Contains(got, "10.20.70.21") {
		t.Error("the node's own bind address was reclaimed; the box would go off the network")
	}
	if slices.Contains(got, "10.20.70.67") {
		t.Error("a configured floating IP was reclaimed; the group paths own that address")
	}
}

// The bind address is excluded by name, from the config, and not only because
// NetworkManager happens to claim it. On a DHCP interface NM cannot claim it, and
// that is exactly when this rail has to hold.
func TestTheBindAddressIsProtectedEvenWhenNetworkManagerCannotClaimIt(t *testing.T) {
	managed, groups, protected, on, _ := applianceFixture()

	candidates := reclaimCandidates(managed, groups, protected, on)
	if slices.Contains(candidates["enp1s0"], "10.20.70.21") {
		t.Fatal("the bind address reached the NetworkManager question at all; it must " +
			"be excluded before anything external is consulted")
	}
}

// The outage case, and the reason the feature is gated rather than simply on. A
// DHCP profile lists no addresses, so NM's silence about an address means nothing
// — and the address it is silent about may be the one the box is reachable on.
func TestNothingIsReclaimedOnAnInterfaceNMCannotAnswerFor(t *testing.T) {
	managed, groups, protected, on, nm := applianceFixture()
	nm.enumerable["enp1s0"] = false

	candidates := reclaimCandidates(managed, groups, protected, on)
	reclaim, skipped := reclaimableIPs(candidates, nm)

	if len(reclaim) != 0 {
		t.Fatalf("reclaim = %v, want nothing: ownership on this interface is unknown", reclaim)
	}
	if len(skipped) == 0 || !strings.Contains(strings.Join(skipped, " "), "cannot be enumerated") {
		t.Errorf("skipped = %v, want a reason an operator can read", skipped)
	}
}

// An interface PulseHA has no group assignment on is not PulseHA's to tidy,
// whatever is on it.
func TestAnUnmanagedInterfaceIsNeverTouched(t *testing.T) {
	managed, groups, protected, _, nm := applianceFixture()
	on := func(iface string) []string {
		if iface == "enp2s0" {
			return []string{"192.168.5.5"}
		}
		return nil
	}
	nm.enumerable["enp2s0"] = true

	candidates := reclaimCandidates(managed, groups, protected, on)
	reclaim, _ := reclaimableIPs(candidates, nm)
	if len(reclaim) != 0 {
		t.Fatalf("reclaim = %v, want nothing: enp2s0 carries no PulseHA group", reclaim)
	}
}

// An address a group names anywhere in the cluster is accounted for, even if this
// node should not be holding it — that is surplusFloatingIPs' question, answered
// against the expectation set, and answering it twice from two rules is how a
// node ends up fighting itself.
func TestAnAddressConfiguredInAnyGroupIsLeftToTheGroupPaths(t *testing.T) {
	managed, groups, protected, on, nm := applianceFixture()
	groups["SomeOtherGroupOnAnotherNode"] = []string{"10.20.70.78/24"}

	candidates := reclaimCandidates(managed, groups, protected, on)
	reclaim, _ := reclaimableIPs(candidates, nm)
	if len(reclaim) != 0 {
		t.Fatalf("reclaim = %v, want nothing: a group still names that address", reclaim)
	}
}

// A mask disagreement is not a different address. The kernel may hold /24 where
// the config says /32, and #104 is what happens when the two are confused.
func TestOwnershipIgnoresThePrefixLength(t *testing.T) {
	managed, groups, protected, on, nm := applianceFixture()
	groups["Management"] = []string{"10.20.70.67/32"}

	candidates := reclaimCandidates(managed, groups, protected, on)
	reclaim, _ := reclaimableIPs(candidates, nm)
	if slices.Contains(reclaim["enp1s0"], "10.20.70.67") {
		t.Error("a configured address was reclaimed because its mask differs from the kernel's")
	}
}

func TestScopesThatAreNeverFloatingIPs(t *testing.T) {
	managed := map[string][]string{"enp1s0": {"Management"}}
	on := func(string) []string {
		return []string{"127.0.0.1", "169.254.3.4", "fe80::1", "224.0.0.1", "0.0.0.0", "10.0.0.9"}
	}

	candidates := reclaimCandidates(managed, nil, nil, on)
	if !slices.Equal(candidates["enp1s0"], []string{"10.0.0.9"}) {
		t.Fatalf("candidates = %v, want only the global unicast address", candidates["enp1s0"])
	}
}

// A converged node must not shell out to nmcli on every enforce tick, which is
// what computing candidates before consulting NetworkManager buys.
func TestAConvergedNodeProducesNoCandidates(t *testing.T) {
	managed, groups, protected, _, _ := applianceFixture()
	on := func(string) []string { return []string{"10.20.70.21", "10.20.70.67"} }

	if candidates := reclaimCandidates(managed, groups, protected, on); len(candidates) != 0 {
		t.Fatalf("candidates = %v, want none: every address is accounted for", candidates)
	}
}
