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
// The version deliberately does not reuse clusterEpoch: bumping the epoch per
// add-ip would collide with defect #2's rule that an equal-epoch peer opinion of
// the local node's own status is ignored.
func TestStaleConfigSyncDoesNotOverwriteNewerGroups(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}

	// The peer's config as of its 200th add, version 200.
	newer := peerConfigWithGroup(s, "group1", 200)
	payload, err := buildFullConfigPayload(newer, states, 1, peerID, peerID, 200)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync(version 200): %v", err)
	}
	if got := groupIPCount(s, "group1"); got != 200 {
		t.Fatalf("group size after the version-200 sync = %d, want 200", got)
	}

	// The same peer's older snapshot, delayed in flight and delivered second.
	// This is the 189 in 200/189/192/193.
	older := peerConfigWithGroup(s, "group1", 189)
	stalePayload, err := buildFullConfigPayload(older, states, 1, peerID, peerID, 189)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: stalePayload}); err != nil {
		t.Fatalf("ConfigSync(version 189): %v", err)
	}

	if got := groupIPCount(s, "group1"); got != 200 {
		t.Errorf("a version-189 snapshot overwrote version 200: group size = %d, want 200 "+
			"(11 addresses this node would never bring up on failover)", got)
	}
}

// The periodic reconcile re-sends the current config repeatedly, so receiving the
// same payload more than once must leave the group exactly as it was — whether the
// receiver applies it again or rejects it as already held. Either answer is
// correct; losing addresses is not.
func TestReconcileResendOfTheSameConfigIsIdempotent(t *testing.T) {
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

// Regression for docs/TEST-PLAN.md defect #38: a peer that is *behind* must not
// be able to erase a newer config, whoever it heard the newer one from.
//
// This is the case a per-sender generation structurally cannot catch, and it is
// why #38 survived the #5 fix. Generations from different senders were held in
// separate sequences, so a snapshot was only ever compared against that sender's
// own previous high-water mark — never against how current the *content* was.
// The coordinator, which re-broadcasts once a minute, is a different sender from
// the node taking the add-ip calls, so its stale view always passed the guard and
// was applied wholesale.
//
// On whitecrane, run 19: node-1 took 200 serial add-ip calls; node-2 (lowest UUID,
// therefore coordinator, therefore the only node allowed to re-broadcast) declined
// 16 of node-1's pushes and so was missing those adds. Its next reconcile pushed
// its own older config to everyone, and 9 addresses that had each been reported
// `Successfully added IP … to group RealTest` went missing from all four configs at
// once — uniform loss rather than divergence, which is why TC-3's "all four agree"
// criterion scored the run a pass.
func TestABehindPeerCannotEraseANewerConfigFromAnotherSender(t *testing.T) {
	const localID, mutator, coordinator = "node-local", "node-mutator", "node-coordinator"
	s, _ := newConfigSyncTestServer(t, localID, mutator, coordinator)

	states := map[string]membership.MemberStatus{
		localID:     membership.StatusActive,
		mutator:     membership.StatusActive,
		coordinator: membership.StatusActive,
	}

	// The node taking the add-ip calls reaches 200 addresses.
	newer := peerConfigWithGroup(s, "group1", 200)
	payload, err := buildFullConfigPayload(newer, states, 1, mutator, mutator, 200)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync(mutator, version 200): %v", err)
	}
	if got := groupIPCount(s, "group1"); got != 200 {
		t.Fatalf("group size after the mutator's sync = %d, want 200", got)
	}

	// The coordinator missed the last 11 adds, so its own view is version 189.
	// It is a different sender and has never spoken before, so under a per-sender
	// guard this is its generation 189 against a high-water mark of 0 — applied,
	// and 11 addresses that reported success vanish.
	behind := peerConfigWithGroup(s, "group1", 189)
	stale, err := buildFullConfigPayload(behind, states, 1, coordinator, coordinator, 189)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: stale}); err != nil {
		t.Fatalf("ConfigSync(coordinator, version 189): %v", err)
	}

	if got := groupIPCount(s, "group1"); got != 200 {
		t.Errorf("a behind coordinator's version-189 reconcile erased version 200: "+
			"group size = %d, want 200 (11 addresses whose add-ip reported success)", got)
	}
}

// A node that only ever *receives* configs must still broadcast a meaningful
// version, because the coordinator — the one node allowed to re-broadcast — is
// usually not the node taking the mutations.
//
// This is the mechanism that made defect #38 certain rather than merely possible.
// The version was a count of the node's *own* mutations, so a coordinator that had
// never mutated sat at 0 forever; buildFullConfigPayload omits the metadata at 0 to
// leave rolling upgrades working, so every reconcile it sent went out unversioned
// and the receiver applied it unconditionally. Run 19 caught one on the wire:
// node-2 pushing its own config at generation=0, inside the window where
// 10.200.0.181 and .182 were added and lost.
//
// Adopting the version on apply is what closes it: the number describes the config,
// not the speaker.
func TestApplyingAPeerConfigAdoptsItsVersion(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}
	cfg := peerConfigWithGroup(s, "group1", 200)
	payload, err := buildFullConfigPayload(cfg, states, 1, peerID, peerID, 200)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync(version 200): %v", err)
	}

	if got := s.configVersion.Load(); got != 200 {
		t.Errorf("config version after applying a version-200 config = %d, want 200; "+
			"at 0 this node's reconcile goes out unversioned and overwrites everyone", got)
	}

	// And a mutation of its own must land strictly above what it adopted, or the
	// change is invisible to every peer that already holds 200.
	s.Lock()
	s.markConfigDirty()
	s.Unlock()
	if got := s.configVersion.Load(); got != 201 {
		t.Errorf("config version after a local mutation = %d, want 201", got)
	}
}

// Two nodes mutating at the same version have to converge on one of them.
//
// Rejecting on anything but a strictly greater version would leave each holding
// its own and rejecting the other's, with the periodic reconcile — also at the
// equal version — unable to break the tie. The node ID is the tiebreak because it
// is the one input both sides agree on. The losing mutation is lost, which is the
// pre-existing limitation of applying a config wholesale; the point here is that
// the cluster converges rather than diverging permanently.
func TestEqualVersionsFromTwoSendersConvergeDeterministically(t *testing.T) {
	const localID, lowPeer, highPeer = "node-b", "node-a", "node-c"

	states := map[string]membership.MemberStatus{
		localID:  membership.StatusActive,
		lowPeer:  membership.StatusActive,
		highPeer: membership.StatusActive,
	}

	t.Run("a higher node ID wins the tie", func(t *testing.T) {
		s, _ := newConfigSyncTestServer(t, localID, lowPeer, highPeer)
		seed(t, s, states, lowPeer, 50, 10)

		contender := peerConfigWithGroup(s, "group1", 60)
		payload, err := buildFullConfigPayload(contender, states, 1, highPeer, highPeer, 10)
		if err != nil {
			t.Fatalf("buildFullConfigPayload: %v", err)
		}
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatalf("ConfigSync(%s, version 10): %v", highPeer, err)
		}
		if got := groupIPCount(s, "group1"); got != 60 {
			t.Errorf("group size = %d, want 60 — %s outranks %s so its config must win",
				got, highPeer, localID)
		}
	})

	t.Run("a lower node ID loses the tie", func(t *testing.T) {
		s, _ := newConfigSyncTestServer(t, localID, lowPeer, highPeer)
		seed(t, s, states, highPeer, 50, 10)

		contender := peerConfigWithGroup(s, "group1", 60)
		payload, err := buildFullConfigPayload(contender, states, 1, lowPeer, lowPeer, 10)
		if err != nil {
			t.Fatalf("buildFullConfigPayload: %v", err)
		}
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatalf("ConfigSync(%s, version 10): %v", lowPeer, err)
		}
		if got := groupIPCount(s, "group1"); got != 50 {
			t.Errorf("group size = %d, want 50 — %s is outranked by %s so its config must lose",
				got, lowPeer, localID)
		}
	})
}

// seed applies a starting config so a test can begin from a known version.
func seed(t *testing.T, s *Server, states map[string]membership.MemberStatus,
	senderID string, ips int, version int64) {

	t.Helper()
	cfg := peerConfigWithGroup(s, "group1", ips)
	payload, err := buildFullConfigPayload(cfg, states, 1, senderID, senderID, version)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("seed ConfigSync: %v", err)
	}
	if got := groupIPCount(s, "group1"); got != ips {
		t.Fatalf("seed group size = %d, want %d", got, ips)
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

// The version must be allocated atomically, or two concurrent mutations can be
// handed the same number and the receiver cannot order them. Bumping it is what
// makes a mutation visible to the broadcaster, so this is the property the whole
// guard rests on.
func TestConcurrentMutationsGetDistinctVersions(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	const mutations = 200
	seen := make([]int64, mutations)
	var wg sync.WaitGroup
	for i := 0; i < mutations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = s.nextConfigVersion()
		}(i)
	}
	wg.Wait()

	unique := make(map[int64]bool, mutations)
	for _, g := range seen {
		if g == 0 {
			t.Fatal("version 0 was allocated; 0 means unversioned and must never be handed out")
		}
		if unique[g] {
			t.Fatalf("version %d allocated twice", g)
		}
		unique[g] = true
	}
	if len(unique) != mutations {
		t.Errorf("allocated %d distinct versions for %d mutations", len(unique), mutations)
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

	if got := s.configVersion.Load(); got != 1 {
		t.Errorf("config version after one mutation = %d, want 1", got)
	}
}

// The comparison itself, pinned directly. ConfigSync runs it twice — once before
// taking s.Lock() and again under it, since a local mutation can land in that
// window — so it has to be a pure function of its inputs rather than a method
// that reaches for a lock it may already hold.
func TestConfigIsNewer(t *testing.T) {
	const local, lower, higher = "node-b", "node-a", "node-c"

	cases := []struct {
		name     string
		senderID string
		version  int64
		held     int64
		localID  string
		want     bool
	}{
		{"a newer version applies", higher, 11, 10, local, true},
		{"an older version is dropped", higher, 9, 10, local, false},
		{"a behind coordinator is dropped whoever it is", lower, 189, 200, local, false},
		{"an equal version from a higher node ID wins", higher, 10, 10, local, true},
		{"an equal version from a lower node ID loses", lower, 10, 10, local, false},
		{"an unversioned payload always applies", higher, 0, 200, local, true},
		{"a payload with no sender always applies", "", 500, 200, local, true},
		{"an unknown local ID loses every tie", higher, 10, 10, "", true},
		{"a fresh node accepts anything", higher, 1, 0, local, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configIsNewer(tc.senderID, tc.version, tc.held, tc.localID); got != tc.want {
				t.Errorf("configIsNewer(%q, %d, held %d, local %q) = %v, want %v",
					tc.senderID, tc.version, tc.held, tc.localID, got, tc.want)
			}
		})
	}
}
