package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// newAddIPTestServer builds a Server that can take a real AddIPToGroup: a config
// on disk under t.TempDir() so Save() succeeds, one group assigned to an
// interface that does not exist on the test host (so the local bring-up fails as
// a warning rather than mutating the machine's addresses), and an all-Active
// member list as in active-active. peerAddrs are added as extra nodes carrying
// the same group assignment, which is what drives the remote fan-out.
func newAddIPTestServer(t *testing.T, peerAddrs ...string) *Server {
	t.Helper()

	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	const localID = "local-node"
	// An interface name that cannot exist: over the 15-character limit the
	// kernel allows, so this is not merely absent but unnameable.
	const missingIface = "pulseha-no-such-interface"

	nodes := map[string]*config.Node{
		localID: {
			Hostname: localID,
			IP:       "127.0.0.1",
			Port:     "49151",
			IPGroups: map[string][]string{missingIface: {"group1"}},
		},
	}
	for i, addr := range peerAddrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("SplitHostPort(%s): %v", addr, err)
		}
		nodes[string(rune('a'+i))+"-peer"] = &config.Node{
			Hostname: "peer-" + addr,
			IP:       host,
			Port:     port,
			IPGroups: map[string][]string{missingIface: {"group1"}},
		}
	}

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-active",
			LocalNode:           localID,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": {"10.0.0.1/24"}},
		Nodes:  nodes,
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id := range nodes {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		ml.GetMemberByID(id).Status = membership.StatusActive
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	return &Server{config: cfg, logger: logger, memberList: ml}
}

// stalledPeer returns the address of a listener that completes the TCP handshake
// and then never speaks HTTP/2, so a gRPC call against it blocks until its
// context is done. This is the unreachable peer of defect #39 made
// deterministic: the failure mode that matters is a fan-out that outlives the
// caller's deadline, not one that is refused.
func stalledPeer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold it open and say nothing.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	return ln.Addr().String()
}

// Regression for docs/TEST-PLAN.md defect #39: an add-ip that returns rc=1 has
// still been applied. The handler used to run the whole bring-up fan-out before
// it appended to the config, and treat "no interface came up" as fatal — so an
// address that could not be brought up anywhere was rejected, and the same
// fan-out was what pushed the call past the caller's 30s deadline. The
// configuration is the record of intent; the IP monitor's ENFORCE pass is what
// puts the address on an interface. Committing first is what makes the returned
// status describe the committed state.
func TestAddIPToGroupCommitsWhenNoInterfaceComesUp(t *testing.T) {
	s := newAddIPTestServer(t)

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName: "group1",
		Ip:        "10.0.0.9/24",
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}

	if !resp.Success {
		t.Errorf("Success = false (%q); the config change is durable, so the "+
			"status must report it as applied", resp.Message)
	}
	if !slices.Contains(s.config.Groups["group1"], "10.0.0.9/24") {
		t.Errorf("group1 = %v, want it to contain 10.0.0.9/24", s.config.Groups["group1"])
	}

	// The failure to reach an interface is still reported, just not as fatal.
	if len(resp.Warnings) == 0 {
		t.Error("no warnings returned; the operator gets no indication the " +
			"address did not come up locally")
	}

	// And it is on disk, not only in memory. Read the file rather than calling
	// Load(), which is a deliberate no-op under PULSEHA_TEST.
	raw, err := os.ReadFile(config.CONFIG_LOCATION)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !slices.Contains(onDisk.Groups["group1"], "10.0.0.9/24") {
		t.Errorf("persisted group1 = %v, want it to contain 10.0.0.9/24", onDisk.Groups["group1"])
	}
}

// Regression for defect #39's mechanism. Client.Send puts a 30s deadline on
// every CLI call and group add-ip sets none of its own, while the fan-out costs
// ~4s per peer and ~28s with one peer unreachable. The fan-out must therefore
// not run on the caller's context, nor on the caller's clock.
func TestAddIPToGroupDoesNotWaitForTheRemoteFanOut(t *testing.T) {
	s := newAddIPTestServer(t, stalledPeer(t))

	// Stands in for the client's 30s deadline, scaled down.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := s.AddIPToGroup(ctx, &rpc.AddIPToGroupRequest{
		GroupName: "group1",
		Ip:        "10.0.0.9/24",
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned after %s; an unreachable peer must not hold the "+
			"caller past its deadline", elapsed.Round(time.Millisecond))
	}
	if !resp.Success {
		t.Errorf("Success = false (%q) because a peer was unreachable; the "+
			"config change is durable regardless", resp.Message)
	}
	if !slices.Contains(s.config.Groups["group1"], "10.0.0.9/24") {
		t.Errorf("group1 = %v, want it to contain 10.0.0.9/24", s.config.Groups["group1"])
	}
}

// A duplicate must still be rejected before anything is committed — the
// commit-first ordering must not turn the existing validation into a no-op.
func TestAddIPToGroupStillRejectsAnIPHeldByAnotherGroup(t *testing.T) {
	s := newAddIPTestServer(t)
	s.config.Groups["group2"] = []string{"10.0.0.9/24"}

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName: "group1",
		Ip:        "10.0.0.9/24",
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if resp.Success {
		t.Fatal("Success = true for an IP already in group2")
	}
	if slices.Contains(s.config.Groups["group1"], "10.0.0.9/24") {
		t.Errorf("group1 = %v, want the rejected IP absent", s.config.Groups["group1"])
	}
}
