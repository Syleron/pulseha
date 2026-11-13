package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// Regression tests for docs/TEST-PLAN.md defect #117: a node that has just
// restarted accepted a mutation against a config version of zero, reported
// success, and had the change refused by every peer as superseded -- reverted on
// the Active node, left as a permanent divergence on the Passive one. The
// appliance makes exactly such a mutation on every daemon start.

// heldStamp reads a server's in-memory clock.
func heldStamp(s *Server) configStamp { return s.loadConfigStamp() }

// setStamp sets a server's clock directly, the way a test stands a node at a
// known point in the cluster's history.
func setStamp(s *Server, version int64, origin string) {
	s.configStamp.Store(&configStamp{version: version, origin: origin})
}

// setGroup replaces a group's addresses on the live config, under the lock.
func setGroup(s *Server, group string, ips []string) {
	s.Lock()
	defer s.Unlock()
	s.config.Groups[group] = append([]string(nil), ips...)
}

// liveGroupIPs reads a group's addresses off the live config.
func liveGroupIPs(s *Server, group string) []string {
	s.RLock()
	defer s.RUnlock()
	return append([]string(nil), s.config.Groups[group]...)
}

// stampOnDisk reads the stamp off the config file the harness points
// CONFIG_LOCATION at. Read directly rather than through Load, which skips the
// disk under PULSEHA_TEST.
func stampOnDisk(t *testing.T) config.Stamp {
	t.Helper()
	raw, err := os.ReadFile(config.CONFIG_LOCATION)
	if err != nil {
		t.Fatalf("reading the config on disk: %v", err)
	}
	onDisk := &config.Config{}
	if err := json.Unmarshal(raw, onDisk); err != nil {
		t.Fatalf("unmarshal the config on disk: %v", err)
	}
	return onDisk.Stamp
}

// twoNodePair builds the lab's shape: a local node and one peer, each a real
// *Server with its own config, the peer served over loopback so the local
// node's FetchConfig and ConfigSync reach the actual handlers.
func twoNodePair(t *testing.T) (local, peer *Server) {
	t.Helper()
	peer, _ = newConfigSyncTestServer(t, "node-a", "node-b")
	local, _ = newConfigSyncTestServer(t, "node-b", "node-a")
	ip, port := serveServer(t, peer)
	pointAt(local, "node-a", ip, port)
	return local, peer
}

// --- the gate -------------------------------------------------------------

// A Server nothing has shut the gate on -- every literal in this package -- is
// current, so the gate costs the existing tests nothing.
func TestTheGateIsOpenOnAServerNothingHasShut(t *testing.T) {
	s := &Server{}
	if !s.configIsCurrent() {
		t.Fatal("a fresh Server reports its config as not current")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.awaitConfigCurrent(ctx); err != nil {
		t.Fatalf("awaitConfigCurrent on an open gate: %v", err)
	}
}

func TestTheGateHoldsCallersUntilTheNodeIsCurrent(t *testing.T) {
	s := &Server{}
	s.markConfigStale("test")

	released := make(chan error, 1)
	go func() { released <- s.awaitConfigCurrent(context.Background()) }()

	select {
	case err := <-released:
		t.Fatalf("awaitConfigCurrent returned %v while the gate was shut", err)
	case <-time.After(50 * time.Millisecond):
	}

	s.markConfigCurrent()
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("awaitConfigCurrent after markConfigCurrent: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("markConfigCurrent did not release the waiter")
	}
}

// Deliverable 2 of #117 at the gate itself: a caller that cannot wait is
// refused, with the reason, rather than let through to a mutation the cluster
// will refuse.
func TestTheGateRefusesACallerThatCannotWait(t *testing.T) {
	s := &Server{}
	s.markConfigStale(configStaleSinceStart)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := s.awaitConfigCurrent(ctx)
	if !errors.Is(err, errConfigNotCurrent) {
		t.Fatalf("awaitConfigCurrent = %v, want errConfigNotCurrent", err)
	}
	if !strings.Contains(err.Error(), configStaleSinceStart) {
		t.Errorf("the refusal %q does not say why the node is not current", err)
	}
}

// The same refusal, seen the way lb_api sees it: over the handler, as
// Success:false with the reason in the message, and with the config untouched.
func TestAMutationOnANodeThatIsNotCurrentFailsToItsCaller(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	s.markConfigStale(configStaleSinceStart)
	before := liveGroupIPs(s, "group1")
	stampBefore := heldStamp(s)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp, err := s.AddIPToGroup(ctx, &rpc.AddIPToGroupRequest{GroupName: "group1", Ip: "10.0.0.3/24"})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if resp.Success {
		t.Fatal("AddIPToGroup reported success on a node whose config is not current; " +
			"the change would be refused by every peer and reverted here (#117)")
	}
	if !strings.Contains(resp.Message, "not current") {
		t.Errorf("the refusal %q does not tell the caller why", resp.Message)
	}
	if got := liveGroupIPs(s, "group1"); len(got) != len(before) {
		t.Errorf("the refused add still changed the group: %v -> %v", before, got)
	}
	if got := heldStamp(s); got != stampBefore {
		t.Errorf("the refused add still minted a version: %+v -> %+v", stampBefore, got)
	}
}

// Every mutation that propagates waits at the gate; a node-scoped `config set`
// does not, because it never leaves the node.
func TestEveryPropagatingMutationWaitsAtTheGate(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	s.markConfigStale(configStaleSinceStart)

	refused := func(name string, success bool, message string) {
		t.Helper()
		if success {
			t.Errorf("%s succeeded on a node whose config is not current", name)
		} else if !strings.Contains(message, "not current") {
			t.Errorf("%s was refused for another reason: %q", name, message)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	r1, _ := s.CreateGroup(ctx, &rpc.CreateGroupRequest{Name: "g2"})
	refused("CreateGroup", r1.Success, r1.Message)
	r2, _ := s.RemoveIPFromGroup(ctx, &rpc.RemoveIPFromGroupRequest{GroupName: "group1", Ip: "10.0.0.1/24"})
	refused("RemoveIPFromGroup", r2.Success, r2.Message)
	r3, _ := s.AssignGroupToNode(ctx, &rpc.AssignGroupRequest{GroupName: "group1", NodeId: "node-b", Interface: "eth1"})
	refused("AssignGroupToNode", r3.Success, r3.Message)
	r4, _ := s.UnassignGroupFromNode(ctx, &rpc.UnassignGroupRequest{GroupName: "group1", NodeId: "node-b", Interface: "eth0"})
	refused("UnassignGroupFromNode", r4.Success, r4.Message)
	r5, _ := s.DeleteGroup(ctx, &rpc.DeleteGroupRequest{GroupName: "group1"})
	refused("DeleteGroup", r5.Success, r5.Message)
	r6, _ := s.SetCapacity(ctx, &rpc.SetCapacityRequest{Capacity: 5})
	refused("SetCapacity", r6.Success, r6.Message)
	r7, _ := s.SetMode(ctx, &rpc.SetModeRequest{Mode: "active-passive"})
	refused("SetMode", r7.Success, r7.Message)
	r8, _ := s.UpdateConfig(ctx, &rpc.UpdateConfigRequest{Key: "fo_limit", Value: "12000"})
	refused("UpdateConfig(cluster-scoped)", r8.Success, r8.Message)
	r9, _ := s.HandleNodeJoin(ctx, &rpc.JoinRequest{Hostname: "node-c", NodeId: "node-c", BindIp: "192.0.2.9", BindPort: "8443"})
	refused("HandleNodeJoin", r9.Success, r9.Message)

	// Node-scoped: this node's own logging level propagates nowhere.
	r10, _ := s.UpdateConfig(ctx, &rpc.UpdateConfigRequest{Key: "logging_level", Value: "info"})
	if !r10.Success {
		t.Errorf("a node-scoped config set was refused at the gate: %q", r10.Message)
	}
}

// --- the persisted clock --------------------------------------------------

// A mutation writes the version it minted to disk beside the content, so the
// next start resumes the clock instead of restarting it.
func TestAMutationPersistsTheVersionItMints(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	setStamp(s, 4, "node-a")

	addAddress(s, "group1", "10.0.0.9/24")

	if got := heldStamp(s); got != (configStamp{version: 5, origin: "node-b"}) {
		t.Fatalf("stamp after the mutation = %+v, want {5 node-b}", got)
	}
	if got := s.config.PersistedStamp(); got != (config.Stamp{Version: 5, Origin: "node-b"}) {
		t.Errorf("the live config carries stamp %+v, want {5 node-b}", got)
	}
	if got := stampOnDisk(t); got != (config.Stamp{Version: 5, Origin: "node-b"}) {
		t.Errorf("the config on disk carries stamp %+v, want {5 node-b}; a restart "+
			"would resume the clock behind the cluster", got)
	}
}

// Applying a peer's config writes that config's stamp to disk with it.
func TestApplyingAPeerConfigPersistsItsVersion(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	states := map[string]membership.MemberStatus{"node-a": membership.StatusActive, "node-b": membership.StatusActive}
	payload, err := buildFullConfigPayload(peerConfigWithGroup(s, "group1", 3), states, 1, "node-a", "node-a",
		configStamp{version: 9, origin: "node-a"})
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}
	s.awaitAsyncReconfigures()

	if got := stampOnDisk(t); got != (config.Stamp{Version: 9, Origin: "node-a"}) {
		t.Errorf("the config on disk carries stamp %+v after applying version 9, want {9 node-a}", got)
	}
}

// NewServer resumes the clock from the file. This is the restart itself.
func TestANewServerResumesTheClockFromDisk(t *testing.T) {
	t.Setenv("PULSEHA_TEST", "true")
	prev := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = t.TempDir() + "/config.json"
	t.Cleanup(func() { config.CONFIG_LOCATION = prev })

	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-passive", LocalNode: "node-b", HealthCheckInterval: 1000, FailOverInterval: 5000, FailOverLimit: 10000},
		Groups: map[string][]string{},
		Nodes:  map[string]*config.Node{"node-b": {Hostname: "node-b", IP: "192.0.2.2", Port: "8443"}},
		Stamp:  config.Stamp{Version: 31, Origin: "node-a"},
	}
	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	s := NewServer(cfg, logger, ml, membership.NewHealthChecker(ml, logger))

	if got := heldStamp(s); got != (configStamp{version: 31, origin: "node-a"}) {
		t.Errorf("a new server started its clock at %+v, want {31 node-a} from the file", got)
	}

	// And a file with no stamp -- one from before the field, or rewritten by
	// the appliance -- leaves the clock empty, as it was.
	cfg.Stamp = config.Stamp{}
	s2 := NewServer(cfg, logger, ml, membership.NewHealthChecker(ml, logger))
	if got := heldStamp(s2); !got.isEmpty() {
		t.Errorf("a new server on an unstamped file started at %+v, want empty", got)
	}
}

func TestPersistedStampAfter(t *testing.T) {
	held := configStamp{version: 5, origin: "node-a"}
	cases := []struct {
		name     string
		incoming configStamp
		want     config.Stamp
	}{
		{"newer is adopted", configStamp{version: 6, origin: "node-b"}, config.Stamp{Version: 6, Origin: "node-b"}},
		{"older keeps held", configStamp{version: 4, origin: "node-b"}, config.Stamp{Version: 5, Origin: "node-a"}},
		{"unversioned keeps held", configStamp{}, config.Stamp{Version: 5, Origin: "node-a"}},
		{"tie winner moves the origin", configStamp{version: 5, origin: "node-b"}, config.Stamp{Version: 5, Origin: "node-b"}},
	}
	for _, tc := range cases {
		if got := persistedStampAfter(tc.incoming, held); got != tc.want {
			t.Errorf("%s: persistedStampAfter(%+v, %+v) = %+v, want %+v", tc.name, tc.incoming, held, got, tc.want)
		}
	}
}

// The stamp is metadata, not content: two nodes holding the same config at the
// same version agree, and the detector must not see the key.
func TestTheHashIgnoresThePersistedStamp(t *testing.T) {
	plain := hashTestConfig()
	stamped := hashTestConfig()
	stamped.Stamp = config.Stamp{Version: 12, Origin: "node-a"}
	if hashOf(t, plain) != hashOf(t, stamped) {
		t.Error("the config hash changes with the persisted stamp, so a node that has " +
			"written its stamp reports divergence from one that has not")
	}
}

// --- the startup pull: deliverable 1 ---------------------------------------

// The lab's Passive-node restart, end to end. The restarted node's disk is
// behind the peer; the startup pull brings it level; lb_api's re-add then mints
// a version above the cluster's, and the peer takes it. Before #117 the add was
// minted at 1, refused by the peer, and the two nodes never agreed again.
func TestARestartedNodeDoesNotLoseTheFirstChangeMadeOnIt(t *testing.T) {
	local, peer := twoNodePair(t)

	// What the node had on disk when it went down, and what the cluster did
	// while it was away.
	setStamp(local, 3, "node-b")
	setGroup(local, "group1", ipRange(2))
	setStamp(peer, 7, "node-a")
	setGroup(peer, "group1", ipRange(5))

	local.startStartupConfigSync()
	if local.configIsCurrent() {
		t.Fatal("the gate is open before the startup pull has run")
	}
	local.awaitConfigRepairs()

	if !local.configIsCurrent() {
		t.Fatal("the gate is still shut after the startup pull")
	}
	if got := heldStamp(local); got != (configStamp{version: 7, origin: "node-a"}) {
		t.Fatalf("after the startup pull the restarted node is at %+v, want the peer's {7 node-a}", got)
	}
	if got := groupIPCount(local, "group1"); got != 5 {
		t.Fatalf("after the startup pull the restarted node holds %d addresses, want the peer's 5", got)
	}
	local.awaitAsyncReconfigures()

	// lb_api's re-add, a second after the daemon came up.
	resp, err := local.AddIPToGroup(context.Background(),
		&rpc.AddIPToGroupRequest{GroupName: "group1", Ip: "10.10.91.212/24"})
	if err != nil || !resp.Success {
		t.Fatalf("AddIPToGroup after the startup pull: err=%v resp=%+v", err, resp)
	}
	if got := heldStamp(local); got != (configStamp{version: 8, origin: "node-b"}) {
		t.Fatalf("the add was minted at %+v, want {8 node-b}: above the cluster's version", got)
	}

	// The broadcast the add requested, run to completion against the peer's real
	// handler: the peer must take it, not refuse it as superseded.
	drainBroadcastTrigger(local)
	local.broadcastConfigToPeersOnce()
	peer.awaitAsyncReconfigures()

	if got := heldStamp(peer); got != (configStamp{version: 8, origin: "node-b"}) {
		t.Errorf("the peer is at %+v after the broadcast, want {8 node-b}: it refused the restarted node's add", got)
	}
	if got := liveGroupIPs(peer, "group1"); len(got) != 6 || got[5] != "10.10.91.212/24" {
		t.Errorf("the peer's group is %v, want the five it had plus 10.10.91.212/24", got)
	}
	if outstanding, ok := local.pendingPropagation(); ok {
		t.Errorf("the add is recorded as unpropagated: %+v", outstanding)
	}
}

// A restarted node that is level with its peer stays where it is, and the
// gate opens.
func TestAStartupPullAgainstALevelPeerChangesNothing(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 7, "node-a")
	setStamp(peer, 7, "node-a")
	before := liveGroupIPs(local, "group1")

	local.startStartupConfigSync()
	local.awaitConfigRepairs()

	if !local.configIsCurrent() {
		t.Fatal("the gate stayed shut after a startup pull from a level peer")
	}
	if got := heldStamp(local); got != (configStamp{version: 7, origin: "node-a"}) {
		t.Errorf("stamp after the pull = %+v, want unchanged {7 node-a}", got)
	}
	if got := liveGroupIPs(local, "group1"); len(got) != len(before) {
		t.Errorf("group changed across a pull from a level peer: %v -> %v", before, got)
	}
}

// A node that died between committing a mutation and delivering it comes back
// ahead of its peer. It must not adopt the peer's older config, and it re-sends
// its own so the peer catches up.
func TestAStartupPullFromAPeerThatIsBehindReSendsThisNodesConfig(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 9, "node-b")
	setGroup(local, "group1", ipRange(4))
	setStamp(peer, 8, "node-a")
	setGroup(peer, "group1", ipRange(3))
	// The harness leaves the trigger nil, which makes a re-send request inert;
	// this test is about whether one is made.
	local.broadcastTrigger = make(chan struct{}, 1)

	local.startStartupConfigSync()
	local.awaitConfigRepairs()

	if got := heldStamp(local); got != (configStamp{version: 9, origin: "node-b"}) {
		t.Errorf("the node ahead adopted the peer's older config: stamp %+v, want {9 node-b}", got)
	}
	if got := groupIPCount(local, "group1"); got != 4 {
		t.Errorf("the node ahead holds %d addresses after the pull, want its own 4", got)
	}
	if len(local.broadcastTrigger) == 0 {
		t.Error("no re-send was requested although the peer is behind this node")
	}
}

// Deliverable 2 in its natural setting: during the startup pull the appliance's
// first mutation arrives, and either waits for the pull or is refused -- never
// taken against the stale config. Then, with no peer reachable, the node has to
// become usable anyway (ADR-0002): a surviving node of a two-node cluster takes
// changes while its peer is down.
func TestAMutationDuringTheStartupPullWaitsAndAnUnreachablePeerDoesNotBlockTheNode(t *testing.T) {
	// node-a is left at the harness's 192.0.2.x address: TEST-NET-1, unroutable,
	// so the fetch runs out its budget.
	local, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	setStamp(local, 3, "node-b")

	started := time.Now()
	local.startStartupConfigSync()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp, err := local.AddIPToGroup(ctx, &rpc.AddIPToGroupRequest{GroupName: "group1", Ip: "10.0.0.3/24"})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if resp.Success {
		t.Fatal("a mutation during the startup pull was taken against the config on disk")
	}
	if !strings.Contains(resp.Message, "not current") {
		t.Errorf("refusal %q does not say the node is not current", resp.Message)
	}

	local.awaitConfigRepairs()
	if !local.configIsCurrent() {
		t.Fatal("the gate stayed shut with no peer reachable; a surviving node could never take a change")
	}
	if waited := time.Since(started); waited > configPullTimeout+2*time.Second {
		t.Errorf("the startup pull took %v to give up on an unreachable peer; the "+
			"appliance's reconcile waits that long on every start", waited)
	}
	resp, err = local.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{GroupName: "group1", Ip: "10.0.0.3/24"})
	if err != nil || !resp.Success {
		t.Fatalf("AddIPToGroup once the pull gave up: err=%v resp=%+v", err, resp)
	}
}

// --- superseded and envelope evidence: deliverable 3 -----------------------

// The lab's "will be reverted by the next sync", kept: a peer that answers
// superseded is pulled from at once, so the node is level with it in seconds
// rather than at the next mutation or never.
func TestASupersededAnswerMakesTheNodePullThePeersConfig(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 1, "node-b")
	setGroup(local, "group1", []string{"10.0.0.1/24", "10.10.91.212/24"})
	setStamp(peer, 10, "node-a")
	setGroup(peer, "group1", ipRange(3))

	// The mutation this node took while behind, and its broadcast.
	addAddress(local, "group1", "10.0.0.7/24")
	drainBroadcastTrigger(local)
	local.broadcastConfigToPeersOnce()

	waitFor(t, 5*time.Second, func() bool {
		return heldStamp(local) == configStamp{version: 10, origin: "node-a"}
	}, "the node that was answered superseded never pulled the peer's config")
	local.awaitConfigRepairs()

	if !local.configIsCurrent() {
		t.Error("the gate is still shut after the pull")
	}
	if got := liveGroupIPs(local, "group1"); len(got) != 3 {
		t.Errorf("after the pull the node holds %v, want the peer's three addresses", got)
	}
	if got := groupIPCount(peer, "group1"); got != 3 {
		t.Errorf("the peer's group changed to %d addresses; the node that was behind pushed onto it", got)
	}
}

// Envelopes carry the version, and a node that sees a higher one on an
// envelope pulls from the sender. This is what converges a two-node cluster
// where no full config is ever pushed and the hash tally has no majority.
func TestAnEnvelopeWithAHigherVersionMakesTheNodePull(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 2, "node-b")
	setStamp(peer, 5, "node-a")
	setGroup(peer, "group1", ipRange(4))

	envelope := envelopeFrom(t, "node-a", hashOfServer(t, peer))
	var decoded map[string]interface{}
	mustUnmarshal(t, envelope, &decoded)
	decoded["config_version"] = int64(5)
	decoded["config_origin"] = "node-a"
	if _, err := local.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: mustMarshal(t, decoded)}); err != nil {
		t.Fatalf("ConfigSync(envelope): %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		return heldStamp(local) == configStamp{version: 5, origin: "node-a"}
	}, "the node never pulled from the peer whose envelope showed a higher version")
	local.awaitConfigRepairs()
	if got := groupIPCount(local, "group1"); got != 4 {
		t.Errorf("after the pull the node holds %d addresses, want the peer's 4", got)
	}
	if !local.configIsCurrent() {
		t.Error("the gate is still shut after the pull")
	}
}

func TestAnEnvelopeAtOrBelowThisNodesVersionDoesNotPull(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 5, "node-a")
	setStamp(peer, 5, "node-a")
	setGroup(peer, "group1", ipRange(4))
	before := liveGroupIPs(local, "group1")

	for _, version := range []int64{5, 4} {
		envelope := envelopeFrom(t, "node-a", hashOfServer(t, local))
		var decoded map[string]interface{}
		mustUnmarshal(t, envelope, &decoded)
		decoded["config_version"] = version
		decoded["config_origin"] = "node-a"
		if _, err := local.ConfigSync(context.Background(),
			&rpc.ConfigSyncRequest{Config: mustMarshal(t, decoded)}); err != nil {
			t.Fatalf("ConfigSync(envelope %d): %v", version, err)
		}
	}
	local.awaitConfigRepairs()

	if !local.configIsCurrent() {
		t.Error("an envelope at or below this node's version shut the gate")
	}
	if got := liveGroupIPs(local, "group1"); len(got) != len(before) {
		t.Errorf("an envelope at or below this node's version changed the group: %v -> %v", before, got)
	}
}

// An unversioned envelope -- an older binary's -- is no evidence either way.
func TestAnUnversionedEnvelopeDoesNotPull(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(peer, 5, "node-a")

	if _, err := local.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: envelopeFrom(t, "node-a", hashOfServer(t, peer))}); err != nil {
		t.Fatalf("ConfigSync(envelope): %v", err)
	}
	local.awaitConfigRepairs()
	if !heldStamp(local).isEmpty() {
		t.Errorf("an unversioned envelope moved the clock to %+v", heldStamp(local))
	}
}

// Envelope pulls from one peer are floored, so a peer whose envelopes arrive
// but whose config cannot be fetched costs one pull per interval, not one per
// envelope.
func TestEnvelopePullsFromOnePeerAreRateLimited(t *testing.T) {
	local, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	setStamp(local, 2, "node-b")
	local.currency.lastPull = map[string]time.Time{"node-a": time.Now()}

	local.noteConfigVersion("node-a", configStamp{version: 9, origin: "node-a"})
	if !local.configIsCurrent() {
		t.Error("a second envelope inside the interval started another pull")
	}
}

func TestEnvelopesCarryTheConfigVersion(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-a", "node-b")
	setStamp(s, 6, "node-a")
	peer, host, port := startCapturingPeer(t)
	pointAt(s, "node-b", host, port)

	if err := s.BroadcastClusterState(map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
	}, 1, "node-a", nil); err != nil {
		t.Fatalf("BroadcastClusterState: %v", err)
	}
	if peer.calls() == 0 {
		t.Fatal("the peer was never called")
	}
	peer.mu.Lock()
	got := payloadStamp(peer.last)
	peer.mu.Unlock()
	if got != (configStamp{version: 6, origin: "node-a"}) {
		t.Errorf("the envelope carried stamp %+v, want {6 node-a}; a peer behind this node "+
			"cannot tell from its envelopes", got)
	}
}

// --- the pull itself --------------------------------------------------------

// The repair tag is for hash repairs at an equal generation. A pull on version
// evidence strips it, so a payload that turns out not to be newer -- the
// evidence was stale -- is refused by the ordinary guard rather than applied
// over newer content.
func TestAPullDoesNotUseTheRepairExemption(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 6, "node-b")
	setGroup(local, "group1", ipRange(6))
	setStamp(peer, 6, "node-a") // equal version, losing origin: not newer
	setGroup(peer, "group1", ipRange(2))

	applied, stamp, err := local.pullConfigFrom("node-a")
	if err != nil {
		t.Fatalf("pullConfigFrom: %v", err)
	}
	if applied {
		t.Fatal("a pull applied a config that is not newer than this node's")
	}
	if stamp != (configStamp{version: 6, origin: "node-a"}) {
		t.Errorf("the pull read stamp %+v off the payload, want {6 node-a}", stamp)
	}
	if got := groupIPCount(local, "group1"); got != 6 {
		t.Errorf("the pull replaced this node's 6 addresses with %d", got)
	}
}

func TestStripRepairTag(t *testing.T) {
	tagged := []byte(`{"pulseha":{},"repair_for":"node-b","config_version":3}`)
	stripped, err := stripRepairTag(tagged)
	if err != nil {
		t.Fatalf("stripRepairTag: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(stripped, &decoded); err != nil {
		t.Fatalf("unmarshal stripped: %v", err)
	}
	if _, still := decoded["repair_for"]; still {
		t.Error("repair_for survived stripping")
	}
	if _, kept := decoded["config_version"]; !kept {
		t.Error("stripping the tag dropped other keys")
	}

	untagged := []byte(`{"pulseha":{}}`)
	same, err := stripRepairTag(untagged)
	if err != nil {
		t.Fatalf("stripRepairTag(untagged): %v", err)
	}
	if string(same) != string(untagged) {
		t.Errorf("an untagged payload was rewritten: %s", same)
	}
}

func TestPayloadStamp(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    configStamp
	}{
		{"origin named", `{"sender_id":"s","config_version":4,"config_origin":"o"}`, configStamp{version: 4, origin: "o"}},
		{"origin falls back to sender", `{"sender_id":"s","config_version":4}`, configStamp{version: 4, origin: "s"}},
		{"unversioned", `{"sender_id":"s"}`, configStamp{origin: "s"}},
		{"garbage", `not json`, configStamp{}},
	}
	for _, tc := range cases {
		if got := payloadStamp([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: payloadStamp = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A pull that cannot reach its peer leaves the node usable: the gate opens and
// the node proceeds on what it holds.
func TestAFailedPullOpensTheGate(t *testing.T) {
	local, _ := newConfigSyncTestServer(t, "node-b", "node-a")
	// node-a stays at TEST-NET-1; the pull runs out its budget.
	local.startConfigPull("node-a", configStaleSuperseded, 0)
	if local.configIsCurrent() {
		t.Fatal("the gate is open while a pull is in flight")
	}
	local.awaitConfigRepairs()
	if !local.configIsCurrent() {
		t.Fatal("the gate stayed shut after a pull that could not reach its peer")
	}
}

// Two triggers while one pull is in flight share it: the second shuts nothing
// twice and starts nothing extra, and the one pull's end opens the gate.
func TestConcurrentTriggersShareOnePull(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(local, 1, "node-b")
	setStamp(peer, 4, "node-a")

	local.startConfigPull("node-a", configStaleSuperseded, 0)
	local.startConfigPull("node-a", configStaleFromEnvelope, 0)
	local.awaitConfigRepairs()

	if !local.configIsCurrent() {
		t.Error("the gate is still shut after the shared pull")
	}
	if got := heldStamp(local); got != (configStamp{version: 4, origin: "node-a"}) {
		t.Errorf("stamp after the shared pull = %+v, want {4 node-a}", got)
	}
}

// The config file the daemon writes is what the next daemon reads; the test
// above for NewServer covers the read, this covers that a sync-then-restart
// sequence gives the same clock the previous daemon held.
func TestTheClockSurvivesARestartAfterASync(t *testing.T) {
	local, peer := twoNodePair(t)
	setStamp(peer, 11, "node-a")

	local.startStartupConfigSync()
	local.awaitConfigRepairs()
	local.awaitAsyncReconfigures()

	if got := stampOnDisk(t); got != (config.Stamp{Version: 11, Origin: "node-a"}) {
		t.Errorf("the config on disk after the sync carries %+v, want {11 node-a}", got)
	}
}
