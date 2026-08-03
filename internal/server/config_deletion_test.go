package server

import (
	"context"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// Regression tests for docs/TEST-PLAN.md defect #43, the silent arm: a mutation
// that *removes* something commits locally, reports success, and is undone by
// every peer that receives it.
//
// ConfigSync's merge was written to protect a receiver from a payload that does
// not carry groups at all (the #5-era envelope), but it expressed that as "if the
// incoming list is missing or empty, prefer mine". Absence and emptiness are
// exactly what a removal looks like on the wire, so a full config that had a
// group deleted, an assignment unassigned, or its last address removed was read
// as an opinion-free payload and the receiver restored its own copy — then
// replied Success. The sender's broadcaster therefore recorded full propagation
// and cleared its retry state, so #43's retry timer could never repair it.
//
// On whitecrane this was `group delete --force` on the coordinator: write 1 (the
// release) propagated to all four, write 2 (the delete) never did, leaving the
// other three nodes still listing a group the coordinator had dropped (run 27,
// and again in cycle 10 of run 28). It is also what gated #59 — see the run 27
// notes.
//
// The rule these tests pin: within the full-config branch, a payload that is
// strictly newer and *carries* the field is authoritative about it. Nil (the
// field genuinely absent from the JSON) still means "no opinion", which is the
// legacy case the merge existed for and is covered below.

// newDeletionSyncServer builds the receiving node and makes the asynchronous
// reconfigure a full ConfigSync spawns waitable, so a test cannot end while that
// goroutine is still reading config.CONFIG_LOCATION — see onAsyncReconfigure.
// The channel is buffered because the goroutine can finish before ConfigSync has
// even replied.
func newDeletionSyncServer(t *testing.T, localID, peerID string) (*Server, chan struct{}) {
	t.Helper()

	s, _ := newConfigSyncTestServer(t, localID, peerID)
	reconfigured := make(chan struct{}, 8)
	s.onAsyncReconfigure = func() { reconfigured <- struct{}{} }
	return s, reconfigured
}

// syncFromPeer delivers cfg to s as a full config from peerID at the given
// version, the way broadcastConfigToPeersOnce does, and returns once the
// reconfigure it triggered has finished.
func syncFromPeer(t *testing.T, s *Server, reconfigured chan struct{}, cfg *config.Config, peerID string, version int64) {
	t.Helper()

	states := map[string]membership.MemberStatus{}
	s.RLock()
	for id := range s.config.Nodes {
		states[id] = membership.StatusActive
	}
	s.RUnlock()

	payload, err := buildFullConfigPayload(cfg, states, 1, peerID, peerID, configStamp{version: version, origin: peerID})
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	resp, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload})
	if err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("ConfigSync was declined: %+v", resp)
	}
	if resp.Message == supersededConfigMessage {
		t.Fatalf("ConfigSync treated version %d as superseded; the test is not exercising the apply path", version)
	}

	select {
	case <-reconfigured:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconfigure spawned by ConfigSync never completed")
	}
}

// peerConfigFrom copies s's config into an independent *config.Config the test
// can mutate, so an assertion cannot pass on its own write. Groups and every
// node's IPGroups are deep-copied for the same reason.
func peerConfigFrom(s *Server) *config.Config {
	s.RLock()
	defer s.RUnlock()

	groups := make(map[string][]string, len(s.config.Groups))
	for g, ips := range s.config.Groups {
		groups[g] = append([]string(nil), ips...)
	}
	nodes := make(map[string]*config.Node, len(s.config.Nodes))
	for id, node := range s.config.Nodes {
		copied := *node
		if node.IPGroups != nil {
			copied.IPGroups = make(map[string][]string, len(node.IPGroups))
			for iface, gs := range node.IPGroups {
				copied.IPGroups[iface] = append([]string(nil), gs...)
			}
		}
		nodes[id] = &copied
	}
	return &config.Config{Pulse: s.config.Pulse, Groups: groups, Nodes: nodes}
}

// groupExists reports whether the group is still a key of the live config, which
// is the distinction a deletion turns on — an empty group and no group are
// different states.
func groupExists(s *Server, group string) bool {
	s.RLock()
	defer s.RUnlock()
	_, ok := s.config.Groups[group]
	return ok
}

// ifaceGroups reads a node's assigned groups for one interface off the live config.
func ifaceGroups(s *Server, nodeID, iface string) ([]string, bool) {
	s.RLock()
	defer s.RUnlock()
	node := s.config.Nodes[nodeID]
	if node == nil || node.IPGroups == nil {
		return nil, false
	}
	groups, ok := node.IPGroups[iface]
	return groups, ok
}

// The write-2 case: `group delete` removes the key from Groups, so the group is
// absent from the payload. Every peer resurrected it and answered Success.
func TestConfigSyncAppliesAGroupDeletion(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, reconfigured := newDeletionSyncServer(t, localID, peerID)

	if !groupExists(s, "group1") {
		t.Fatal("precondition: the receiver should start out holding group1")
	}

	// The coordinator's config after commitGroupDeletion.
	deleted := peerConfigFrom(s)
	delete(deleted.Groups, "group1")

	syncFromPeer(t, s, reconfigured, deleted, peerID, 5)

	if groupExists(s, "group1") {
		t.Errorf("a deleted group came back after the sync that deleted it: Groups still has group1 " +
			"(whitecrane run 27: the coordinator dropped the group and the other three kept listing it, " +
			"which is what leaves its addresses outside every computable set — defect #59)")
	}
}

// The `group unassign` case: the last group on an interface removes the whole
// interface entry, so the interface is absent from the payload and the receiver
// restored its own assignment. A node the operator unassigned stayed assigned
// everywhere else.
func TestConfigSyncAppliesAGroupUnassignment(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, reconfigured := newDeletionSyncServer(t, localID, peerID)

	if groups, ok := ifaceGroups(s, peerID, "eth0"); !ok || len(groups) == 0 {
		t.Fatal("precondition: the receiver should start out believing eth0 on the peer holds a group")
	}

	// The config after UnassignGroupFromNode dropped the peer's only assignment,
	// which deletes the interface key rather than leaving an empty list.
	unassigned := peerConfigFrom(s)
	delete(unassigned.Nodes[peerID].IPGroups, "eth0")

	syncFromPeer(t, s, reconfigured, unassigned, peerID, 5)

	if groups, ok := ifaceGroups(s, peerID, "eth0"); ok {
		t.Errorf("an unassignment was undone by the receiver: eth0 on %s still holds %v, want no entry", peerID, groups)
	}
}

// The `group remove-ip` case: removing the last address leaves the group present
// with an empty list, which the merge read as "no opinion" and refilled from
// local. The addresses the operator removed stayed in every peer's config.
func TestConfigSyncAppliesEmptyingAGroup(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, reconfigured := newDeletionSyncServer(t, localID, peerID)

	emptied := peerConfigFrom(s)
	emptied.Groups["group1"] = []string{}

	syncFromPeer(t, s, reconfigured, emptied, peerID, 5)

	if !groupExists(s, "group1") {
		t.Fatal("emptying a group deleted it; the group should still exist with no addresses")
	}
	if got := groupIPCount(s, "group1"); got != 0 {
		t.Errorf("a removal of every address in group1 was undone: group holds %d addresses, want 0", got)
	}
}

// The behaviour the merge was written for, and the reason the fix keys on nil
// rather than on emptiness: a payload that does not carry groups at all has no
// opinion about them, and must not be read as deleting every group. This is a
// sender that pre-dates the field, or one whose config never initialised the map.
func TestConfigSyncWithNoGroupsFieldPreservesLocalGroups(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, reconfigured := newDeletionSyncServer(t, localID, peerID)

	silent := peerConfigFrom(s)
	silent.Groups = nil
	silent.Nodes[peerID].IPGroups = nil

	syncFromPeer(t, s, reconfigured, silent, peerID, 5)

	if got := groupIPCount(s, "group1"); got != 2 {
		t.Errorf("a payload carrying no groups field erased local groups: group1 holds %d addresses, want 2", got)
	}
	if groups, ok := ifaceGroups(s, peerID, "eth0"); !ok || len(groups) == 0 {
		t.Errorf("a payload carrying no assignments erased local assignments: eth0 on %s = %v", peerID, groups)
	}
}
