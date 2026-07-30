package ipam

import (
	"reflect"
	"testing"
)

// capacityTopologies are the shapes where a destination is also a source, which is
// what lets an aggregated batch overfill it. A node that must shed before it can
// accept beyond its capacity has its whole incoming batch applied first once the
// moves are merged by (src, dst, group).
func capacityTopologies() map[string][]Node {
	return map[string][]Node{
		"destination must shed before it can accept": {
			{Hostname: "node1", IPCount: 9, Groups: hosts("RealTest", "Management"),
				Held: map[string]int{"RealTest": 9}},
			{Hostname: "node2", IPCount: 3, Capacity: 4, Groups: hosts("RealTest", "Management"),
				Held: map[string]int{"Management": 3}},
			{Hostname: "node3", IPCount: 0, Capacity: 8, Groups: hosts("Management")},
		},
		"tight capacity on every node": {
			{Hostname: "node1", IPCount: 8, Capacity: 8, Groups: hosts("RealTest", "Management"),
				Held: map[string]int{"RealTest": 5, "Management": 3}},
			{Hostname: "node2", IPCount: 2, Capacity: 4, Groups: hosts("RealTest", "Management"),
				Held: map[string]int{"RealTest": 2}},
			{Hostname: "node3", IPCount: 1, Capacity: 5, Groups: hosts("Management"),
				Held: map[string]int{"Management": 1}},
		},
		"capped destination fed by a capped source": {
			{Hostname: "node1", IPCount: 7, Capacity: 7},
			{Hostname: "node2", IPCount: 1, Capacity: 2},
			{Hostname: "node3", IPCount: 0, Capacity: 6},
		},
	}
}

// Applying a batch is not atomic across the plan: the caller brings a batch down on
// the source and up on the destination before moving to the next. So no prefix of
// the plan may leave a node holding more than its capacity, even though the final
// state is correct either way.
func TestPlanMovesNeverTransientlyExceedsCapacity(t *testing.T) {
	for name, nodes := range capacityTopologies() {
		t.Run(name, func(t *testing.T) {
			live := make([]int, len(nodes))
			for i, node := range nodes {
				live[i] = node.IPCount
			}

			for step, move := range PlanMoves(nodes) {
				live[move.Src] -= move.Count
				live[move.Dst] += move.Count

				dst := nodes[move.Dst]
				if dst.Capacity > 0 && live[move.Dst] > dst.Capacity {
					t.Errorf("after step %d (%+v), %s holds %d with capacity %d",
						step, move, dst.Hostname, live[move.Dst], dst.Capacity)
				}
				if live[move.Src] < 0 {
					t.Errorf("after step %d (%+v), %s holds %d",
						step, move, nodes[move.Src].Hostname, live[move.Src])
				}
			}
		})
	}
}

// Scheduling must not change where the addresses end up — it only reorders and
// splits. Checked against the incremental PlanMove loop, the same invariant the
// existing suite pins for the uncapped topologies.
func TestPlanMovesSchedulingPreservesTheFinalState(t *testing.T) {
	for name, nodes := range capacityTopologies() {
		t.Run(name, func(t *testing.T) {
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
				t.Errorf("batched gave %v, incremental gave %v", batched, incremental)
			}
		})
	}
}

// The scheduler must not fragment plans it has no reason to touch: an uncapped
// drain is where batching earns its keep, turning ~150 sequential IP failovers
// into one call per destination.
func TestPlanMovesKeepsBatchesWhereCapacityIsUnbounded(t *testing.T) {
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
		if move.Count != 50 {
			t.Errorf("expected batches of 50, got %+v", move)
		}
	}
}
