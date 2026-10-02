package server

import (
	"context"
	"os"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/rpc"
	"github.com/vishvananda/netlink"
)

// Opt-in: run the compiled test in an isolated Linux container with NET_ADMIN
// and arping. Never touches a host interface; all links below belong to the test.
func TestIPTransferKernel(t *testing.T) {
	if os.Getenv("PULSEHA_TRANSFER_KERNEL_TEST") != "1" {
		t.Skip("requires isolated NET_ADMIN Linux container")
	}
	for _, name := range []string{"xfer-src", "xfer-dst"} {
		link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name}, PeerName: name + "p"}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(link) })
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
		peer, err := netlink.LinkByName(name + "p")
		if err != nil {
			t.Fatal(err)
		}
		if err = netlink.LinkSetUp(peer); err != nil {
			t.Fatal(err)
		}
	}
	ips := []string{"10.254.40.1/24", "10.254.40.2/24"}
	source := newVotingServer(t, "b")
	dest := newVotingServer(t, "c")
	coordinator := newVotingServer(t, "a")
	source.ipMonitor = nil
	dest.ipMonitor = nil // test the RPC/verification boundary, not concurrent reconciliation
	for _, s := range []*Server{source, dest, coordinator} {
		s.config.Groups["group"] = ips
	}
	coordinator.config.Nodes["b"].IPGroups = map[string][]string{"xfer-src": {"group"}}
	coordinator.config.Nodes["c"].IPGroups = map[string][]string{"xfer-dst": {"group"}}
	setVoterAddress(t, coordinator, "b", serveVoter(t, source))
	setVoterAddress(t, coordinator, "c", serveVoter(t, dest))
	for _, ip := range ips {
		if err := network.BringIPup("xfer-src", ip); err != nil {
			t.Fatal(err)
		}
	}
	source.memberList.GetMemberByID("b").SetClaim(membership.Claim{Status: membership.StatusActive, ActiveIPs: ips})
	// A source address on the wrong interface cannot be waved through as absent.
	resp, err := source.BringDownIP(context.Background(), &rpc.DownIpRequest{Iface: "xfer-dst", Ips: ips})
	if err != nil || resp.Success || len(resp.FailedIps) != 2 {
		t.Fatalf("source elsewhere accepted: %v %v", resp, err)
	}
	if err = coordinator.OrchestrateIPFailover("b", "c", ips); err != nil {
		t.Fatal(err)
	}
	for _, ip := range ips {
		held, iface, e := network.CheckIfIPExists(ip)
		if e != nil || !held || iface != "xfer-dst" {
			t.Fatalf("address %s: %v %s %v", ip, held, iface, e)
		}
	}
	// Lost acknowledgement/retry: the already acquired target is idempotent. A
	// source-scoped retry is conservatively refused when this shared namespace
	// sees the destination address; real nodes have separate interface inventories.
	if err = coordinator.OrchestrateIPFailover("c", "c", ips); err != nil {
		t.Fatal(err)
	}
	down, err := dest.BringDownIP(context.Background(), &rpc.DownIpRequest{Iface: "xfer-dst", Ips: ips})
	if err != nil || !down.Success || len(down.VerifiedIps) != 2 {
		t.Fatalf("release: %v %v", down, err)
	}
	for _, ip := range ips {
		held, _, e := network.CheckIfIPExists(ip)
		if e != nil || held {
			t.Fatalf("address retained: %s %v", ip, e)
		}
	}
}
