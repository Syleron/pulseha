package server

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// syncBuffer is a log sink a test can read while the code under test is still
// writing to it. charmbracelet/log does not serialise writes for us.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// replyingPeer answers every ConfigSync with one canned response, so a test can
// drive the sender's classification of a reply without having to reproduce the
// receiver state that would produce it.
type replyingPeer struct {
	rpc.UnimplementedServerServer

	reply *rpc.ConfigSyncResponse

	mu    sync.Mutex
	calls int
}

func (p *replyingPeer) ConfigSync(_ context.Context, _ *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	return p.reply, nil
}

func (p *replyingPeer) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func startReplyingPeer(t *testing.T, reply *rpc.ConfigSyncResponse) (*replyingPeer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	peer := &replyingPeer{reply: reply}
	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// syncMessage sends a stamped full config and returns the message the receiver
// replied with.
func syncMessage(t *testing.T, s *Server, states map[string]membership.MemberStatus,
	sender string, stamp configStamp) string {

	t.Helper()

	s.RLock()
	cfg := s.config
	s.RUnlock()

	payload, err := buildFullConfigPayload(cfg, states, 1, sender, sender, stamp)
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	resp, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload})
	if err != nil {
		t.Fatalf("ConfigSync(version %d): %v", stamp.version, err)
	}
	return resp.Message
}

// A re-push of the config a peer already holds is not the peer being ahead.
//
// The #68 fix has SetMode put the same stamp on the wire twice — the direct
// broadcastConfigAndStates push, and the markConfigDirty broadcast that follows
// 250ms later. The peer applies whichever lands first and adopts stamp N; the
// second arrival compares N against a held N with the same origin, fails
// configIsNewer, and used to be answered `superseded config version ignored`.
// That reply means something specific to the sender — "you are behind, your
// change will be reverted" — and none of it is true here: the peer holds exactly
// this config, from this node, at this version.
//
// The receiver is the only party that can tell the two apart, so the distinction
// has to be made in the reply rather than guessed at by the sender.
func TestReceiverDistinguishesAnEqualConfigFromAStaleOne(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}
	held := configStamp{version: 9, origin: peerID}

	if got := syncMessage(t, s, states, peerID, held); got == supersededConfigMessage {
		t.Fatalf("setup: the first push of version %d was itself declined", held.version)
	}
	if got := s.loadConfigStamp(); got != held {
		t.Fatalf("setup: held stamp = %+v, want %+v", got, held)
	}

	// The second SetMode push: same version, same origin, same content.
	if got := syncMessage(t, s, states, peerID, held); got != configAlreadyHeldMessage {
		t.Errorf("reply to a re-push of the held version = %q, want %q. Answering "+
			"%q tells the sender its change will not propagate and will be reverted, "+
			"which is false in every clause when the peer holds exactly this config",
			got, configAlreadyHeldMessage, supersededConfigMessage)
	}

	// The genuinely-behind case still has to say so, or #38's signature from the
	// sending end goes unreported.
	older := configStamp{version: held.version - 1, origin: peerID}
	if got := syncMessage(t, s, states, peerID, older); got != supersededConfigMessage {
		t.Errorf("reply to a strictly older config = %q, want %q", got, supersededConfigMessage)
	}

	// Equal version from a *different* origin is the concurrent-mutation case, not
	// the re-push case: one of the two mutations really is being dropped.
	loser := configStamp{version: held.version, origin: "node-aaa"}
	if got := syncMessage(t, s, states, "node-aaa", loser); got != supersededConfigMessage {
		t.Errorf("reply to an equal version from a losing origin = %q, want %q: that "+
			"mutation is genuinely lost and the sender has to hear about it",
			got, supersededConfigMessage)
	}
}

// The sending end of the same defect: a peer that answers "already held" has
// accepted, and must not be reported as a lost change.
//
// Under the old reply the broadcaster hit its superseded arm on every mode
// switch and logged, per peer, that this node's change "will not propagate and
// will be reverted by the next sync". By this branch's own standard (#61) a
// misleading Warn is what hides a line worth reading, and a mode switch is
// exactly when the log is being read.
func TestReAnnouncedConfigVersionIsNotWarnedAsALostChange(t *testing.T) {
	peer, addr := startReplyingPeer(t, &rpc.ConfigSyncResponse{
		Success: true,
		Message: configAlreadyHeldMessage,
	})

	s := newPropagationTestServer(t, addr)
	logs := &syncBuffer{}
	s.logger = log.New(logs)

	s.Lock()
	s.config.Pulse.Mode = "active-passive"
	s.markConfigDirty()
	s.Unlock()

	s.broadcastConfigToPeersOnce()

	if peer.callCount() == 0 {
		t.Fatal("the peer received no ConfigSync at all")
	}
	if got := logs.String(); strings.Contains(got, "will be reverted by the next sync") {
		t.Errorf("the broadcaster warned that its change will be reverted although the "+
			"peer replied %q — the peer holds exactly this config, the change did "+
			"propagate, and nothing will revert it. Logged:\n%s",
			configAlreadyHeldMessage, got)
	}
	if outstanding, ok := s.pendingPropagation(); ok {
		t.Errorf("the peer is recorded as unpropagated (%+v) although it holds this "+
			"very version; the broadcaster would re-push it forever", outstanding)
	}
}

// The other half: a peer that is genuinely ahead must still produce the warning,
// and must still be dropped from the retry set — re-sending cannot fix it.
func TestSupersededConfigIsStillWarnedAsALostChange(t *testing.T) {
	peer, addr := startReplyingPeer(t, &rpc.ConfigSyncResponse{
		Success: true,
		Message: supersededConfigMessage,
	})

	s := newPropagationTestServer(t, addr)
	logs := &syncBuffer{}
	s.logger = log.New(logs)

	s.Lock()
	s.markConfigDirty()
	s.Unlock()

	s.broadcastConfigToPeersOnce()

	if peer.callCount() == 0 {
		t.Fatal("the peer received no ConfigSync at all")
	}
	if got := logs.String(); !strings.Contains(got, "will be reverted by the next sync") {
		t.Errorf("a peer holding a strictly newer config produced no warning; this "+
			"node's mutation reported success to the operator and is about to be "+
			"undone (#38 from the sending end). Logged:\n%s", got)
	}
}
