package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// stampOf reads the ordering metadata off a recorded ConfigSync payload.
func stampOf(t *testing.T, payload []byte) (version int64, origin, sender string, present bool) {
	t.Helper()

	var envelope struct {
		Version *int64  `json:"config_version"`
		Origin  *string `json:"config_origin"`
		Sender  *string `json:"sender_id"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if envelope.Version == nil || envelope.Origin == nil || envelope.Sender == nil {
		return 0, "", "", false
	}
	return *envelope.Version, *envelope.Origin, *envelope.Sender, true
}

// applyConfigPayload hands the server a full config payload stamped as if it came
// from sender, and fails the test if the RPC itself errors. Whether the payload was
// *applied* is the caller's assertion to make.
func applyConfigPayload(t *testing.T, s *Server, cfg *config.Config,
	states map[string]membership.MemberStatus, sender string, stamp configStamp) {

	t.Helper()

	payload, err := buildFullConfigPayload(cfg, states, 1, sender, sender, stamp)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync(version %d): %v", stamp.version, err)
	}
}

// SetMode's direct config+state push has to be orderable like every other
// broadcast on this branch.
//
// It was not. broadcastConfigAndStates went through a buildConfigAndStatePayload
// wrapper that hardcoded `senderID: ""` and an empty configStamp, and
// buildFullConfigPayload gates the three metadata keys on both being set, so the
// payload went out with no config_version and no config_origin at all. On the
// receiving side configIsNewer treats an empty incoming stamp as "cannot be
// ordered, apply it" — deliberately, for a peer on an older binary — so the mode
// switch was applied *unconditionally by every peer*, including one holding
// strictly newer content. That is the #5/#38 window reopened for the length of a
// switch, in the one operation whose whole purpose is to stop two nodes running
// different modes.
//
// The stamp was already minted: SetMode calls markConfigDirty() before snapshotting
// the decision, so the fix is to carry what the clock already said rather than to
// weaken the ordering guarantee.
func TestSetModeBroadcastCarriesAnOrderableStamp(t *testing.T) {
	peer, addr := startRecordingPeer(t)
	s := newPropagationTestServer(t, addr)

	// Shaped like SetMode: mutate, stamp, then snapshot and broadcast.
	s.Lock()
	s.config.Pulse.Mode = "active-passive"
	s.markConfigDirty()
	stamp := s.loadConfigStamp()
	epoch, leader := s.clusterEpoch, "local-node"
	s.Unlock()

	states := map[string]membership.MemberStatus{"local-node": membership.StatusActive}
	s.broadcastConfigAndStates(states, epoch, leader)

	payloads := peer.received()
	if len(payloads) == 0 {
		t.Fatal("the peer received no ConfigSync at all")
	}

	version, origin, sender, present := stampOf(t, payloads[0])
	if !present {
		t.Fatalf("SetMode's config+state push carries no ordering metadata, so every "+
			"peer applies it unconditionally — including one holding a newer config. "+
			"payload: %s", payloads[0])
	}
	if version != stamp.version {
		t.Errorf("config_version = %d, want the stamp SetMode already minted (%d)",
			version, stamp.version)
	}
	if origin != "local-node" {
		t.Errorf("config_origin = %q, want the mutating node %q", origin, "local-node")
	}
	if sender != "local-node" {
		t.Errorf("sender_id = %q, want the broadcasting node %q", sender, "local-node")
	}
}

// The same defect from the receiving end, driven by the bytes SetMode actually
// puts on the wire rather than by a payload the test built itself.
//
// A real broadcast is captured off a recording peer and replayed into a second
// server that holds strictly newer content. That server must decline it. Under the
// old code the captured payload had no stamp, so configIsNewer returned its
// "cannot be ordered, apply it" default and the switch overwrote newer content —
// and the second-order half followed, because adoptConfigStamp also returns early
// on an empty stamp, leaving the receiver holding the new content under its own
// older stamp. Its subsequent broadcasts then misreport their version, and the
// coordinator's versioned re-push can be answered `superseded config version
// ignored` against content it does not actually hold.
//
// Asserted on both: the content must survive, and so must the stamp describing it.
func TestSetModeBroadcastIsDeclinedByAPeerHoldingNewerConfig(t *testing.T) {
	peer, addr := startRecordingPeer(t)
	sender := newPropagationTestServer(t, addr)

	// Drive the sender's clock well past the receiver's so the ordering question is
	// decided by the stamp rather than by the numbers happening to line up.
	sender.Lock()
	for i := 0; i < 5; i++ {
		sender.markConfigDirty()
	}
	sender.config.Pulse.Mode = "active-passive"
	sender.markConfigDirty()
	senderStamp := sender.loadConfigStamp()
	epoch := sender.clusterEpoch
	sender.Unlock()

	sender.broadcastConfigAndStates(
		map[string]membership.MemberStatus{"local-node": membership.StatusActive},
		epoch, "local-node")

	payloads := peer.received()
	if len(payloads) == 0 {
		t.Fatal("the peer received no ConfigSync at all")
	}
	captured := payloads[0]

	// A receiver holding newer content than the sender's stamp describes.
	const localID, peerID = "node-local", "node-peer"
	receiver, _ := newConfigSyncTestServer(t, localID, peerID)
	held := configStamp{version: senderStamp.version + 1, origin: peerID}
	applyConfigPayload(t, receiver, peerConfigWithGroup(receiver, "group1", 100),
		map[string]membership.MemberStatus{
			localID: membership.StatusActive,
			peerID:  membership.StatusActive,
		}, peerID, held)
	if got := receiver.loadConfigStamp(); got != held {
		t.Fatalf("setup: receiver stamp = %+v, want %+v", got, held)
	}

	if _, err := receiver.ConfigSync(context.Background(),
		&rpc.ConfigSyncRequest{Config: captured}); err != nil {
		t.Fatalf("ConfigSync(captured SetMode payload): %v", err)
	}

	if got := groupIPCount(receiver, "group1"); got != 100 {
		t.Errorf("group size = %d, want 100: SetMode's push overwrote content newer "+
			"than itself, so the switch is unordered against what the peer holds", got)
	}
	if got := receiver.config.Pulse.Mode; got != "active-active" {
		t.Errorf("mode = %q, want it unchanged at active-active: a mode switch older "+
			"than the receiver's config was applied anyway", got)
	}
	if got := receiver.loadConfigStamp(); got != held {
		t.Errorf("receiver stamp = %+v, want %+v held throughout; a peer that takes "+
			"content without its stamp misreports its version on every later broadcast",
			got, held)
	}
}
