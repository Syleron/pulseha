package ipam

import (
	"reflect"
	"testing"
)

// ipsIn builds placement inputs for a single group. Tests that do not care
// about group eligibility use the empty group, which every unrestricted node
// can host.
func ipsIn(group string, addrs ...string) []IP {
	out := make([]IP, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, IP{Addr: addr, Group: group})
	}
	return out
}

func hosts(groups ...string) map[string]bool {
	set := make(map[string]bool, len(groups))
	for _, g := range groups {
		set[g] = true
	}
	return set
}

func TestDistributeEvenSpread(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1"},
		{Hostname: "node2"},
	}

	assignments, unplaced := Distribute(ipsIn("", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"), nodes)

	if len(unplaced) != 0 {
		t.Fatalf("expected no unplaced IPs, got %v", unplaced)
	}
	if len(assignments["node1"]) != 2 || len(assignments["node2"]) != 2 {
		t.Fatalf("expected even 2/2 split, got %v", assignments)
	}
}

func TestDistributeFavoursLeastLoaded(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 3},
		{Hostname: "node2", IPCount: 0},
	}

	assignments, _ := Distribute(ipsIn("", "10.0.0.1", "10.0.0.2", "10.0.0.3"), nodes)

	if !reflect.DeepEqual(assignments["node2"], []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}) {
		t.Fatalf("expected all IPs on the empty node, got %v", assignments)
	}
}

func TestDistributeRespectsCapacity(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", Capacity: 1},
		{Hostname: "node2"},
	}

	assignments, unplaced := Distribute(ipsIn("", "10.0.0.1", "10.0.0.2", "10.0.0.3"), nodes)

	if len(unplaced) != 0 {
		t.Fatalf("expected no unplaced IPs, got %v", unplaced)
	}
	if len(assignments["node1"]) != 1 {
		t.Fatalf("expected capped node to hold exactly 1 IP, got %v", assignments["node1"])
	}
	if len(assignments["node2"]) != 2 {
		t.Fatalf("expected overflow on unlimited node, got %v", assignments["node2"])
	}
}

func TestDistributeAllAtCapacity(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 1, Capacity: 1},
		{Hostname: "node2", IPCount: 2, Capacity: 2},
	}

	assignments, unplaced := Distribute(ipsIn("", "10.0.0.1", "10.0.0.2"), nodes)

	if len(assignments) != 0 {
		t.Fatalf("expected no assignments, got %v", assignments)
	}
	if !reflect.DeepEqual(unplaced, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("expected both IPs unplaced, got %v", unplaced)
	}
}

func TestDistributeNoNodes(t *testing.T) {
	assignments, unplaced := Distribute(ipsIn("", "10.0.0.1"), nil)

	if len(assignments) != 0 {
		t.Fatalf("expected no assignments, got %v", assignments)
	}
	if !reflect.DeepEqual(unplaced, []string{"10.0.0.1"}) {
		t.Fatalf("expected all IPs unplaced, got %v", unplaced)
	}
}

func TestDistributeDeterministicTieBreak(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1"},
		{Hostname: "node2"},
	}

	assignments, _ := Distribute(ipsIn("", "10.0.0.1"), nodes)

	if len(assignments["node1"]) != 1 {
		t.Fatalf("expected tie to break to first node, got %v", assignments)
	}
}

// Regression for docs/TEST-PLAN.md defect #40. Placement did not filter
// candidates by group assignment, so the coordinator picked a node the group had
// just been unassigned from, the assign was correctly refused, and the addresses
// were never placed anywhere: 61 addresses down cluster-wide behind a passing
// quorum vote.
func TestDistributeSkipsNodesWithoutTheGroup(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", Groups: hosts("RealTest", "Management")},
		{Hostname: "node2", Groups: hosts("Management")},
		{Hostname: "node3", Groups: hosts("RealTest")},
	}

	assignments, unplaced := Distribute(ipsIn("RealTest", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"), nodes)

	if len(unplaced) != 0 {
		t.Fatalf("expected every IP placed on an eligible node, got unplaced %v", unplaced)
	}
	if len(assignments["node2"]) != 0 {
		t.Errorf("expected no RealTest IPs on the node without the group, got %v", assignments["node2"])
	}
	if len(assignments["node1"]) != 2 || len(assignments["node3"]) != 2 {
		t.Errorf("expected an even split across the two eligible nodes, got %v", assignments)
	}
}

func TestDistributeUnplacedWhenNoNodeHostsTheGroup(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", Groups: hosts("Management")},
		{Hostname: "node2", Groups: hosts("Management")},
	}

	assignments, unplaced := Distribute(ipsIn("RealTest", "10.0.0.1", "10.0.0.2"), nodes)

	if len(assignments) != 0 {
		t.Fatalf("expected no assignments, got %v", assignments)
	}
	if !reflect.DeepEqual(unplaced, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("expected both IPs unplaced, got %v", unplaced)
	}
}

// Load is shared across groups: a node's count is every floating IP it holds,
// whichever group it came from. An eligible-but-loaded node still takes the
// address when it is the only node that can host the group.
func TestDistributeBalancesAcrossGroupsByTotalLoad(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 10, Groups: hosts("RealTest", "Management")},
		{Hostname: "node2", IPCount: 0, Groups: hosts("Management")},
	}

	assignments, unplaced := Distribute([]IP{
		{Addr: "10.0.0.1", Group: "RealTest"},
		{Addr: "10.0.1.1", Group: "Management"},
	}, nodes)

	if len(unplaced) != 0 {
		t.Fatalf("expected no unplaced IPs, got %v", unplaced)
	}
	if !reflect.DeepEqual(assignments["node1"], []string{"10.0.0.1"}) {
		t.Errorf("expected the RealTest IP on its only eligible node, got %v", assignments["node1"])
	}
	if !reflect.DeepEqual(assignments["node2"], []string{"10.0.1.1"}) {
		t.Errorf("expected the Management IP on the least-loaded eligible node, got %v", assignments["node2"])
	}
}

func TestDistributeMixedGroupsRespectsCapacity(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 1, Capacity: 1, Groups: hosts("RealTest")},
		{Hostname: "node2", Groups: hosts("RealTest")},
	}

	assignments, unplaced := Distribute(ipsIn("RealTest", "10.0.0.1", "10.0.0.2"), nodes)

	if len(unplaced) != 0 {
		t.Fatalf("expected no unplaced IPs, got %v", unplaced)
	}
	if len(assignments["node1"]) != 0 {
		t.Errorf("expected nothing on the node at capacity, got %v", assignments["node1"])
	}
	if len(assignments["node2"]) != 2 {
		t.Errorf("expected both IPs on the node with capacity, got %v", assignments["node2"])
	}
}

func TestPlanMoveBalancesLoad(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 3},
		{Hostname: "node2", IPCount: 0},
	}

	src, dst, _, ok := PlanMove(nodes)

	if !ok {
		t.Fatal("expected a move to be planned")
	}
	if src != 0 || dst != 1 {
		t.Fatalf("expected move from node1 to node2, got src=%d dst=%d", src, dst)
	}
}

func TestPlanMoveAlreadyBalanced(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 2},
		{Hostname: "node2", IPCount: 1},
	}

	if _, _, _, ok := PlanMove(nodes); ok {
		t.Fatal("expected no move when max-min <= 1")
	}
}

func TestPlanMoveSkipsDestinationAtCapacity(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 4},
		{Hostname: "node2", IPCount: 1, Capacity: 1},
		{Hostname: "node3", IPCount: 2},
	}

	src, dst, _, ok := PlanMove(nodes)

	if !ok {
		t.Fatal("expected a move to be planned")
	}
	if src != 0 || dst != 2 {
		t.Fatalf("expected move from node1 to node3 (node2 at capacity), got src=%d dst=%d", src, dst)
	}
}

func TestPlanMoveNoEligibleDestination(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 4},
		{Hostname: "node2", IPCount: 1, Capacity: 1},
	}

	if _, _, _, ok := PlanMove(nodes); ok {
		t.Fatal("expected no move when the only under-loaded node is at capacity")
	}
}

// Regression for docs/TEST-PLAN.md defect #40. Rebalancing is the other
// direction of the same bug: an empty node that cannot host the group the
// over-loaded node holds is not a valid destination. Choosing it made
// OrchestrateIPFailover fail, and rebalanceActiveActive breaks out of its loop
// on error — so one ineligible node stalled all rebalancing.
func TestPlanMoveSkipsDestinationWithoutTheGroup(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 4, Groups: hosts("RealTest"), Held: map[string]int{"RealTest": 4}},
		{Hostname: "node2", IPCount: 0, Groups: hosts("Management")},
	}

	if _, _, _, ok := PlanMove(nodes); ok {
		t.Fatal("expected no move when the destination cannot host the source's group")
	}
}

func TestPlanMovePicksAGroupTheDestinationCanHost(t *testing.T) {
	nodes := []Node{
		{
			Hostname: "node1", IPCount: 4,
			Groups: hosts("RealTest", "Management"),
			Held:   map[string]int{"RealTest": 3, "Management": 1},
		},
		{Hostname: "node2", IPCount: 0, Groups: hosts("Management")},
	}

	src, dst, group, ok := PlanMove(nodes)

	if !ok {
		t.Fatal("expected a move to be planned")
	}
	if src != 0 || dst != 1 {
		t.Fatalf("expected move from node1 to node2, got src=%d dst=%d", src, dst)
	}
	if group != "Management" {
		t.Fatalf("expected the group the destination can host, got %q", group)
	}
}

// The most-loaded node is not always a valid source: if nothing it holds can go
// to the under-loaded node, the next-most-loaded node that can must be used
// instead of giving up.
func TestPlanMoveFallsBackToAnEligibleSource(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 6, Groups: hosts("RealTest"), Held: map[string]int{"RealTest": 6}},
		{Hostname: "node2", IPCount: 4, Groups: hosts("Management"), Held: map[string]int{"Management": 4}},
		{Hostname: "node3", IPCount: 0, Groups: hosts("Management")},
	}

	src, dst, group, ok := PlanMove(nodes)

	if !ok {
		t.Fatal("expected a move to be planned from the eligible source")
	}
	if src != 1 || dst != 2 {
		t.Fatalf("expected move from node2 to node3, got src=%d dst=%d", src, dst)
	}
	if group != "Management" {
		t.Fatalf("expected group Management, got %q", group)
	}
}

// PlanMoves must agree with the incremental loop it replaces: applying it once
// has to leave the cluster in the same state as calling PlanMove until it stops.
func TestPlanMovesMatchesIncrementalPlanMove(t *testing.T) {
	cases := [][]Node{
		{{Hostname: "node1", IPCount: 200}, {Hostname: "node2"}, {Hostname: "node3"}, {Hostname: "node4"}},
		{{Hostname: "node1", IPCount: 5}, {Hostname: "node2", IPCount: 4}},
		{{Hostname: "node1", IPCount: 7}, {Hostname: "node2", IPCount: 1, Capacity: 2}, {Hostname: "node3"}},
		{{Hostname: "node1", IPCount: 3}, {Hostname: "node2", IPCount: 3}},
		{
			{Hostname: "node1", IPCount: 9, Groups: hosts("RealTest", "Management"),
				Held: map[string]int{"RealTest": 6, "Management": 3}},
			{Hostname: "node2", IPCount: 0, Groups: hosts("Management")},
			{Hostname: "node3", IPCount: 0, Groups: hosts("RealTest", "Management")},
		},
	}

	for _, nodes := range cases {
		batched := make([]int, len(nodes))
		incremental := make([]int, len(nodes))
		for i, node := range nodes {
			batched[i], incremental[i] = node.IPCount, node.IPCount
		}

		for _, move := range PlanMoves(nodes) {
			batched[move.Src] -= move.Count
			batched[move.Dst] += move.Count
		}

		sim := cloneNodes(nodes)
		for {
			src, dst, group, ok := PlanMove(sim)
			if !ok {
				break
			}
			applyMove(sim, src, dst, group)
			incremental[src]--
			incremental[dst]++
		}

		if !reflect.DeepEqual(batched, incremental) {
			t.Errorf("PlanMoves gave %v, incremental PlanMove gave %v for %v", batched, incremental, nodes)
		}
	}
}

func TestPlanMovesBatchesPerDestination(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 200},
		{Hostname: "node2"},
		{Hostname: "node3"},
		{Hostname: "node4"},
	}

	moves := PlanMoves(nodes)

	if len(moves) != 3 {
		t.Fatalf("expected one batch per destination, got %d: %+v", len(moves), moves)
	}
	for _, move := range moves {
		if move.Src != 0 || move.Count != 50 {
			t.Errorf("expected 50 IPs from node1 per destination, got %+v", move)
		}
	}
}

// A batch carries the group its addresses come from, so the caller can pick the
// right addresses off the source instead of taking an arbitrary tail.
func TestPlanMovesBatchesPerGroup(t *testing.T) {
	nodes := []Node{
		{
			Hostname: "node1", IPCount: 8,
			Groups: hosts("RealTest", "Management"),
			Held:   map[string]int{"RealTest": 4, "Management": 4},
		},
		{Hostname: "node2", IPCount: 0, Groups: hosts("Management")},
		{Hostname: "node3", IPCount: 0, Groups: hosts("RealTest", "Management")},
	}

	moves := PlanMoves(nodes)

	byGroup := make(map[string]int)
	for _, move := range moves {
		if move.Src != 0 {
			t.Errorf("expected every batch to come off node1, got %+v", move)
		}
		if move.Group == "" {
			t.Errorf("expected a batch to name its group, got %+v", move)
		}
		if nodes[move.Dst].Groups[move.Group] != true {
			t.Errorf("batch %+v sends group %q to a node that cannot host it", move, move.Group)
		}
		byGroup[move.Group] += move.Count
	}
	// node2 can only take Management, so it can only be filled from those 4.
	if byGroup["Management"] == 0 {
		t.Errorf("expected Management addresses to be moved, got %v", byGroup)
	}
}

func TestPlanMovesAlreadyBalanced(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 2},
		{Hostname: "node2", IPCount: 1},
	}

	if moves := PlanMoves(nodes); len(moves) != 0 {
		t.Fatalf("expected no moves when max-min <= 1, got %+v", moves)
	}
}

// A node that cannot host anything the cluster needs moved must not make the
// planner loop forever trying to fill it.
func TestPlanMovesTerminatesWithNoEligibleDestination(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 10, Groups: hosts("RealTest"), Held: map[string]int{"RealTest": 10}},
		{Hostname: "node2", IPCount: 0, Groups: hosts("Management")},
	}

	if moves := PlanMoves(nodes); len(moves) != 0 {
		t.Fatalf("expected no moves when the only destination cannot host the group, got %+v", moves)
	}
}

func TestHasCapacity(t *testing.T) {
	if !(Node{IPCount: 100}).HasCapacity() {
		t.Fatal("capacity 0 should mean unlimited")
	}
	if (Node{IPCount: 2, Capacity: 2}).HasCapacity() {
		t.Fatal("node at capacity should report no capacity")
	}
	if !(Node{IPCount: 1, Capacity: 2}).HasCapacity() {
		t.Fatal("node under capacity should report capacity")
	}
}

func TestCanHost(t *testing.T) {
	if !(Node{}).CanHost("RealTest") {
		t.Fatal("a node with no declared groups should be unrestricted")
	}
	if !(Node{Groups: hosts("RealTest")}).CanHost("RealTest") {
		t.Fatal("a node declaring the group should host it")
	}
	if (Node{Groups: hosts("Management")}).CanHost("RealTest") {
		t.Fatal("a node not declaring the group must not host it")
	}
}
