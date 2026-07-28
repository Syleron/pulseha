package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// groupIPCount reads the length of a floating IP group off the live config.
func groupIPCount(s *Server, group string) int {
	s.RLock()
	defer s.RUnlock()
	return len(s.config.Groups[group])
}

// ipRange builds a group of n addresses, so a "config as of add #n" snapshot can
// be described by its size the way the whitecrane runs were.
func ipRange(n int) []string {
	ips := make([]string, n)
	for i := 0; i < n; i++ {
		ips[i] = fmt.Sprintf("10.200.%d.%d/23", i/254, i%254+1)
	}
	return ips
}

// peerConfigWithGroup builds the config a peer would send, holding n addresses in
// the named group.
//
// It must be an independent *config.Config, not the server's own: s.config is a
// pointer, so aliasing it and assigning Groups mutates the very state the
// assertion then reads, and the test passes or fails on its own write rather than
// on what ConfigSync did.
//
// The base pointer is read under the read lock because ConfigSync spawns
// `go s.Reconfigure()`, which swaps s.config under the write lock. Reading it
// bare here is a genuine race the detector catches — Reload returns a fresh
// config so the pointed-to struct is never mutated, but the pointer itself moves.
func peerConfigWithGroup(s *Server, group string, n int) *config.Config {
	s.RLock()
	base := s.config
	s.RUnlock()

	nodes := make(map[string]*config.Node, len(base.Nodes))
	for id, node := range base.Nodes {
		copied := *node
		nodes[id] = &copied
	}
	return &config.Config{
		Pulse:  base.Pulse,
		Groups: map[string][]string{group: ipRange(n)},
		Nodes:  nodes,
	}
}

// Regression for docs/TEST-PLAN.md defect #5, the ordering half.
//
// Every group mutation used to end in a fire-and-forget
// `go s.broadcastFullConfigToPeers()`, and that goroutine marshalled s.config
// whenever it happened to be scheduled. N concurrent mutations therefore put N
// snapshots on the wire with no ordering, and ConfigSync applies a group
// wholesale — local is preferred only when the incoming list is empty or the
// group is absent. So an older snapshot arriving last simply overwrote a newer
// one, and nothing ever corrected it: on whitecrane 200 rapid add-ip calls from
// a single node left the four configs at 200/189/192/193, still diverged after
// two minutes, and one further serialised add-ip snapped all four into line.
//
// The generation is per sender because generations from different nodes are not
// comparable. It deliberately does not reuse clusterEpoch: bumping the epoch
// per add-ip would collide with defect #2's rule that an equal-epoch peer
// opinion of the local node's own status is ignored.
func TestStaleConfigSyncDoesNotOverwriteNewerGroups(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}

	// The peer's config as of its 200th add, generation 200.
	newer := peerConfigWithGroup(s, "group1", 200)
	payload, err := buildFullConfigPayload(newer, states, 1, peerID, peerID, 200)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync(gen 200): %v", err)
	}
	if got := groupIPCount(s, "group1"); got != 200 {
		t.Fatalf("group size after the generation-200 sync = %d, want 200", got)
	}

	// The same peer's older snapshot, delayed in flight and delivered second.
	// This is the 189 in 200/189/192/193.
	older := peerConfigWithGroup(s, "group1", 189)
	stalePayload, err := buildFullConfigPayload(older, states, 1, peerID, peerID, 189)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: stalePayload}); err != nil {
		t.Fatalf("ConfigSync(gen 189): %v", err)
	}

	if got := groupIPCount(s, "group1"); got != 200 {
		t.Errorf("a generation-189 snapshot overwrote generation 200: group size = %d, want 200 "+
			"(11 addresses this node would never bring up on failover)", got)
	}
}

// Re-sending the current generation must be a no-op rather than a rejection
// that loses data, because that is exactly what the periodic reconcile does: it
// re-broadcasts the current generation so a peer that missed a delivery heals,
// while a peer already holding it ignores the message.
func TestReconcileResendOfCurrentGenerationIsIdempotent(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}
	cfg := peerConfigWithGroup(s, "group1", 201)
	payload, err := buildFullConfigPayload(cfg, states, 1, peerID, peerID, 7)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatalf("ConfigSync #%d: %v", i+1, err)
		}
		if got := groupIPCount(s, "group1"); got != 201 {
			t.Fatalf("group size after resend #%d = %d, want 201", i+1, got)
		}
	}
}

// A payload from a different sender is ordered on its own generation sequence.
// Tracking one global high-water mark would make the first node to reach a high
// generation permanently silence every other node.
func TestConfigGenerationIsTrackedPerSender(t *testing.T) {
	const localID, peerA, peerB = "node-local", "node-a", "node-b"
	s, _ := newConfigSyncTestServer(t, localID, peerA, peerB)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerA:   membership.StatusActive,
		peerB:   membership.StatusActive,
	}

	high := peerConfigWithGroup(s, "group1", 50)
	payloadA, err := buildFullConfigPayload(high, states, 1, peerA, peerA, 500)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payloadA}); err != nil {
		t.Fatalf("ConfigSync(peerA gen 500): %v", err)
	}

	// peerB's generation 3 is not stale — it is a different sequence entirely.
	low := peerConfigWithGroup(s, "group1", 60)
	payloadB, err := buildFullConfigPayload(low, states, 1, peerB, peerB, 3)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payloadB}); err != nil {
		t.Fatalf("ConfigSync(peerB gen 3): %v", err)
	}

	if got := groupIPCount(s, "group1"); got != 60 {
		t.Errorf("peerB's generation 3 was rejected against peerA's 500: group size = %d, want 60", got)
	}
}

// An unversioned payload — an older peer mid-rolling-upgrade, or SetMode before
// it threads a generation through — must still apply. The guard is an ordering
// check, not an authentication check, so absent metadata means "cannot order
// this, apply it" rather than "drop it".
func TestUnversionedConfigSyncStillApplies(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}
	versioned := peerConfigWithGroup(s, "group1", 100)
	payload, err := buildFullConfigPayload(versioned, states, 1, peerID, peerID, 42)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync(gen 42): %v", err)
	}

	unversioned := peerConfigWithGroup(s, "group1", 120)
	legacy, err := buildConfigAndStatePayload(unversioned, states, 1, peerID)
	if err != nil {
		t.Fatalf("buildConfigAndStatePayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: legacy}); err != nil {
		t.Fatalf("ConfigSync(unversioned): %v", err)
	}

	if got := groupIPCount(s, "group1"); got != 120 {
		t.Errorf("an unversioned ConfigSync was dropped: group size = %d, want 120", got)
	}
}

// The generation must be allocated under the same lock that mutates the config,
// or two concurrent mutations can be handed the same number and the receiver
// cannot order them. Bumping it is what makes a mutation visible to the
// broadcaster, so this is the property the whole guard rests on.
func TestConcurrentMutationsGetDistinctGenerations(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	const mutations = 200
	seen := make([]int64, mutations)
	var wg sync.WaitGroup
	for i := 0; i < mutations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = s.nextConfigGeneration()
		}(i)
	}
	wg.Wait()

	unique := make(map[int64]bool, mutations)
	for _, g := range seen {
		if g == 0 {
			t.Fatal("generation 0 was allocated; 0 means unversioned and must never be handed out")
		}
		if unique[g] {
			t.Fatalf("generation %d allocated twice", g)
		}
		unique[g] = true
	}
	if len(unique) != mutations {
		t.Errorf("allocated %d distinct generations for %d mutations", len(unique), mutations)
	}
}

// Regression for a deadlock this fix nearly shipped with.
//
// Every group mutation — AddIPToGroup, RemoveIPFromGroup, CreateGroup,
// DeleteGroup, AssignGroupToNode, UnassignGroupFromNode, SetCapacity — takes
// s.Lock() and releases it through a defer, so markConfigDirty runs with the
// write lock still held. Allocating the generation under s.Lock() therefore
// self-deadlocked on a non-reentrant mutex and hung the mutation forever, which
// is why the counter is atomic. Same shape as the Load()/Save() self-deadlock
// recorded against defect #32.
//
// The test would hang rather than fail, so it runs on its own goroutine with a
// deadline.
func TestMarkConfigDirtyUnderServerLockDoesNotDeadlock(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Exactly how the mutation RPCs are written.
		s.Lock()
		defer s.Unlock()
		s.markConfigDirty()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("markConfigDirty deadlocked while the caller held s.Lock(); " +
			"every group mutation calls it that way")
	}

	if got := s.configGeneration.Load(); got != 1 {
		t.Errorf("config generation after one mutation = %d, want 1", got)
	}
}
