// Package ipam provides pure IP placement calculations for floating IP
// groups. It holds no cluster runtime state; callers snapshot member state
// into Node values, call the placement functions, and apply the results.
package ipam

// Node is a point-in-time snapshot of a cluster member used for
// placement decisions.
type Node struct {
	Hostname string
	IPCount  int // floating IPs currently hosted by the node
	Capacity int // maximum floating IPs the node may host; 0 = unlimited
}

// HasCapacity reports whether the node can accept at least one more IP.
func (n Node) HasCapacity() bool {
	return n.Capacity == 0 || n.IPCount < n.Capacity
}

// Distribute assigns ips across nodes least-loaded-first, respecting each
// node's capacity. Ties break by node order, so callers must pass nodes in
// a deterministic order (sorted by ID) for stable placement. It returns the
// assignments keyed by hostname plus any IPs that could not be placed
// because every node was at capacity.
func Distribute(ips []string, nodes []Node) (map[string][]string, []string) {
	assignments := make(map[string][]string)
	if len(nodes) == 0 {
		return assignments, ips
	}

	counts := make([]int, len(nodes))
	for i, node := range nodes {
		counts[i] = node.IPCount
	}

	var unplaced []string
	for _, ip := range ips {
		target := -1
		for i, node := range nodes {
			if node.Capacity > 0 && counts[i] >= node.Capacity {
				continue
			}
			if target == -1 || counts[i] < counts[target] {
				target = i
			}
		}

		if target == -1 {
			unplaced = append(unplaced, ip)
			continue
		}

		assignments[nodes[target].Hostname] = append(assignments[nodes[target].Hostname], ip)
		counts[target]++
	}

	return assignments, unplaced
}

// Move is a batch of IPs to relocate from one node to another. Src and Dst
// are indices into the nodes slice passed to PlanMoves.
type Move struct {
	Src   int
	Dst   int
	Count int
}

// PlanMoves returns every move needed to balance the cluster, aggregated per
// source/destination pair.
//
// Callers that apply one PlanMove at a time pay a full IP-failover round trip
// per address. Switching an active-passive cluster to active-active leaves the
// whole group on the former sole Active, and draining ~150 addresses one at a
// time took about 27 minutes — long enough that the cluster looked permanently
// unbalanced (docs/TEST-PLAN.md defects #2/#26). Aggregating first lets the
// caller move each pair's addresses in a single batch.
//
// Balance semantics are PlanMove's, applied to a simulated copy of the counts,
// so this returns exactly the moves the incremental loop would have made.
// Nodes must be in a deterministic order (sorted by ID) for a stable plan.
func PlanMoves(nodes []Node) []Move {
	sim := make([]Node, len(nodes))
	copy(sim, nodes)

	total := 0
	for _, node := range nodes {
		total += node.IPCount
	}

	type pair struct{ src, dst int }
	counts := make(map[pair]int)
	var order []pair

	// Every iteration relocates one IP and strictly reduces the imbalance, so
	// the number of hosted IPs bounds the loop.
	for i := 0; i < total; i++ {
		src, dst, ok := PlanMove(sim)
		if !ok {
			break
		}
		sim[src].IPCount--
		sim[dst].IPCount++

		p := pair{src, dst}
		if _, seen := counts[p]; !seen {
			order = append(order, p)
		}
		counts[p]++
	}

	moves := make([]Move, 0, len(order))
	for _, p := range order {
		moves = append(moves, Move{Src: p.src, Dst: p.dst, Count: counts[p]})
	}
	return moves
}

// PlanMove returns the indices of the next single-IP move that reduces
// imbalance: from the most-loaded node to the least-loaded node that still
// has capacity. It returns ok=false when the assignment is already balanced
// (max-min <= 1) or no eligible destination exists. Ties break by node
// order, so callers must pass nodes in a deterministic order.
func PlanMove(nodes []Node) (src, dst int, ok bool) {
	src, dst = -1, -1
	srcCount, dstCount := -1, -1
	for i, node := range nodes {
		if node.IPCount > srcCount {
			src, srcCount = i, node.IPCount
		}
		if node.HasCapacity() && (dst == -1 || node.IPCount < dstCount) {
			dst, dstCount = i, node.IPCount
		}
	}
	if src == -1 || dst == -1 || src == dst || srcCount-dstCount < 2 {
		return -1, -1, false
	}
	return src, dst, true
}
