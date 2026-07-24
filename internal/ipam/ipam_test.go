package ipam

import (
	"reflect"
	"testing"
)

func TestDistributeEvenSpread(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1"},
		{Hostname: "node2"},
	}

	assignments, unplaced := Distribute([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}, nodes)

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

	assignments, _ := Distribute([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nodes)

	if !reflect.DeepEqual(assignments["node2"], []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}) {
		t.Fatalf("expected all IPs on the empty node, got %v", assignments)
	}
}

func TestDistributeRespectsCapacity(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", Capacity: 1},
		{Hostname: "node2"},
	}

	assignments, unplaced := Distribute([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nodes)

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

	assignments, unplaced := Distribute([]string{"10.0.0.1", "10.0.0.2"}, nodes)

	if len(assignments) != 0 {
		t.Fatalf("expected no assignments, got %v", assignments)
	}
	if !reflect.DeepEqual(unplaced, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("expected both IPs unplaced, got %v", unplaced)
	}
}

func TestDistributeNoNodes(t *testing.T) {
	ips := []string{"10.0.0.1"}
	assignments, unplaced := Distribute(ips, nil)

	if len(assignments) != 0 {
		t.Fatalf("expected no assignments, got %v", assignments)
	}
	if !reflect.DeepEqual(unplaced, ips) {
		t.Fatalf("expected all IPs unplaced, got %v", unplaced)
	}
}

func TestDistributeDeterministicTieBreak(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1"},
		{Hostname: "node2"},
	}

	assignments, _ := Distribute([]string{"10.0.0.1"}, nodes)

	if len(assignments["node1"]) != 1 {
		t.Fatalf("expected tie to break to first node, got %v", assignments)
	}
}

func TestPlanMoveBalancesLoad(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 3},
		{Hostname: "node2", IPCount: 0},
	}

	src, dst, ok := PlanMove(nodes)

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

	if _, _, ok := PlanMove(nodes); ok {
		t.Fatal("expected no move when max-min <= 1")
	}
}

func TestPlanMoveSkipsDestinationAtCapacity(t *testing.T) {
	nodes := []Node{
		{Hostname: "node1", IPCount: 4},
		{Hostname: "node2", IPCount: 1, Capacity: 1},
		{Hostname: "node3", IPCount: 2},
	}

	src, dst, ok := PlanMove(nodes)

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

	if _, _, ok := PlanMove(nodes); ok {
		t.Fatal("expected no move when the only under-loaded node is at capacity")
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
