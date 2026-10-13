package server

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/rpc"
)

const ipVerificationVersion = 1

func canonicalTransferIPs(ips []string) ([]string, error) {
	out := make([]string, 0, len(ips))
	seen := map[string]bool{}
	for _, ip := range ips {
		p, err := netip.ParsePrefix(ip)
		if err != nil {
			a, e := netip.ParseAddr(ip)
			if e != nil {
				return nil, fmt.Errorf("invalid address %q", ip)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		key := p.String()
		if !seen[key] {
			out = append(out, key)
			seen[key] = true
		}
	}
	return out, nil
}

// Read a fresh inventory AFTER the operation. Release means absent on every
// interface; acquisition means present on the requested interface. A successful
// syscall, a stale preflight snapshot, or a failed inventory is not confirmation.
func verifyIPBatch(iface string, ips []string, up bool, lookup func(string) (bool, string, error)) (verified, failed []string) {
	for _, ip := range ips {
		p, err := netip.ParsePrefix(ip)
		if err != nil || lookup == nil {
			failed = append(failed, ip)
			continue
		}
		held, where, err := lookup(p.Addr().String())
		if err == nil && ((!up && !held) || (up && held && where == iface)) {
			verified = append(verified, ip)
		} else {
			failed = append(failed, ip)
		}
	}
	return
}

type ipStateLookup func(string) (bool, string, error)

func (s *Server) verifyLocalIPBatch(iface string, ips []string, up bool) ([]string, []string) {
	var lookup ipStateLookup
	if s.ipVerificationSnapshot != nil {
		var err error
		lookup, err = s.ipVerificationSnapshot()
		if err != nil {
			return nil, append([]string(nil), ips...)
		}
	} else {
		inv, err := network.BuildIPInventory()
		if err != nil {
			return nil, append([]string(nil), ips...)
		}
		lookup = inv.Exists
	}
	return verifyIPBatch(iface, ips, up, lookup)
}

// Validate the complete partition of requested addresses before trusting even a
// partial result. Old peers' Success=true is not a verification acknowledgement.
func validatedIPResult(ips []string, success bool, version uint32, verified, failed []string, message string) ([]string, error) {
	want, err := canonicalTransferIPs(ips)
	if err != nil {
		return nil, err
	}
	if version != ipVerificationVersion {
		return nil, fmt.Errorf("peer does not provide verified IP results (version %d)", version)
	}
	expected := map[string]bool{}
	seen := map[string]bool{}
	for _, ip := range want {
		expected[ip] = true
	}
	for _, group := range [][]string{verified, failed} {
		for _, ip := range group {
			c, e := canonicalTransferIPs([]string{ip})
			if e != nil || len(c) != 1 || !expected[c[0]] || seen[c[0]] {
				return nil, fmt.Errorf("invalid or duplicate IP result %q", ip)
			}
			seen[c[0]] = true
		}
	}
	if len(seen) != len(expected) {
		return nil, errors.New("peer omitted requested IP results")
	}
	confirmed, _ := canonicalTransferIPs(verified)
	if !success || len(failed) > 0 {
		return confirmed, fmt.Errorf("IP operation incomplete: %s (%d unverified)", message, len(failed))
	}
	return confirmed, nil
}

// Record only addresses confirmed by the handler, even when another address in
// the batch failed. Never roll back a partial acquisition by blindly reactivating
// the source: that can put the same address on both nodes.
func (s *Server) recordVerifiedIPs(nodeID string, ips []string, up bool) {
	if len(ips) == 0 {
		return
	}
	m := s.memberList.GetMemberByID(nodeID)
	if m == nil {
		return
	}
	if !up {
		absent := map[netip.Addr]bool{}
		for _, ip := range ips {
			p, err := netip.ParsePrefix(ip)
			if err == nil {
				absent[p.Addr()] = true
			}
		}
		m.UpdateClaim(func(c membership.Claim) (membership.Claim, bool) {
			remaining := make([]string, 0, len(c.ActiveIPs))
			for _, ip := range c.ActiveIPs {
				normalized, err := canonicalTransferIPs([]string{ip})
				if err != nil {
					remaining = append(remaining, ip)
					continue
				}
				p, _ := netip.ParsePrefix(normalized[0])
				if !absent[p.Addr()] {
					remaining = append(remaining, ip)
				}
			}
			c.ActiveIPs = remaining
			return c, true
		})
		return
	}
	m.UpdateClaim(func(c membership.Claim) (membership.Claim, bool) { return c.WithAddresses(ips...), true })
}

func (s *Server) transferIPBatch(ctx context.Context, cfg *config.Config, nodeID, iface string, ips []string, up bool) error {
	node := cfg.Nodes[nodeID]
	if node == nil {
		return fmt.Errorf("node configuration not found: %s", nodeID)
	}
	var remote rpc.ServerClient
	if nodeID != cfg.Pulse.LocalNode {
		c, err := client.New()
		if err != nil {
			return err
		}
		defer c.Close()
		if err = s.dialPeer(c, node.IP, node.Port); err != nil {
			return err
		}
		remote = c.Server()
	}
	var verified []string
	var err error
	if up {
		req := &rpc.UpIpRequest{Iface: iface, Ips: ips}
		var resp *rpc.UpIpResponse
		if remote != nil {
			resp, err = remote.BringUpIP(ctx, req)
		} else {
			resp, err = s.BringUpIP(ctx, req)
		}
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("empty bring-up response")
		}
		verified, err = validatedIPResult(ips, resp.Success, resp.VerificationVersion, resp.VerifiedIps, resp.FailedIps, resp.Message)
	} else {
		req := &rpc.DownIpRequest{Iface: iface, Ips: ips}
		var resp *rpc.DownIpResponse
		if remote != nil {
			resp, err = remote.BringDownIP(ctx, req)
		} else {
			resp, err = s.BringDownIP(ctx, req)
		}
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("empty bring-down response")
		}
		verified, err = validatedIPResult(ips, resp.Success, resp.VerificationVersion, resp.VerifiedIps, resp.FailedIps, resp.Message)
	}
	s.recordVerifiedIPs(nodeID, verified, up)
	return err
}

func (s *Server) transferInterfaces(cfg *config.Config, nodeID string, plan map[string][]string, up bool) error {
	// Bounded per-interface operations. Sort for repeatable partial outcomes and
	// stop on failure; a retry re-verifies already completed addresses idempotently.
	ifaces := make([]string, 0, len(plan))
	for iface := range plan {
		ifaces = append(ifaces, iface)
	}
	sort.Strings(ifaces)
	for _, iface := range ifaces {
		timeout := membership.DemotionTimeoutFor(len(plan[iface]))
		if up {
			timeout = bringUpTimeoutFor(len(plan[iface]))
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := s.transferIPBatch(ctx, cfg, nodeID, iface, plan[iface], up)
		cancel()
		if err != nil {
			return fmt.Errorf("node %s interface %s: %w", nodeID, iface, err)
		}
	}
	return nil
}

// planIPTransfer rejects incomplete or ambiguous configuration before any node
// is mutated. A request must map every address to exactly one interface per node.
func planIPTransfer(cfg *config.Config, nodeID string, ips []string) (map[string][]string, error) {
	node := cfg.Nodes[nodeID]
	if node == nil {
		return nil, fmt.Errorf("node configuration not found: %s", nodeID)
	}
	plan := map[string][]string{}
	for _, ip := range ips {
		matches := map[string]bool{}
		for iface, groups := range node.IPGroups {
			for _, g := range groups {
				configured, err := canonicalTransferIPs(cfg.Groups[g])
				if err != nil {
					return nil, err
				}
				for _, candidate := range configured {
					if candidate == ip && iface != "" {
						matches[iface] = true
					}
				}
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("address %s maps to %d interfaces on node %s", ip, len(matches), nodeID)
		}
		for iface := range matches {
			plan[iface] = append(plan[iface], ip)
		}
	}
	return plan, nil
}
