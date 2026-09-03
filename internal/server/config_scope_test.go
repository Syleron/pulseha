package server

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// recordingPeer accepts every ConfigSync and keeps the payloads, so a test can
// ask what a peer was actually told rather than only whether it was called.
type recordingPeer struct {
	rpc.UnimplementedServerServer

	mu       sync.Mutex
	payloads [][]byte
}

func (p *recordingPeer) ConfigSync(_ context.Context, req *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.payloads = append(p.payloads, req.Config)
	return &rpc.ConfigSyncResponse{Success: true, Message: "applied"}, nil
}

func (p *recordingPeer) received() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([][]byte(nil), p.payloads...)
}

// startRecordingPeer serves a recordingPeer on an ephemeral loopback port.
func startRecordingPeer(t *testing.T) (*recordingPeer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	peer := &recordingPeer{}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// awaitPulseValue waits for the peer to receive a full config whose `pulseha`
// section holds key = want, and reports whether it did. Returns the last value it
// saw for the key so a failure can say what the peer was told instead.
func awaitPulseValue(t *testing.T, peer *recordingPeer, key string, want interface{}, within time.Duration) (bool, interface{}) {
	t.Helper()

	var lastSeen interface{}
	deadline := time.Now().Add(within)
	for {
		for _, payload := range peer.received() {
			var envelope struct {
				Pulse map[string]interface{} `json:"pulseha"`
			}
			if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Pulse == nil {
				continue
			}
			got, ok := envelope.Pulse[key]
			if !ok {
				continue
			}
			lastSeen = got
			if got == want {
				return true, got
			}
		}
		if time.Now().After(deadline) {
			return false, lastSeen
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Regression for docs/TEST-PLAN.md defect #42: a cluster-wide `config set`
// commits on the node the CLI ran against and nowhere else.
//
// UpdateConfig wrote the value into the local config, saved it, and returned
// success. Nothing stamped a new config version and nothing woke the broadcaster,
// so the value never left the node while the command's help text promised it was
// applied to the cluster — on whitecrane run 23 that left one node in
// active-passive against three in active-active.
func TestClusterWideConfigSetPropagatesToPeers(t *testing.T) {
	peer, addr := startRecordingPeer(t)

	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "fos_interval",
		Value: "7000",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.Success {
		t.Fatalf("UpdateConfig reported failure: %s", resp.Message)
	}
	if resp.Message != configScopeClusterMessage {
		t.Errorf("message = %q, want the cluster-wide scope %q", resp.Message, configScopeClusterMessage)
	}

	if got := s.config.Pulse.FailOverInterval; got != 7000 {
		t.Errorf("local fos_interval = %d, want 7000", got)
	}

	// json numbers unmarshal into float64.
	if ok, seen := awaitPulseValue(t, peer, "fos_interval", float64(7000), 10*time.Second); !ok {
		t.Fatalf("the peer never received fos_interval=7000 (last value seen: %v). A "+
			"cluster-wide config change has to be stamped and broadcast like any "+
			"other mutation; without that it commits on one node while the operator "+
			"is told it was applied to the cluster (defect #42)", seen)
	}
}

// The mode is the case that made #42 severe: node-1 flipped to active-passive on
// its own and logged "4 nodes are Active in active-passive mode; waiting for the
// coordinator to consolidate" 529 times against a coordinator still in
// active-active. Writing the value is not the operation — SetMode consolidates the
// cluster onto a single Active and broadcasts the statuses with the new mode, so
// `config set mode` has to go through it rather than past it.
func TestConfigSetModeGoesThroughSetMode(t *testing.T) {
	peer, addr := startRecordingPeer(t)

	s := newPropagationTestServer(t, addr) // starts in active-active, every member Active
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "mode",
		Value: "active-passive",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.Success {
		t.Fatalf("UpdateConfig reported failure: %s", resp.Message)
	}

	if got := s.config.Pulse.Mode; got != "active-passive" {
		t.Errorf("local mode = %q, want active-passive", got)
	}

	actives := 0
	for _, member := range s.memberList.MembersSnapshot() {
		if member.GetStatus() == membership.StatusActive {
			actives++
		}
	}
	if actives != 1 {
		t.Errorf("%d members Active after the switch to active-passive, want exactly 1; "+
			"a bare config write leaves every node Active, which is the wedged state "+
			"run 23 recorded (defect #42)", actives)
	}

	if ok, seen := awaitPulseValue(t, peer, "mode", "active-passive", 10*time.Second); !ok {
		t.Fatalf("the peer never received mode=active-passive (last value seen: %v); "+
			"a cluster running two modes at once is the split-brain configuration "+
			"quorum exists to prevent", seen)
	}
}

// A rejected value must not be settable cluster-wide either: refusing the key is
// what keeps the identity and the shared secret out of `config set`, and out of
// the broadcast that now follows a successful one.
func TestConfigSetRefusesKeysThatAreNotConfiguration(t *testing.T) {
	s := newPropagationTestServer(t)

	before := s.config.Pulse.ClusterToken
	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "cluster_token",
		Value: "attacker-supplied",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.Success {
		t.Error("cluster_token was accepted by config set; the cluster secret is not a " +
			"configuration value an operator sets by hand")
	}
	if got := s.config.Pulse.ClusterToken; got != before {
		t.Errorf("cluster_token = %q, want it left at %q", got, before)
	}
}

// Logging keys are node-local by design — ConfigSync preserves them so a peer left
// at debug for an investigation survives the next broadcast — so the command has
// to say so. Reporting "applied to the cluster" for a value that reaches one node
// is how run 23 ended up with logging_level set on two of four nodes.
func TestNodeLocalConfigSetIsReportedAsNodeLocal(t *testing.T) {
	peer, addr := startRecordingPeer(t)

	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "logging_level",
		Value: "debug",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.Success {
		t.Fatalf("UpdateConfig reported failure: %s", resp.Message)
	}
	if resp.Message != configScopeNodeMessage {
		t.Errorf("message = %q, want the node-local scope %q", resp.Message, configScopeNodeMessage)
	}
	if got := s.config.Pulse.LoggingLevel; got != "debug" {
		t.Errorf("local logging_level = %q, want debug", got)
	}

	// Nothing should have been pushed: a broadcast would be discarded by the
	// receiver's preserve list anyway, so sending one only invites the operator to
	// believe the level changed cluster-wide.
	if ok, seen := awaitPulseValue(t, peer, "logging_level", "debug", time.Second); ok {
		t.Errorf("the peer was sent logging_level=%v; node-local keys are not "+
			"propagated, and ConfigSync would preserve its own value regardless", seen)
	}
}
