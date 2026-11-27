package server

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/rpc"
	"github.com/vishvananda/netlink"
)

// The unit tests replace the probe, so they cannot show that the argv is one
// arping accepts, that an address in use is answered for, or that the exit
// codes mean what interpretDuplicateProbe reads into them. This does, on a veth
// pair: one end stands in for the segment the cluster's interface is on, the
// other for the host that already has the address.
//
// Opt-in, on the same terms as TestIPTransferKernel: an isolated Linux
// container with NET_ADMIN, NET_RAW and arping. Never touches a host interface.
func TestDuplicateAddressProbeKernel(t *testing.T) {
	if os.Getenv("PULSEHA_TRANSFER_KERNEL_TEST") != "1" {
		t.Skip("requires isolated NET_ADMIN Linux container")
	}

	const (
		ours   = "dup-ours"
		theirs = "dup-theirs"
		taken  = "10.254.41.1"
		free   = "10.254.41.2"
	)
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: ours}, PeerName: theirs}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })
	peer, err := netlink.LinkByName(theirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []netlink.Link{link, peer} {
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := network.BringIPup(theirs, taken+"/24"); err != nil {
		t.Fatal(err)
	}

	// The interface asking holds no address at all, which is the ordinary state
	// of one that carries only floating IPs. The probe has to work from there.
	holder, held, err := network.ProbeAddress(ours, taken+"/24")
	if err != nil {
		t.Fatalf("probe of an address in use: %v", err)
	}
	if !held {
		t.Fatal("an address the other end holds was reported free")
	}
	if want := strings.ToLower(peer.Attrs().HardwareAddr.String()); holder != want {
		t.Errorf("holder = %q, want the other end's MAC %q", holder, want)
	}

	holder, held, err = network.ProbeAddress(ours, free+"/24")
	if err != nil {
		t.Fatalf("probe of a free address: %v", err)
	}
	if held || holder != "" {
		t.Fatalf("a free address was reported in use by %q", holder)
	}

	// Both ends are in this namespace, so the taken address is one this node
	// holds, and that is not "elsewhere" whatever the wire says.
	if _, held, err := network.AddressHeldElsewhere(ours, taken+"/24"); err != nil || held {
		t.Fatalf("an address this node holds was reported held elsewhere: held=%v err=%v", held, err)
	}

	// And through the RPC. The handler's probe consults this node's own
	// addresses first, which in a shared namespace would hide the other end, so
	// the wire probe is put in its place directly.
	s := newAddIPTestServer(t)
	s.config.Nodes[s.config.Pulse.LocalNode].IPGroups = map[string][]string{ours: {"group1"}}
	s.probeAddedIP = network.ProbeAddress
	s.placeAddedIP = func(string, string) (bool, string) { return false, "" }

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{GroupName: "group1", Ip: taken + "/24", VerifyUnused: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Success || slices.Contains(s.config.Groups["group1"], taken+"/24") {
		t.Fatalf("an address in use was added: success=%v message=%q", resp.Success, resp.Message)
	}

	resp, err = s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{GroupName: "group1", Ip: free + "/24", VerifyUnused: true})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Success || !slices.Contains(s.config.Groups["group1"], free+"/24") {
		t.Fatalf("a free address was refused: success=%v message=%q", resp.Success, resp.Message)
	}
}
