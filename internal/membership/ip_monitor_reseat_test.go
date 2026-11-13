package membership

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/config"
)

// docs/TEST-PLAN.md defect #116: the enforce pass deletes and re-adds a floating
// IP to seat it behind the interface's static. The watcher sees the delete, and
// without a record of it would restore the address with a warning — a false
// alarm on every reseat.

func TestReseatProtectionMatchesWithAndWithoutAMask(t *testing.T) {
	m := &IPMonitor{}
	m.markReseating("enp2s0", []string{"10.10.91.100/24"})

	if !m.reseatSuppressed("enp2s0", "10.10.91.100/24") {
		t.Error("the address as written is not protected")
	}
	if !m.reseatSuppressed("enp2s0", "10.10.91.100") {
		t.Error("a bare address is not matched against a record written in CIDR")
	}
	if m.reseatSuppressed("enp1s0", "10.10.91.100") {
		t.Error("the protection leaked onto another interface")
	}
	if m.reseatSuppressed("enp2s0", "10.10.91.101") {
		t.Error("an address that is not being reseated is protected")
	}
}

func TestReseatProtectionExpires(t *testing.T) {
	m := &IPMonitor{}
	now := time.Now()
	m.now = func() time.Time { return now }

	m.markReseating("enp2s0", []string{"10.10.91.100/24"})
	if !m.reseatSuppressed("enp2s0", "10.10.91.100") {
		t.Fatal("the reseat was not recorded at all")
	}

	now = now.Add(reseatGraceWindow + time.Second)
	if m.reseatSuppressed("enp2s0", "10.10.91.100") {
		t.Error("the protection outlived its window; a genuinely lost address would stay down")
	}
	if _, still := m.reseatingIPs["enp2s0"]; still {
		t.Error("an expired record was not dropped")
	}
}

func TestReseatProtectionIsClearedForAnAddressThatStayedDown(t *testing.T) {
	// A reseat whose add failed leaves the address genuinely down, and the
	// watcher must be free to put it back at once.
	m := &IPMonitor{}
	m.markReseating("enp2s0", []string{"10.10.91.100/24", "10.10.91.101/24"})

	m.clearReseating("enp2s0", []string{"10.10.91.100"})
	if m.reseatSuppressed("enp2s0", "10.10.91.100/24") {
		t.Error("a cleared address is still protected")
	}
	if !m.reseatSuppressed("enp2s0", "10.10.91.101/24") {
		t.Error("clearing one address dropped the other's protection")
	}

	m.clearReseating("enp2s0", []string{"10.10.91.101"})
	if _, still := m.reseatingIPs["enp2s0"]; still {
		t.Error("an interface with nothing left protected keeps an empty record")
	}
}

func TestReseatProtectionOnANilMonitorIsOff(t *testing.T) {
	var m *IPMonitor
	m.markReseating("enp2s0", []string{"10.10.91.100"})
	m.clearReseating("enp2s0", []string{"10.10.91.100"})
	if m.reseatSuppressed("enp2s0", "10.10.91.100") {
		t.Error("a nil monitor protects nothing")
	}
}

func TestClusterFloatingIPsByInterfaceIsTheWholeConfiguredSet(t *testing.T) {
	cfg := &config.Config{
		Groups: map[string][]string{
			"web":  {"10.10.91.100/24", "10.10.91.101/24"},
			"mail": {"10.10.90.212/24"},
			"idle": {"192.0.2.1/24"},
		},
		Nodes: map[string]*config.Node{
			"local": {IPGroups: map[string][]string{
				"enp2s0": {"web"},
				"enp1s0": {"mail", "missing-group"},
			}},
		},
	}

	got := clusterFloatingIPsByInterface(cfg, "local")

	want := map[string]string{
		"enp2s0": "10.10.91.100/24,10.10.91.101/24",
		"enp1s0": "10.10.90.212/24",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want interfaces %v", got, want)
	}
	for iface, ips := range want {
		have := append([]string(nil), got[iface]...)
		sort.Strings(have)
		if strings.Join(have, ",") != ips {
			t.Errorf("%s: got %v, want %s", iface, have, ips)
		}
	}
}

func TestClusterFloatingIPsByInterfaceForAnUnknownNodeIsEmpty(t *testing.T) {
	cfg := &config.Config{Groups: map[string][]string{"web": {"10.10.91.100/24"}}}
	if got := clusterFloatingIPsByInterface(cfg, "nobody"); len(got) != 0 {
		t.Errorf("got %v, want nothing for a node the config has no entry for", got)
	}
	if got := clusterFloatingIPsByInterface(nil, "local"); got == nil || len(got) != 0 {
		t.Errorf("got %v, want an empty (not nil) map for a nil config", got)
	}
}
