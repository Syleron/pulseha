package server

import (
	"fmt"
	"net"

	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
)

// addedIPHeldElsewhere is AddIPToGroup's check that an address new to the
// configuration does not already belong to another host on the segment. It runs
// when the request sets verify_unused.
//
// refusal is the message to fail the request with. warning is for a probe that
// could not give an answer: the add goes ahead and the caller is told the check
// was not made.
//
// Why here. AddIPToGroup is the one point where an address enters the cluster,
// and nothing after it asks whether the address was free to take: the bring-up
// places it and the announcement tells every neighbour this node now answers
// for it. Given the address of a machine that is in service, that takes the
// machine off the network, and removing the floating IP again does not put it
// back, because neighbours keep this node's MAC until their ARP entries age
// out. The appliance's own check for this covers static addresses only, and a
// configuration restore or a change pushed from elsewhere reaches this RPC
// without going through any check at all.
//
// Why only here. Every other bring-up moves an address the cluster already
// owns, and there the previous holder is expected to be answering until the
// announcement lands. Probing a failover would delay it and, on a positive
// answer, refuse the transfer the cluster exists to make.
//
// Why only when asked. The answer comes from ARP, and ARP is only evidence of
// a holder on a segment where hosts answer for themselves. A cloud provider's
// virtual network answers every request from the fabric, as does a router
// running proxy ARP for the subnet, and there every address would be refused.
// The daemon cannot tell which kind of network it is on. Its caller can, so the
// caller decides, and a request that does not ask behaves exactly as before.
//
// A probe that fails does not refuse the address. This is a safety net, and a
// net that blocks every add whenever arping is missing or the link is down
// would be worse than the fault it catches.
//
// Called before s.Lock(), and it must stay that way: the probe listens for
// about a second, and AddIPToGroup's own comments record what holding the
// server lock across an arping costs every other RPC. The configuration can
// change between this read and the handler's, which is harmless: the handler
// validates the request again under the lock, and the answer from the wire does
// not depend on the configuration.
func (s *Server) addedIPHeldElsewhere(groupName, ip string) (refusal, warning string) {
	iface, target, probe := s.addedIPProbeTarget(groupName, ip)
	if !probe {
		return "", ""
	}

	check := s.probeAddedIP
	if check == nil {
		check = network.AddressHeldElsewhere
	}
	holder, held, err := check(iface, target)
	if err != nil {
		s.logger.Warn("AddIPToGroup: could not check whether the address is already in use",
			"iface", iface, "ip", target, "error", err)
		return "", fmt.Sprintf("Could not check whether %s is already in use on %s: %v", target, iface, err)
	}
	if !held {
		return "", ""
	}

	by := "another host"
	if holder != "" {
		by = "the host at " + holder
	}
	s.logger.Warn("AddIPToGroup: refusing an address another host answers for",
		"group", groupName, "iface", iface, "ip", target, "holder", holder)
	return fmt.Sprintf("IP %s is already in use on %s by %s; not added to group %s. "+
		"Adding it would take that host off the network. If the address is meant to move here, "+
		"remove it from that host first.", target, iface, by, groupName), ""
}

// addedIPProbeTarget decides whether an add needs the probe, and against which
// interface.
//
// probe is false whenever AddIPToGroup will not place a new address from this
// request, so that the handler's own answer is the one the caller gets:
//
//   - no cluster, no such group, an address that does not parse: the handler
//     rejects these with its own message.
//   - an address already in the configuration. That is either the idempotent
//     re-add the appliance makes for every floating IP on each daemon start,
//     which must stay as cheap as it is, or a clash with another group, which
//     the handler reports.
//   - a group this node carries on no interface. There is no segment to ask
//     from here; the node that does carry it runs the same check when the add
//     is made there.
//
// The probe goes out on this node's interface whichever node will end up
// holding the address: the nodes that carry a group are on the same segment,
// so one of them asking is enough.
func (s *Server) addedIPProbeTarget(groupName, ip string) (iface, target string, probe bool) {
	s.RLock()
	defer s.RUnlock()

	if s.config == nil || !s.config.ClusterCheck() {
		return "", "", false
	}
	if _, exists := s.config.Groups[groupName]; !exists {
		return "", "", false
	}

	// The same defaulting the handler applies, so "already configured" is
	// decided on the string the handler would store.
	configured := ip
	var parsed net.IP
	switch {
	case utils.IsCIDR(ip):
		parsed, _ = utils.GetCIDR(ip)
	case utils.IsIPv4(ip):
		parsed = net.ParseIP(ip)
		configured = ip + "/32"
	case utils.IsIPv6(ip):
		parsed = net.ParseIP(ip)
		configured = ip + "/128"
	}
	if parsed == nil {
		return "", "", false
	}
	for _, ips := range s.config.Groups {
		for _, existing := range ips {
			if existing == configured {
				return "", "", false
			}
		}
	}

	local, ok := s.config.Nodes[s.config.Pulse.LocalNode]
	if !ok || local == nil {
		return "", "", false
	}
	for candidate, groups := range local.IPGroups {
		for _, g := range groups {
			if g == groupName {
				return candidate, parsed.String(), true
			}
		}
	}
	return "", "", false
}
