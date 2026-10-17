package config

import (
	"slices"
	"testing"
)

// The Witness is configured like any node and counted like none. A two-node
// cluster with a Witness has to read as two everywhere quorum is decided, or the
// two-node path ADR-0002 depends on switches off (ADR-0006).
func TestCountedNodesLeaveTheWitnessOut(t *testing.T) {
	nodes := map[string]*Node{
		"node-b":  {Hostname: "b"},
		"witness": {Hostname: "w", Witness: true},
		"node-a":  {Hostname: "a"},
		"":        {Hostname: "blank id"},
		"node-c":  nil,
	}
	if got, want := CountedNodeIDs(nodes), []string{"node-a", "node-b"}; !slices.Equal(got, want) {
		t.Errorf("CountedNodeIDs = %v, want %v", got, want)
	}
	if got := CountedNodeCount(nodes); got != 2 {
		t.Errorf("CountedNodeCount = %d, want 2", got)
	}
}

// Maintenance still counts. It is the precedent the Witness borrows its
// plumbing from, and confusing the two is the risk the separate field exists for.
func TestMaintenanceStillCounts(t *testing.T) {
	nodes := map[string]*Node{"a": {}, "b": {Maintenance: true}}
	if got := CountedNodeCount(nodes); got != 2 {
		t.Errorf("CountedNodeCount with a node in Maintenance = %d, want 2", got)
	}
}

func TestIsWitnessIsNilSafe(t *testing.T) {
	var n *Node
	if n.IsWitness() {
		t.Error("a missing node read as the Witness")
	}
	if !(&Node{Witness: true}).IsWitness() {
		t.Error("the Witness did not read as one")
	}
}
