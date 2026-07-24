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
