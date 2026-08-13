package membership

import (
	"testing"
)

// The periodic config reconcile must only ever be driven by the coordinator.
//
// It re-broadcasts a node's whole config, and ConfigSync applies a config
// wholesale. If every node reconciled its own view, a node that was *behind*
// would push its stale config at a generation its peers had not seen from it, and
// they would adopt it — the repair would become the corruption. One speaker per
// cluster is the entire safety argument, so this gate is load-bearing rather than
// an optimisation (docs/TEST-PLAN.md defect #5).
func TestConfigReconcileIsCoordinatorGated(t *testing.T) {
	// clusterCoordinator picks deterministically, so asking it directly is what
	// makes this test independent of that choice rather than assuming it.
	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
	b := newAATestMember("node-b", "host-b", StatusPassive, nil)
	c := newAATestMember("node-c", "host-c", StatusPassive, nil)

	probe, _ := newAPTestChecker("node-a", a, b, c)
	coordinator := clusterCoordinator(probe.members.Members, probe.failoverGrace())
	if coordinator == "" {
		t.Fatal("no coordinator appointed; the fixture cannot exercise the gate")
	}

	t.Run("the coordinator reconciles", func(t *testing.T) {
		h, stub := newAPTestChecker(coordinator, a, b, c)
		h.reconcileConfigAcrossPeers(h.members.Members)
		if stub.configReconciles != 1 {
			t.Errorf("coordinator %s triggered %d reconciles, want 1", coordinator, stub.configReconciles)
		}
	})

	t.Run("a non-coordinator stays silent", func(t *testing.T) {
		var other string
		for _, id := range []string{"node-a", "node-b", "node-c"} {
			if id != coordinator {
				other = id
				break
			}
		}
		h, stub := newAPTestChecker(other, a, b, c)
		h.reconcileConfigAcrossPeers(h.members.Members)
		if stub.configReconciles != 0 {
			t.Errorf("non-coordinator %s triggered %d reconciles, want 0 — a node that is "+
				"behind would push its stale config over its peers' newer one",
				other, stub.configReconciles)
		}
	})
}
