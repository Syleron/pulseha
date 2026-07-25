package membership

import (
	"io"
	"reflect"
	"sort"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

func newAATestConfig(groups map[string][]string) *config.Config {
	return &config.Config{
		Pulse:  config.Local{Mode: "active-active", LocalNode: "node-a"},
		Groups: groups,
		Nodes:  map[string]*config.Node{},
	}
}

func newAATestMember(id, hostname string, status MemberStatus, ips []string) *Member {
	m := NewMember(id, hostname, nil, log.New(io.Discard))
	m.Status = status
	m.ActiveIPs = ips
	return m
}

func newAATestMemberList(cfg *config.Config, members ...*Member) *MemberList {
	ml := NewMemberList(cfg, log.New(io.Discard))
	for _, m := range members {
		m.config = cfg
		ml.Members[m.ID] = m
	}
	return ml
}

func TestCalculateIPDistributionEvenSpread(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24", "10.0.0.5/24", "10.0.0.6/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, nil)
	b := newAATestMember("node-b", "host-b", StatusActive, nil)
	c := newAATestMember("node-c", "host-c", StatusActive, nil)
	ml := newAATestMemberList(cfg, a, b, c)

	dist := ml.calculateIPDistribution(cfg.Groups["group1"], []*Member{a, b, c})

	total := 0
	for _, host := range []string{"host-a", "host-b", "host-c"} {
		got := len(dist[host])
		if got != 2 {
			t.Errorf("expected 2 IPs on %s, got %d (%v)", host, got, dist[host])
		}
		total += got
	}
	if total != 6 {
		t.Errorf("expected all 6 IPs distributed, got %d", total)
	}
}

// Regression: with a single group holding more IPs than there are groups, the
// old "unlimited" capacity sentinel (len(Groups)+1) silently dropped IPs.
func TestCalculateIPDistributionMoreIPsThanGroups(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24", "10.0.0.5/24", "10.0.0.6/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, nil)
	b := newAATestMember("node-b", "host-b", StatusActive, nil)
	ml := newAATestMemberList(cfg, a, b)

	dist := ml.calculateIPDistribution(cfg.Groups["group1"], []*Member{a, b})

	if total := len(dist["host-a"]) + len(dist["host-b"]); total != 6 {
		t.Errorf("expected all 6 IPs distributed across 2 nodes, got %d: %v", total, dist)
	}
}

func TestCalculateIPDistributionAccountsForExistingIPs(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, nil)
	ml := newAATestMemberList(cfg, a, b)

	dist := ml.calculateIPDistribution([]string{"10.0.0.3/24", "10.0.0.4/24"}, []*Member{a, b})

	if len(dist["host-a"]) != 0 {
		t.Errorf("expected loaded node-a to receive nothing, got %v", dist["host-a"])
	}
	if !reflect.DeepEqual(dist["host-b"], []string{"10.0.0.3/24", "10.0.0.4/24"}) {
		t.Errorf("expected empty node-b to receive both IPs, got %v", dist["host-b"])
	}
}

func TestCalculateIPDistributionRespectsCapacity(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	a.Capacity = 1
	b := newAATestMember("node-b", "host-b", StatusActive, nil)
	ml := newAATestMemberList(cfg, a, b)

	dist := ml.calculateIPDistribution([]string{"10.0.0.2/24", "10.0.0.3/24"}, []*Member{a, b})

	if len(dist["host-a"]) != 0 {
		t.Errorf("expected at-capacity node-a to receive nothing, got %v", dist["host-a"])
	}
	if len(dist["host-b"]) != 2 {
		t.Errorf("expected node-b to receive both IPs, got %v", dist["host-b"])
	}
}

func TestPlanRebalanceMove(t *testing.T) {
	t.Run("moves from most to least loaded", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, nil)
		src, dst, ip := planRebalanceMove([]*Member{a, b})
		if src != a || dst != b {
			t.Fatalf("expected move from node-a to node-b, got src=%v dst=%v", src, dst)
		}
		if ip != "10.0.0.3/24" {
			t.Errorf("expected last IP of source to move, got %s", ip)
		}
	})

	t.Run("balanced cluster plans no move", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.3/24"})
		if src, _, _ := planRebalanceMove([]*Member{a, b}); src != nil {
			t.Errorf("expected no move for max-min diff of 1, got move from %s", src.Hostname)
		}
	})

	t.Run("skips destinations at capacity", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.4/24"})
		b.Capacity = 1
		if src, _, _ := planRebalanceMove([]*Member{a, b}); src != nil {
			t.Errorf("expected no move when only destination is at capacity, got move from %s", src.Hostname)
		}
	})

	t.Run("single node plans no move", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})
		if src, _, _ := planRebalanceMove([]*Member{a}); src != nil {
			t.Error("expected no move with a single node")
		}
	})
}

func TestClusterCoordinator(t *testing.T) {
	members := map[string]*Member{
		"node-c": newAATestMember("node-c", "host-c", StatusActive, nil),
		"node-a": newAATestMember("node-a", "host-a", StatusUnknown, nil),
		"node-b": newAATestMember("node-b", "host-b", StatusPassive, nil),
		"node-d": newAATestMember("node-d", "host-d", StatusMaintenance, nil),
	}
	if got := clusterCoordinator(members); got != "node-b" {
		t.Errorf("expected lowest healthy node-b as coordinator, got %q", got)
	}

	if got := clusterCoordinator(map[string]*Member{
		"node-a": newAATestMember("node-a", "host-a", StatusUnknown, nil),
	}); got != "" {
		t.Errorf("expected no coordinator with no healthy nodes, got %q", got)
	}
}

func TestOrphanedGroupIPs(t *testing.T) {
	groups := map[string][]string{
		"group1": {"10.0.0.2/24", "10.0.0.1/24"},
		"group2": {"10.0.1.1/24"},
	}
	hosted := map[string]bool{"10.0.0.1/24": true}

	got := orphanedGroupIPs(groups, hosted)
	want := []string{"10.0.0.2/24", "10.0.1.1/24"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected orphaned IPs %v, got %v", want, got)
	}

	if got := orphanedGroupIPs(groups, map[string]bool{
		"10.0.0.1/24": true, "10.0.0.2/24": true, "10.0.1.1/24": true,
	}); len(got) != 0 {
		t.Errorf("expected no orphans when everything is hosted, got %v", got)
	}
}

// Regression: redistribution must merge with a node's existing assignments.
// MakeActive's replace semantics used to wipe them.
func TestRedistributeIPsMergesExistingAssignments(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, nil)
	ml := newAATestMemberList(cfg, a, b)

	// Bring-up will fail (no node configs / no network in unit tests) but the
	// assignment bookkeeping must still be recorded before that.
	if err := ml.RedistributeIPs([]string{"10.0.0.2/24", "10.0.0.3/24"}); err != nil {
		t.Fatalf("RedistributeIPs returned error: %v", err)
	}

	var all []string
	all = append(all, a.ActiveIPs...)
	all = append(all, b.ActiveIPs...)
	sort.Strings(all)
	want := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("expected all IPs tracked across nodes after redistribution, got a=%v b=%v", a.ActiveIPs, b.ActiveIPs)
	}
	if len(a.ActiveIPs) == 0 || a.ActiveIPs[0] != "10.0.0.1/24" {
		t.Errorf("expected node-a to keep its existing IP 10.0.0.1/24, got %v", a.ActiveIPs)
	}
}

func TestResolveDuplicateAssignments(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})
	ml := newAATestMemberList(cfg, a, b)
	h := NewHealthChecker(ml, log.New(io.Discard))

	resolved := h.resolveDuplicateAssignments(ml.Members)

	if !resolved {
		t.Error("expected duplicate to be reported as resolved")
	}
	// Lowest node ID keeps the IP; the other loses it.
	if !reflect.DeepEqual(a.ActiveIPs, []string{"10.0.0.1/24", "10.0.0.2/24"}) {
		t.Errorf("expected node-a to keep both IPs, got %v", a.ActiveIPs)
	}
	if len(b.ActiveIPs) != 0 {
		t.Errorf("expected duplicate removed from node-b, got %v", b.ActiveIPs)
	}

	if h.resolveDuplicateAssignments(ml.Members) {
		t.Error("expected no duplicates on second pass")
	}
}

func TestRemoveIPFromList(t *testing.T) {
	got := removeIPFromList([]string{"a", "b", "c"}, "b")
	if !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("expected [a c], got %v", got)
	}
	if got := removeIPFromList(nil, "a"); len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

// Regression: capacity must not exclude nodes from redistribution
// eligibility. In active-passive mode all IPs go to one node regardless of
// capacity; in active-active mode ipam.Distribute enforces capacity during
// placement. Filtering here made active-passive failover error with "no
// available nodes" once capacities were set.
func TestGetAvailableNodesIgnoresCapacity(t *testing.T) {
	cfg := newAATestConfig(nil)
	cfg.Pulse.Mode = "active-passive"
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	a.Capacity = 1 // at capacity
	b := newAATestMember("node-b", "host-b", StatusPassive, []string{"10.0.0.2/24"})
	b.Capacity = 1 // at capacity
	ml := newAATestMemberList(cfg, a, b)

	nodes := ml.getAvailableNodes()

	if len(nodes) != 2 {
		t.Fatalf("expected both at-capacity nodes to remain eligible, got %d", len(nodes))
	}
}

func TestAddMemberSeedsCapacityFromConfig(t *testing.T) {
	cfg := newAATestConfig(nil)
	cfg.Nodes["node-a"] = &config.Node{Hostname: "host-a", Capacity: 3}
	ml := newAATestMemberList(cfg)

	if err := ml.AddMember("node-a", "host-a", "127.0.0.1", "1234"); err != nil {
		t.Fatalf("AddMember failed: %v", err)
	}

	if got := ml.Members["node-a"].Capacity; got != 3 {
		t.Errorf("expected capacity 3 seeded from config, got %d", got)
	}
}

// ConsolidationTarget picks the node that keeps every floating IP when the
// cluster switches from active-active to active-passive.
func TestConsolidationTarget(t *testing.T) {
	members := func(ms ...*Member) map[string]*Member {
		out := make(map[string]*Member, len(ms))
		for _, m := range ms {
			out[m.ID] = m
		}
		return out
	}

	t.Run("prefers the most loaded active node", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24", "10.0.0.3/24"})
		c := newAATestMember("node-c", "host-c", StatusPassive, nil)

		if got := ConsolidationTarget(members(a, b, c), ""); got != b {
			t.Errorf("expected most-loaded node-b, got %v", got)
		}
	})

	t.Run("breaks load ties on lowest node ID", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})

		// Run repeatedly: the selection must not depend on map iteration order.
		for i := 0; i < 20; i++ {
			if got := ConsolidationTarget(members(a, b), ""); got != a {
				t.Fatalf("expected lowest-ID node-a on tie, got %v", got)
			}
		}
	})

	t.Run("never selects a failed or maintenance node", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusUnknown, []string{"10.0.0.1/24"})
		b := newAATestMember("node-b", "host-b", StatusMaintenance, nil)
		c := newAATestMember("node-c", "host-c", StatusActive, nil)

		if got := ConsolidationTarget(members(a, b, c), "node-a"); got != c {
			t.Errorf("expected healthy node-c, got %v", got)
		}
	})

	t.Run("falls back to the healthy leader when no node is active", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusPassive, nil)
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)

		if got := ConsolidationTarget(members(a, b), "node-b"); got != b {
			t.Errorf("expected leader node-b, got %v", got)
		}
	})

	t.Run("falls back to the lowest-ID healthy node", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusPassive, nil)
		b := newAATestMember("node-b", "host-b", StatusPassive, nil)

		if got := ConsolidationTarget(members(a, b), "node-gone"); got != a {
			t.Errorf("expected lowest-ID node-a, got %v", got)
		}
	})

	t.Run("returns nil when no node is healthy", func(t *testing.T) {
		a := newAATestMember("node-a", "host-a", StatusUnknown, nil)
		b := newAATestMember("node-b", "host-b", StatusMaintenance, nil)

		if got := ConsolidationTarget(members(a, b), "node-a"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}

func TestUpdateConfigRefreshesCapacity(t *testing.T) {
	cfg := newAATestConfig(nil)
	cfg.Nodes["node-a"] = &config.Node{Hostname: "host-a"}
	a := newAATestMember("node-a", "host-a", StatusActive, nil)
	ml := newAATestMemberList(cfg, a)

	newCfg := newAATestConfig(nil)
	newCfg.Nodes["node-a"] = &config.Node{Hostname: "host-a", Capacity: 2}
	ml.UpdateConfig(newCfg)

	if got := a.Capacity; got != 2 {
		t.Errorf("expected capacity refreshed to 2 after config sync, got %d", got)
	}
}
