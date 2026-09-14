package server

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/syleron/pulseha/rpc"
)

// Regression for docs/TEST-PLAN.md defect #114, at the site that writes the
// config.
//
// A join carrying no bind address used to be recorded verbatim: the joinee wrote
// `bind_address: ""` into config.Nodes, propagated it to every peer, and reported
// success. `pulsectl status` then showed the member as `Address: :8080  Status:
// Unknown` and the cluster degraded, with nothing connecting that to the join
// that had just claimed to work.
func TestAJoinWithNoBindAddressIsRefused(t *testing.T) {
	s := newRemoveIPTestServer(t)

	for _, bind := range []string{"", "   ", "\t\n"} {
		resp, err := s.HandleNodeJoin(context.Background(), &rpc.JoinRequest{
			Hostname: "MC-LB-3-node-2",
			NodeId:   "would-be-node",
			BindIp:   bind,
			BindPort: "9083",
			Token:    "irrelevant",
		})
		if err != nil {
			t.Fatalf("HandleNodeJoin: %v", err)
		}
		if resp.Success {
			t.Errorf("bind %q accepted; a member with no address is one nobody can reach", bind)
		}
		if !strings.Contains(resp.Message, "bind address") {
			t.Errorf("message = %q, want it to name the problem", resp.Message)
		}
	}
}

// Refused before anything is mutated. The write site is past a member-list
// insertion, so rejecting there would leave the member behind — the request has
// to be turned away while turning it away is still free.
func TestARefusedJoinLeavesNoTrace(t *testing.T) {
	s := newRemoveIPTestServer(t)
	before := s.memberList.GetMemberCount()

	if _, err := s.HandleNodeJoin(context.Background(), &rpc.JoinRequest{
		Hostname: "MC-LB-3-node-2",
		NodeId:   "would-be-node",
		BindIp:   "",
		BindPort: "9083",
		Token:    "irrelevant",
	}); err != nil {
		t.Fatalf("HandleNodeJoin: %v", err)
	}

	if got := s.memberList.GetMemberCount(); got != before {
		t.Errorf("member count %d -> %d; a refused join added a member", before, got)
	}
	s.RLock()
	_, recorded := s.config.Nodes["would-be-node"]
	s.RUnlock()
	if recorded {
		t.Error("a refused join was written into config.Nodes")
	}
}

// localAddrToward is what supplies the address when --bind-ip is omitted: the
// near end of a connection to the cluster, which is the one answer that cannot
// disagree with reality, since peers reach this node over the path it reaches
// them.
func TestLocalAddrTowardReportsTheNearEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	got, err := localAddrToward(ln.Addr().String())
	if err != nil {
		t.Fatalf("localAddrToward: %v", err)
	}
	if got != "127.0.0.1" {
		t.Errorf("localAddrToward = %q, want the local end of the connection (127.0.0.1)", got)
	}
	if ip := net.ParseIP(got); ip == nil {
		t.Errorf("localAddrToward = %q, which is not an address the config can use", got)
	}
}

// A node that cannot reach the cluster fails here, which is the right moment: it
// could not have joined anyway, and an error now is clearer than an unreachable
// member later.
//
// The unreachable target is a loopback port that was listening and is not any
// more, so the connection is refused immediately on every host. The first draft
// of this used TEST-NET-1 (192.0.2.0/24, reserved for documentation and
// guaranteed not to route) and it **passed the wrong way** on the machine it was
// written on: something on that network answered, localAddrToward returned
// 10.200.110.82, and the test failed. An address block being unroutable by
// standard is not the same as it being unroutable from here -- which is the same
// environment dependence that let #110 hide behind a working /dev/log.
func TestLocalAddrTowardFailsWhenTheClusterIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	closed := ln.Addr().String()
	ln.Close()

	if got, err := localAddrToward(closed); err == nil {
		t.Errorf("a refused connection produced %q rather than an error", got)
	}
}
