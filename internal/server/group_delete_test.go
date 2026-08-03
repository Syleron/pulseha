package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// releasingPeer is a real gRPC peer that records the BringDownIP calls a group
// deletion sends it. inspect, when set, runs while the call is in flight, which
// is how the ordering between the config write and the release is observed.
type releasingPeer struct {
	rpc.UnimplementedServerServer

	refuse  bool
	inspect func()

	mu    sync.Mutex
	calls []downCall
}

type downCall struct {
	iface string
	ips   []string
}

func (p *releasingPeer) BringDownIP(_ context.Context, req *rpc.DownIpRequest) (*rpc.DownIpResponse, error) {
	if p.inspect != nil {
		p.inspect()
	}

	p.mu.Lock()
	p.calls = append(p.calls, downCall{iface: req.Iface, ips: append([]string(nil), req.Ips...)})
	p.mu.Unlock()

	if p.refuse {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	return &rpc.DownIpResponse{Success: true, Message: "IPs brought down"}, nil
}

func (p *releasingPeer) released() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var ips []string
	for _, call := range p.calls {
		ips = append(ips, call.ips...)
	}
	slices.Sort(ips)
	return ips
}

// startReleasingPeer serves a releasingPeer on an ephemeral loopback port.
func startReleasingPeer(t *testing.T, peer *releasingPeer) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	srv := grpc.NewServer()
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return ln.Addr().String()
}

// groupDeleteIface is an interface name over the 15-character limit the kernel
// allows, so it is not merely absent from the test host but unnameable: the
// local release attempt therefore fails at netlink rather than mutating the
// machine's addresses.
const groupDeleteIface = "pulseha-no-such-interface"

// newGroupDeleteTestServer builds a Server that can take a real DeleteGroup: a
// config on disk under t.TempDir() so Save() succeeds, a six-address group
// assigned to the local node and to every peerAddr, and an all-Active member
// list as in active-active with the group split evenly between the nodes.
func newGroupDeleteTestServer(t *testing.T, peerAddrs ...string) *Server {
	t.Helper()

	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	groupIPs := []string{
		"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24",
		"10.0.0.4/24", "10.0.0.5/24", "10.0.0.6/24",
	}

	const localID = "local-node"
	nodes := map[string]*config.Node{
		localID: {
			Hostname: localID,
			IP:       "127.0.0.1",
			Port:     "49151",
			IPGroups: map[string][]string{groupDeleteIface: {"group1"}},
		},
	}
	nodeOrder := []string{localID}
	for i, addr := range peerAddrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("SplitHostPort(%s): %v", addr, err)
		}
		id := fmt.Sprintf("peer-%d", i)
		nodes[id] = &config.Node{
			Hostname: "peer-" + addr,
			IP:       host,
			Port:     port,
			IPGroups: map[string][]string{groupDeleteIface: {"group1"}},
		}
		nodeOrder = append(nodeOrder, id)
	}

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-active",
			LocalNode:           localID,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": groupIPs},
		Nodes:  nodes,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for _, id := range nodeOrder {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		ml.GetMemberByID(id).Status = membership.StatusActive
	}
	// Deal the group out round-robin, so each node's assigned share is the
	// subset it would actually be holding.
	for i, ip := range groupIPs {
		id := nodeOrder[i%len(nodeOrder)]
		m := ml.GetMemberByID(id)
		m.Lock()
		m.ActiveIPs = append(m.ActiveIPs, ip)
		m.Unlock()
	}

	return &Server{
		config:     cfg,
		logger:     logger,
		memberList: ml,
		ipMonitor:  membership.NewIPMonitor(ml, logger),
	}
}

// assignedShare returns the addresses the member list says nodeID holds.
func assignedShare(t *testing.T, s *Server, nodeID string) []string {
	t.Helper()

	m := s.memberList.GetMemberByID(nodeID)
	if m == nil {
		t.Fatalf("no member %s", nodeID)
	}
	ips := m.GetActiveIPs()
	slices.Sort(ips)
	return ips
}

// Regression for docs/TEST-PLAN.md defect #59: deleting an assigned group can
// leave its addresses up permanently, referenced by nothing.
//
// `group delete --force` removed every assignment and deleted the group in one
// config write and released nothing. Whichever nodes' enforce tick happened to
// fall inside the propagation window released their share as surplus; the rest
// kept theirs up indefinitely — on whitecrane node-1 held three addresses of a
// deleted 12-address group with no release pass ever running for them again.
// It cannot self-heal: surplusFloatingIPs scans only *configured* groups, so
// once the group is gone its addresses are outside every set any pass computes.
//
// The delete has to release them itself, because after it nothing can.
func TestForceDeleteReleasesTheGroupsAddressesOnEveryNode(t *testing.T) {
	peer := &releasingPeer{}
	s := newGroupDeleteTestServer(t, startReleasingPeer(t, peer))

	want := assignedShare(t, s, "peer-0")
	if len(want) == 0 {
		t.Fatal("the peer holds no addresses; the fixture proves nothing")
	}

	resp, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1",
		Force:     true,
	})
	if err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q), want the delete to complete", resp.Message)
	}

	if got := peer.released(); !slices.Equal(got, want) {
		t.Errorf("peer released %v, want its whole share %v", got, want)
	}
	if _, exists := s.config.Groups["group1"]; exists {
		t.Error("group1 is still configured; the delete did not complete")
	}
}

// The ordering is what makes the release safe, and it is not the ordering the
// defect note first suggested. Releasing while the group is still *assigned*
// races the enforce loop: in active-passive an Active node expects the whole
// configured group, so its next tick re-adds everything just released. Dropping
// the assignments first puts the cluster in the one state whose release pass is
// verified working (defect #58) — configured but unassigned — so a node that
// misses the explicit release still converges on its own.
//
// The group must therefore still be configured, and already unassigned, at the
// moment the release goes out.
func TestForceDeleteDropsTheAssignmentBeforeReleasing(t *testing.T) {
	var (
		sawGroup    bool
		sawAssigned bool
	)
	peer := &releasingPeer{}
	var s *Server
	peer.inspect = func() {
		s.RLock()
		defer s.RUnlock()

		_, sawGroup = s.config.Groups["group1"]
		for _, node := range s.config.Nodes {
			for _, groups := range node.IPGroups {
				if slices.Contains(groups, "group1") {
					sawAssigned = true
				}
			}
		}
	}
	s = newGroupDeleteTestServer(t, startReleasingPeer(t, peer))

	if _, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1",
		Force:     true,
	}); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}

	if !sawGroup {
		t.Error("group1 had already left the config when the release went out; " +
			"a node that misses this release has nothing left to converge on")
	}
	if sawAssigned {
		t.Error("group1 was still assigned when the release went out; the enforce " +
			"loop re-adds what it is still expected to hold")
	}
}

// A release that cannot be confirmed must not be followed by the delete.
//
// Same family as defects #13/#21/#31/#39/#57: the returned status is not
// evidence of what happened. Here a lost release is not log noise — deleting the
// group over it is what makes the strand permanent. Leaving the group configured
// and unassigned is the recoverable state: the peer's own release pass takes its
// share down when it can be reached again, and a retried delete finishes the job.
func TestForceDeleteKeepsTheGroupWhenAPeerReleaseCannotBeConfirmed(t *testing.T) {
	peer := &releasingPeer{refuse: true}
	s := newGroupDeleteTestServer(t, startReleasingPeer(t, peer))

	resp, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1",
		Force:     true,
	})
	if err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}

	if resp.Success {
		t.Error("Success = true although a peer's release was refused; the " +
			"addresses are still up and the group is gone")
	}
	if _, exists := s.config.Groups["group1"]; !exists {
		t.Error("group1 was deleted over an unconfirmed release — the addresses " +
			"are now referenced by nothing, which is defect #59 itself")
	}
	if len(resp.Warnings) == 0 {
		t.Error("no warnings; the operator gets no indication of which node failed")
	}

	// The assignments stay dropped so the peer's own release pass can finish.
	s.RLock()
	defer s.RUnlock()
	for id, node := range s.config.Nodes {
		for iface, groups := range node.IPGroups {
			if slices.Contains(groups, "group1") {
				t.Errorf("group1 is still assigned to %s:%s; the release pass only "+
					"fires on a group that is configured but unassigned", id, iface)
			}
		}
	}
}

// And the retry completes. After a failed release the group is configured with
// no assignments, so there is nothing left to release and the delete is the
// whole of the work.
func TestForceDeleteCompletesOnRetryAfterAFailedRelease(t *testing.T) {
	peer := &releasingPeer{refuse: true}
	s := newGroupDeleteTestServer(t, startReleasingPeer(t, peer))

	if _, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1", Force: true,
	}); err != nil {
		t.Fatalf("first DeleteGroup: %v", err)
	}

	resp, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1", Force: true,
	})
	if err != nil {
		t.Fatalf("retried DeleteGroup: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false (%q); with no assignments left there is nothing "+
			"to release and the delete must complete", resp.Message)
	}
	if _, exists := s.config.Groups["group1"]; exists {
		t.Error("group1 is still configured after the retry")
	}
}

// An address a still-configured group provides on the same interface must not be
// torn down. Nothing in the CLI can produce that overlap — AddIPToGroup rejects
// an address already held by another group — but config.json is written by the
// appliance too (defect #3), so the delete cannot assume the groups are disjoint.
func TestForceDeleteSpareAnAddressAnotherGroupStillProvides(t *testing.T) {
	peer := &releasingPeer{}
	s := newGroupDeleteTestServer(t, startReleasingPeer(t, peer))

	shared := assignedShare(t, s, "peer-0")[0]
	s.config.Groups["group2"] = []string{shared}
	s.config.Nodes["peer-0"].IPGroups[groupDeleteIface] = []string{"group1", "group2"}

	if _, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1", Force: true,
	}); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}

	if got := peer.released(); slices.Contains(got, shared) {
		t.Errorf("released %v, which includes %s — group2 still provides that "+
			"address on the same interface", got, shared)
	}
}

// The release plan is built from the record of who holds what, and that record
// was append-only until defect #58 — so on the one node whose interfaces can
// actually be read, the plan must not be the last word. The local node is
// therefore planned even when it is recorded as holding none of the group, so
// releaseGroupIPsLocally gets to check the kernel; a peer, whose state cannot be
// read at all, is not visited for nothing.
func TestPlanGroupReleaseAlwaysVisitsTheLocalNode(t *testing.T) {
	s := newGroupDeleteTestServer(t, "127.0.0.1:49152")

	// Nobody is recorded as holding anything.
	for _, id := range []string{"local-node", "peer-0"} {
		m := s.memberList.GetMemberByID(id)
		m.Lock()
		m.ActiveIPs = nil
		m.Unlock()
	}

	s.RLock()
	targets := s.planGroupRelease("group1")
	s.RUnlock()

	if len(targets) != 1 {
		t.Fatalf("planned %d targets, want only the local node: %+v", len(targets), targets)
	}
	local := targets[0]
	if !local.local || local.nodeID != "local-node" {
		t.Fatalf("planned %+v, want the local node", local)
	}
	if len(local.ips) != 0 {
		t.Errorf("ips = %v, want none: nothing is recorded as held", local.ips)
	}
	if len(local.candidates) != len(s.config.Groups["group1"]) {
		t.Errorf("candidates = %v, want every address of the group so the kernel "+
			"check can catch one the record missed", local.candidates)
	}
}

// The refusal without --force is unchanged: nothing is released and nothing is
// deleted, so the flag stays the only way to delete an assigned group.
func TestDeleteWithoutForceStillRefusesAnAssignedGroup(t *testing.T) {
	peer := &releasingPeer{}
	s := newGroupDeleteTestServer(t, startReleasingPeer(t, peer))

	resp, err := s.DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: "group1",
	})
	if err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if resp.Success {
		t.Error("Success = true for an assigned group without --force")
	}
	if _, exists := s.config.Groups["group1"]; !exists {
		t.Error("group1 was deleted without --force")
	}
	if got := peer.released(); len(got) != 0 {
		t.Errorf("released %v on a refused delete", got)
	}
}
