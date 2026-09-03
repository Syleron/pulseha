package server

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// These cover docs/TEST-PLAN.md #103: a node diverged at an unchanged config
// generation, which the generation guard is documented to refuse to repair ("a
// peer already holding this generation ignores the message") and which nothing
// could see. The lab's three nodes disagreed about a fourth for hours, and two
// `pulsectl cluster converge` runs did nothing because a re-broadcast carries the
// same generation.
//
// The mechanism is a fingerprint of the shared config on every envelope, plus a
// majority tally over the fingerprints peers report, plus an addressed pull. The
// tally is the part that took two attempts: repairing purely because this node
// disagrees with the coordinator obeys a diverged coordinator exactly as a push
// would, which is the failure the coordinator gate in reconcileConfigAcrossPeers
// was written to avoid.

// waitFor polls until cond holds, so a test never asserts on an asynchronous
// repair before it has had the chance to happen.
func waitFor(t *testing.T, limit time.Duration, cond func() bool, complaint string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s (waited %v)", complaint, limit)
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return b
}

func mustUnmarshal(t *testing.T, b []byte, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
}

// capturingPeer keeps the last ConfigSync payload it was sent, so a test can read
// what actually went on the wire rather than what the builder was asked for.
type capturingPeer struct {
	rpc.UnimplementedServerServer

	mu   sync.Mutex
	last []byte
	n    int
}

func (p *capturingPeer) ConfigSync(_ context.Context, req *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	p.last = req.Config
	return &rpc.ConfigSyncResponse{Success: true}, nil
}

func (p *capturingPeer) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// configHash reads the fingerprint off the last payload, or "" if it carried none.
func (p *capturingPeer) configHash() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var e struct {
		ConfigHash string `json:"config_hash"`
	}
	_ = json.Unmarshal(p.last, &e)
	return e.ConfigHash
}

func startCapturingPeer(t *testing.T) (*capturingPeer, string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	peer := &capturingPeer{}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", ln.Addr(), err)
	}
	return peer, host, port
}

// serveServer publishes a real *Server on a loopback port, so a test drives the
// actual FetchConfig handler over the actual transport rather than a stub of it.
func serveServer(t *testing.T, s *Server) (ip, port string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, s)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	host, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", ln.Addr(), err)
	}
	return host, p
}

// envelopeFrom builds the envelope-shaped payload a peer's BroadcastClusterState
// puts on the wire: convergence metadata and a config hash, no `pulseha` root.
func envelopeFrom(t *testing.T, senderID, hash string) []byte {
	t.Helper()

	payload := map[string]interface{}{
		"member_states": map[string]int{},
		"epoch":         int64(1),
		"leader_id":     "",
		"sender_id":     senderID,
		"config_hash":   hash,
	}
	return mustMarshal(t, payload)
}

// divergedCluster builds the #103 shape: a local node whose config has lost
// node-c, a coordinator (node-a) that still has it, and node-c itself.
//
// The local node is the diverged one, so its config genuinely does not contain
// node-c -- which is why the tally cannot be filtered through it.
func divergedCluster(t *testing.T) (local *Server, coordinator *Server) {
	t.Helper()

	coordinator, _ = newConfigSyncTestServer(t, "node-a", "node-b", "node-c")
	local, _ = newConfigSyncTestServer(t, "node-b", "node-a", "node-c")

	// The divergence itself: node-b never learned node-c.
	local.Lock()
	delete(local.config.Nodes, "node-c")
	local.Unlock()

	local.coordinatorID = func() string { return "node-a" }
	coordinator.coordinatorID = func() string { return "node-a" }

	// Both at the same generation, which is the whole of #103: the node that
	// missed a join is not *behind*, it is level and wrong, and everything that
	// would otherwise repair it is documented to skip that case.
	stamp := configStamp{version: 7, origin: "node-a"}
	local.configStamp.Store(&configStamp{version: stamp.version, origin: stamp.origin})
	coordinator.configStamp.Store(&configStamp{version: stamp.version, origin: stamp.origin})

	return local, coordinator
}

// pointAt rewrites the named node's endpoint in cfg so a dial reaches the test's
// listener instead of the 192.0.2.x documentation address the helper assigns.
func pointAt(s *Server, nodeID, ip, port string) {
	s.Lock()
	defer s.Unlock()
	if node := s.config.Nodes[nodeID]; node != nil {
		node.IP = ip
		node.Port = port
	}
}

func hashOfServer(t *testing.T, s *Server) string {
	t.Helper()
	h, err := sharedConfigHash(s.currentConfig())
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	return h
}

// TestADivergedNodeInTheMinorityRepairsItselfWithoutAMutation is #103 end to end,
// and the row's live check in miniature: a node short one member learns it is in
// the minority from the fingerprints on ordinary envelopes, pulls the
// coordinator's config, and gains the member -- with no config mutation made
// anywhere in the cluster, which is exactly what the field had to do by hand.
func TestADivergedNodeInTheMinorityRepairsItselfWithoutAMutation(t *testing.T) {
	local, coordinator := divergedCluster(t)

	ip, port := serveServer(t, coordinator)
	pointAt(local, "node-a", ip, port)

	good := hashOfServer(t, coordinator)
	if bad := hashOfServer(t, local); bad == good {
		t.Fatal("the two configs hash the same, so this test is not set up as diverged " +
			"and proves nothing")
	}

	// node-c reports the coordinator's view, putting the local node in a minority
	// of one. Without this the cluster is two-against-nothing and nothing repairs.
	local.recordPeerConfigHash("node-c", good)

	// An ordinary envelope from the coordinator -- no full config, no mutation.
	if _, err := local.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: envelopeFrom(t, "node-a", good)}); err != nil {
		t.Fatalf("ConfigSync(envelope): %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		local.RLock()
		_, ok := local.config.Nodes["node-c"]
		local.RUnlock()
		return ok
	}, "node-b never learned node-c from the coordinator")

	if got := hashOfServer(t, local); got != good {
		t.Errorf("after repair the local hash is %s, the coordinator's is %s; the "+
			"repair applied something, but not the coordinator's config",
			shortHash(got), shortHash(good))
	}
}

// TestADivergedCoordinatorIsContradictedNotObeyed is the safety property, and the
// reason the tally exists at all.
//
// Here the *coordinator* is the node that lost a member and the local node is
// healthy. A detector that repaired on "I disagree with the coordinator" would
// adopt the coordinator's truncated config and spread the divergence to every
// peer in turn -- the failure reconcileConfigAcrossPeers' coordinator gate was
// written to avoid, and which a pull does nothing to prevent on its own.
func TestADivergedCoordinatorIsContradictedNotObeyed(t *testing.T) {
	healthy, coordinator := newConfigSyncTestServer(t, "node-b", "node-a", "node-c")
	_ = coordinator
	diverged, _ := newConfigSyncTestServer(t, "node-a", "node-b", "node-c")

	// The coordinator is the one missing a member this time.
	diverged.Lock()
	delete(diverged.config.Nodes, "node-c")
	diverged.Unlock()

	sink := &syncBuffer{}
	healthy.logger = log.New(sink)
	healthy.coordinatorID = func() string { return "node-a" }

	ip, port := serveServer(t, diverged)
	pointAt(healthy, "node-a", ip, port)

	healthyHash := hashOfServer(t, healthy)
	coordinatorHash := hashOfServer(t, diverged)
	if healthyHash == coordinatorHash {
		t.Fatal("the configs hash the same; this test is not set up as diverged")
	}

	// node-c agrees with the local node, so the coordinator is one of three.
	healthy.recordPeerConfigHash("node-c", healthyHash)

	if _, err := healthy.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: envelopeFrom(t, "node-a", coordinatorHash)}); err != nil {
		t.Fatalf("ConfigSync(envelope): %v", err)
	}

	// Give a repair every chance to happen before concluding it did not.
	time.Sleep(500 * time.Millisecond)
	healthy.awaitAsyncReconfigures()

	healthy.RLock()
	_, stillHasC := healthy.config.Nodes["node-c"]
	healthy.RUnlock()
	if !stillHasC {
		t.Error("the healthy node adopted the coordinator's truncated config and lost " +
			"node-c. One diverged node has just become two, and every other peer " +
			"follows")
	}
	if got := hashOfServer(t, healthy); got != healthyHash {
		t.Errorf("the healthy node's config changed (%s -> %s) on the word of a "+
			"minority coordinator", shortHash(healthyHash), shortHash(got))
	}
	if logged := sink.String(); !strings.Contains(logged, "not in the majority") {
		t.Errorf("nothing in the log says why no repair happened, so an operator "+
			"cannot tell this from the detector being broken. Got:\n%s", logged)
	}
}

// TestATwoNodeClusterReportsAndRepairsNothing is the degraded case, and it is
// deliberate rather than an oversight. Two nodes disagreeing have no third view to
// consult, so there is no evidence about which copy is right; picking one anyway
// is how a cluster silently discards a config nobody meant to lose. The same
// reasoning as docs/adr/0002-two-node-availability.md.
func TestATwoNodeClusterReportsAndRepairsNothing(t *testing.T) {
	local, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	other, _ := newConfigSyncTestServer(t, "node-a", "node-b")

	other.Lock()
	other.config.Groups["group1"] = append(other.config.Groups["group1"], "10.0.0.3/24")
	other.Unlock()

	sink := &syncBuffer{}
	local.logger = log.New(sink)
	local.coordinatorID = func() string { return "node-a" }

	ip, port := serveServer(t, other)
	pointAt(local, "node-a", ip, port)

	before := hashOfServer(t, local)
	coordinatorHash := hashOfServer(t, other)
	if before == coordinatorHash {
		t.Fatal("the configs hash the same; this test is not set up as diverged")
	}

	if _, err := local.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: envelopeFrom(t, "node-a", coordinatorHash)}); err != nil {
		t.Fatalf("ConfigSync(envelope): %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	local.awaitAsyncReconfigures()

	if got := hashOfServer(t, local); got != before {
		t.Errorf("a two-node cluster repaired on a one-to-one disagreement (%s -> %s); "+
			"there is no majority to establish which copy is right",
			shortHash(before), shortHash(got))
	}
	if logged := sink.String(); !strings.Contains(logged, "CONFIG_DIVERGENCE") {
		t.Errorf("a two-node cluster that cannot repair must at least report; the "+
			"lab's divergence was invisible for hours. Got:\n%s", logged)
	}
}

// TestARepairIsAppliedAtAnEqualGeneration is the exemption itself. Without it the
// pull is pointless: the payload arrives at the generation the node already holds,
// which is the one case the guard refuses.
func TestARepairIsAppliedAtAnEqualGeneration(t *testing.T) {
	local, coordinator := divergedCluster(t)

	stamp := configStamp{version: 7, origin: "node-a"}

	coordinator.RLock()
	payload, err := buildRepairConfigPayload(coordinator.config,
		map[string]membership.MemberStatus{}, 1, "", "node-a", stamp, "node-b")
	coordinator.RUnlock()
	if err != nil {
		t.Fatalf("buildRepairConfigPayload: %v", err)
	}

	resp, err := local.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload})
	if err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}
	if !resp.Success {
		t.Fatalf("the repair was refused: %s", resp.Message)
	}
	local.awaitAsyncReconfigures()

	local.RLock()
	_, ok := local.config.Nodes["node-c"]
	local.RUnlock()
	if !ok {
		t.Errorf("a repair addressed to this node was not applied at an equal "+
			"generation (reply %q). That generation is the whole of #103", resp.Message)
	}
}

// TestARepairAddressedToAnotherNodeIsRefused is the negative control the exemption
// needs, and the row asks for by name. `repair_for` is what makes a config
// applicable at an equal generation, so a payload carrying it must be inert
// anywhere but at the node that asked -- otherwise one captured repair is a way to
// force any config on any node.
func TestARepairAddressedToAnotherNodeIsRefused(t *testing.T) {
	local, coordinator := divergedCluster(t)

	stamp := configStamp{version: 7, origin: "node-a"}

	coordinator.RLock()
	payload, err := buildRepairConfigPayload(coordinator.config,
		map[string]membership.MemberStatus{}, 1, "", "node-a", stamp, "node-somebody-else")
	coordinator.RUnlock()
	if err != nil {
		t.Fatalf("buildRepairConfigPayload: %v", err)
	}

	resp, err := local.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload})
	if err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}
	local.awaitAsyncReconfigures()

	local.RLock()
	_, ok := local.config.Nodes["node-c"]
	local.RUnlock()
	if ok {
		t.Errorf("a repair addressed to node-somebody-else was applied here anyway "+
			"(reply %q). The exemption has to be addressed, or it is simply a hole "+
			"in the generation guard", resp.Message)
	}
}

// TestAnEqualGenerationConfigWithoutARepairTagIsStillRefused is the regression
// control on the guard: the exemption must be the only thing that got looser.
// #5's late-arriving snapshot and #43's absence-means-removal both depend on an
// equal-or-older config being dropped.
func TestAnEqualGenerationConfigWithoutARepairTagIsStillRefused(t *testing.T) {
	local, coordinator := divergedCluster(t)

	stamp := configStamp{version: 7, origin: "node-a"}

	coordinator.RLock()
	payload, err := buildFullConfigPayload(coordinator.config,
		map[string]membership.MemberStatus{}, 1, "", "node-a", stamp)
	coordinator.RUnlock()
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}

	if _, err := local.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}
	local.awaitAsyncReconfigures()

	local.RLock()
	_, ok := local.config.Nodes["node-c"]
	local.RUnlock()
	if ok {
		t.Error("an untagged config at an equal generation was applied. The guard is " +
			"what makes a carried group's absence safe to honour (#43) and what stops " +
			"a late snapshot reverting a newer one (#5)")
	}
}

// TestTheDetectorTreatsAnAbsentHashAsUnknown covers the rolling upgrade. A peer on
// an older binary sends no fingerprint, and comparing against "" would call every
// such peer diverged -- so every node would pull a repair from the coordinator on
// every envelope throughout an upgrade.
func TestTheDetectorTreatsAnAbsentHashAsUnknown(t *testing.T) {
	local, _ := divergedCluster(t)

	sink := &syncBuffer{}
	local.logger = log.New(sink)

	local.noteConfigDivergence("node-a", "")

	if logged := sink.String(); strings.Contains(logged, "CONFIG_DIVERGENCE") {
		t.Errorf("a peer that reported no fingerprint was treated as diverged:\n%s", logged)
	}
	if _, accounted := local.tallyConfigHashes("x", "y"); accounted != 1 {
		t.Errorf("a peer that reported no fingerprint was counted in the tally "+
			"(accounted for %d, want just this node)", accounted)
	}
}

// TestRepairsAreRateLimited bounds the cost of being wrong. Envelopes arrive every
// few seconds from every peer, so a mismatch a pull cannot fix -- a fingerprint
// false positive, a coordinator that will not serve its config -- would otherwise
// be a pull per envelope per peer, indefinitely.
func TestRepairsAreRateLimited(t *testing.T) {
	local, _ := divergedCluster(t)

	// No listener, so every pull fails fast and leaves the mismatch in place.
	pointAt(local, "node-a", "127.0.0.1", "1")

	good := "a-hash-this-node-does-not-hold"
	local.recordPeerConfigHash("node-c", good)

	for i := 0; i < 5; i++ {
		local.noteConfigDivergence("node-a", good)
	}
	waitFor(t, 3*time.Second, func() bool { return !local.repairInFlight.Load() },
		"a repair never finished")

	last := local.lastConfigRepair.Load()
	if last == nil {
		t.Fatal("no repair was attempted at all, so the rate limit is not what is " +
			"being measured here")
	}
	// The second and subsequent notes must not have moved the stamp.
	if time.Since(*last) > configRepairInterval {
		t.Fatalf("the repair stamp is older than the interval; the test took too long")
	}

	before := *last
	local.noteConfigDivergence("node-a", good)
	if got := local.lastConfigRepair.Load(); !got.Equal(before) {
		t.Errorf("a second repair started %v after the first, inside the %v floor",
			got.Sub(before), configRepairInterval)
	}
}

// TestEnvelopesCarryTheConfigHash is the one that keeps the detector reachable at
// all. Envelopes are the continuous signal -- a full config only follows a
// mutation, which is precisely what a diverged cluster is waiting for and not
// getting.
func TestEnvelopesCarryTheConfigHash(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-a", "node-b")

	peer, host, port := startCapturingPeer(t)
	pointAt(s, "node-b", host, port)

	if err := s.BroadcastClusterState(map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
	}, 1, "node-a", nil); err != nil {
		t.Fatalf("BroadcastClusterState: %v", err)
	}
	if peer.calls() == 0 {
		t.Fatal("the peer was never called, so this test cannot see the payload")
	}
	if got := peer.configHash(); got == "" {
		t.Error("the envelope carried no config_hash, so no peer can ever detect " +
			"divergence from this node and #103 stays invisible")
	} else if want := hashOfServer(t, s); got != want {
		t.Errorf("the envelope carried hash %s, but this node's config hashes to %s",
			shortHash(got), shortHash(want))
	}
}

// TestTheCoordinatorServesItsConfigOnlyWhenAsked covers the handler's own
// contract: it is a read addressed to a named requester, and an unnamed request
// gets nothing rather than a config with no `repair_for` that would be refused
// anyway.
func TestTheCoordinatorServesItsConfigOnlyWhenAsked(t *testing.T) {
	coordinator, _ := newConfigSyncTestServer(t, "node-a", "node-b")

	resp, err := coordinator.FetchConfig(context.Background(), &rpc.FetchConfigRequest{})
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if resp.Success || len(resp.Config) != 0 {
		t.Error("FetchConfig served a config to a request that named no requester")
	}

	resp, err = coordinator.FetchConfig(context.Background(),
		&rpc.FetchConfigRequest{RequesterId: "node-b"})
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if !resp.Success || len(resp.Config) == 0 {
		t.Fatalf("FetchConfig served nothing to node-b: %s", resp.Message)
	}

	var payload struct {
		RepairFor string         `json:"repair_for"`
		Pulseha   map[string]any `json:"pulseha"`
		Nodes     map[string]any `json:"nodes"`
	}
	mustUnmarshal(t, resp.Config, &payload)
	if payload.RepairFor != "node-b" {
		t.Errorf("the served payload is addressed to %q, want node-b; without that "+
			"the requester refuses it at its own generation", payload.RepairFor)
	}
	if payload.Pulseha == nil || payload.Nodes == nil {
		t.Error("the served payload is not a full config, so it repairs nothing")
	}
}
