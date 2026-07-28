// Package ipam provides pure IP placement calculations for floating IP
// groups. It holds no cluster runtime state; callers snapshot member state
// into Node values, call the placement functions, and apply the results.
package ipam

import "sort"

// Node is a point-in-time snapshot of a cluster member used for
// placement decisions.
type Node struct {
	Hostname string
	IPCount  int // floating IPs currently hosted by the node
	Capacity int // maximum floating IPs the node may host; 0 = unlimited

	// Groups are the floating IP groups the node may host — the groups mapped
	// to one of its interfaces. A nil map means unrestricted.
	//
	// Placement has to know this. A node the group is not assigned to cannot
	// bring its addresses up: the assign is refused with "group G not assigned
	// to any interface on node N", and both callers of this package treat that
	// as a dead end rather than trying elsewhere. Choosing such a node stranded
	// 61 addresses cluster-wide behind a passing quorum vote
	// (docs/TEST-PLAN.md defect #40).
	Groups map[string]bool

	// Held counts the addresses the node currently hosts per group. Only move
	// planning needs it, to find a group the destination can accept. A nil map
	// means the caller does not track groups, and the node's whole count is
	// treated as one unnamed group that only unrestricted nodes can host.
	Held map[string]int
}

// HasCapacity reports whether the node can accept at least one more IP.
func (n Node) HasCapacity() bool {
	return n.Capacity == 0 || n.IPCount < n.Capacity
}

// CanHost reports whether the node may host an address of the given group.
func (n Node) CanHost(group string) bool {
	return n.Groups == nil || n.Groups[group]
}

// IP is an address to place, together with the group it belongs to. The group
// decides which nodes are eligible to host it.
type IP struct {
	Addr  string
	Group string
}

// Distribute assigns ips across nodes least-loaded-first, respecting each
// node's capacity and group eligibility. Ties break by node order, so callers
// must pass nodes in a deterministic order (sorted by ID) for stable placement.
// It returns the assignments keyed by hostname plus any addresses that could
// not be placed because no eligible node had capacity.
//
// Load is shared across groups: a node's count is every floating IP it holds,
// whichever group it came from, so an eligible-but-loaded node still takes an
// address when it is the only node that can host the group.
func Distribute(ips []IP, nodes []Node) (map[string][]string, []string) {
	assignments := make(map[string][]string)
	var unplaced []string

	if len(nodes) == 0 {
		for _, ip := range ips {
			unplaced = append(unplaced, ip.Addr)
		}
		return assignments, unplaced
	}

	counts := make([]int, len(nodes))
	for i, node := range nodes {
		counts[i] = node.IPCount
	}

	for _, ip := range ips {
		target := -1
		for i, node := range nodes {
			if !node.CanHost(ip.Group) {
				continue
			}
			if node.Capacity > 0 && counts[i] >= node.Capacity {
				continue
			}
			if target == -1 || counts[i] < counts[target] {
				target = i
			}
		}

		if target == -1 {
			unplaced = append(unplaced, ip.Addr)
			continue
		}

		assignments[nodes[target].Hostname] = append(assignments[nodes[target].Hostname], ip.Addr)
		counts[target]++
	}

	return assignments, unplaced
}

// Move is a batch of IPs of one group to relocate from one node to another. Src
// and Dst are indices into the nodes slice passed to PlanMoves.
type Move struct {
	Src   int
	Dst   int
	Count int
	Group string
}

// PlanMoves returns every move needed to balance the cluster, aggregated per
// source/destination/group triple.
//
// Callers that apply one PlanMove at a time pay a full IP-failover round trip
// per address. Switching an active-passive cluster to active-active leaves the
// whole group on the former sole Active, and draining ~150 addresses one at a
// time took about 27 minutes — long enough that the cluster looked permanently
// unbalanced (docs/TEST-PLAN.md defects #2/#26). Aggregating first lets the
// caller move each batch in a single call.
//
// Balance semantics are PlanMove's, applied to a simulated copy of the nodes,
// so this returns exactly the moves the incremental loop would have made.
// Nodes must be in a deterministic order (sorted by ID) for a stable plan.
func PlanMoves(nodes []Node) []Move {
	sim := cloneNodes(nodes)

	total := 0
	for _, node := range nodes {
		total += node.IPCount
	}

	type batch struct {
		src, dst int
		group    string
	}
	counts := make(map[batch]int)
	var order []batch

	// Every iteration relocates one IP and strictly reduces the imbalance, so
	// the number of hosted IPs bounds the loop.
	for i := 0; i < total; i++ {
		src, dst, group, ok := PlanMove(sim)
		if !ok {
			break
		}
		applyMove(sim, src, dst, group)

		b := batch{src, dst, group}
		if _, seen := counts[b]; !seen {
			order = append(order, b)
		}
		counts[b]++
	}

	moves := make([]Move, 0, len(order))
	for _, b := range order {
		moves = append(moves, Move{Src: b.src, Dst: b.dst, Count: counts[b], Group: b.group})
	}
	return moves
}

// PlanMove returns the next single-IP move that reduces imbalance: the
// source/destination pair with the largest load difference for which the source
// holds a group the destination can host and has capacity. It returns ok=false
// when the assignment is already balanced (max-min <= 1) or no such pair exists.
//
// The most-loaded node is not necessarily a valid source, and the least-loaded
// not necessarily a valid destination: group eligibility can rule either out, in
// which case the next-best pair is used rather than giving up. Ties break by
// node order, so callers must pass nodes in a deterministic order.
func PlanMove(nodes []Node) (src, dst int, group string, ok bool) {
	src, dst, group = -1, -1, ""
	best := 0

	for i := range nodes {
		for j := range nodes {
			if i == j || !nodes[j].HasCapacity() {
				continue
			}
			// A difference of one is already balanced; moving would only
			// invert it.
			delta := nodes[i].IPCount - nodes[j].IPCount
			if delta < 2 || delta <= best {
				continue
			}
			movable, found := movableGroup(nodes[i], nodes[j])
			if !found {
				continue
			}
			src, dst, group, best = i, j, movable, delta
		}
	}

	return src, dst, group, src != -1
}

// movableGroup returns a group src holds that dst is allowed to host, choosing
// the lexicographically smallest for a deterministic plan.
func movableGroup(src, dst Node) (string, bool) {
	held := src.held()
	groups := make([]string, 0, len(held))
	for group := range held {
		groups = append(groups, group)
	}
	sort.Strings(groups)

	for _, group := range groups {
		if held[group] > 0 && dst.CanHost(group) {
			return group, true
		}
	}
	return "", false
}

// held returns the node's per-group counts, treating a nil Held map as one
// unnamed group holding everything.
func (n Node) held() map[string]int {
	if n.Held != nil {
		return n.Held
	}
	return map[string]int{"": n.IPCount}
}

// cloneNodes deep-copies nodes for simulation, materialising Held so a
// simulated move can update it. The Groups map is shared: it is read-only here.
func cloneNodes(nodes []Node) []Node {
	sim := make([]Node, len(nodes))
	copy(sim, nodes)
	for i, node := range nodes {
		held := make(map[string]int, len(node.held()))
		for group, count := range node.held() {
			held[group] = count
		}
		sim[i].Held = held
	}
	return sim
}

// applyMove relocates one address of group from src to dst in a simulated
// nodes slice.
func applyMove(nodes []Node, src, dst int, group string) {
	nodes[src].IPCount--
	nodes[dst].IPCount++
	if nodes[src].Held != nil {
		nodes[src].Held[group]--
	}
	if nodes[dst].Held != nil {
		nodes[dst].Held[group]++
	}
}
