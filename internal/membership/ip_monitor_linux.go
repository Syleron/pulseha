//go:build linux

package membership

import (
	"strings"
	"time"

	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/vishvananda/netlink"
)

// startMonitorLoop uses netlink address subscription for event-driven reconciliation
func (m *IPMonitor) monitorLoop() {
	updates := make(chan netlink.AddrUpdate, 32)
	if err := netlink.AddrSubscribe(updates, m.stopChan); err != nil {
		m.logger.Error("IP monitor: failed to subscribe to netlink addr updates", "error", err)
		return
	}

	m.logger.Info("IP monitor: netlink address subscription active")

	for {
		select {
		case <-m.stopChan:
			return
		case upd, ok := <-updates:
			if !ok {
				return
			}

			link, err := netlink.LinkByIndex(upd.LinkIndex)
			if err != nil || link == nil {
				continue
			}
			iface := link.Attrs().Name

			// Snapshot expected IPs for this interface and build monitored interfaces list
			m.RLock()
			expected := make([]string, len(m.expectedIPs[iface]))
			copy(expected, m.expectedIPs[iface])
			// Build list of interfaces that PulseHA manages (have groups assigned)
			monitoredInterfaces := make(map[string]bool)
			for ifn, ips := range m.expectedIPs {
				if len(ips) > 0 {
					monitoredInterfaces[ifn] = true
				}
			}
			// Also capture expected set across all interfaces to resolve correct iface
			allExpected := make(map[string]string) // ip(without mask)->iface
			for ifn, ips := range m.expectedIPs {
				for _, e := range ips {
					ipOnly := e
					if strings.Contains(e, "/") {
						ipOnly = strings.Split(e, "/")[0]
					}
					allExpected[ipOnly] = ifn
				}
			}
			m.RUnlock()

			// Skip interfaces that PulseHA is not configured to manage
			if !monitoredInterfaces[iface] {
				continue
			}

			changedIP := upd.LinkAddress.IP.String()
			// Construct netlink.Addr from update
			addrStr := upd.LinkAddress.String()
			addrObj, perr := netlink.ParseAddr(addrStr)
			if perr != nil {
				continue
			}

			// Evaluate local role
			watchCfg := m.members.Config()
			if watchCfg == nil {
				continue
			}
			localID, err := watchCfg.GetLocalNodeUUID()
			if err != nil {
				continue
			}
			localMember := m.members.GetMemberByID(localID)
			if localMember == nil {
				continue
			}
			// One read per event, for the same reason as in enforceExpectations.
			localClaim := localMember.Claim()

			if upd.NewAddr {
				// Address added
				if localClaim.Status != StatusActive {
					// Drop any VIP additions while passive
					_ = netlink.AddrDel(link, addrObj)
					m.logger.Info("IP monitor: dropped IP on passive node", "ip", changedIP, "iface", iface)
					continue
				}
				// Active: Move to correct interface if it's not already there
				if correctIface, ok := allExpected[changedIP]; ok && correctIface != iface {
					// Move to correct interface
					_ = netlink.AddrDel(link, addrObj)
					if targetLink, e := netlink.LinkByName(correctIface); e == nil {
						_ = netlink.AddrAdd(targetLink, addrObj)
					}
					m.logger.Info("IP monitor: moved IP to correct interface", "ip", changedIP, "from", iface, "to", correctIface)
				}
				continue
			}

			// Address removed: restore expected IPs ONLY if we're currently Active
			if len(expected) == 0 {
				m.logger.Debug("IP monitor: no expected IPs for removed address", "removedIP", changedIP, "iface", iface)
				continue
			}

			// Re-check current node status before attempting restore
			restoreCfg := m.members.Config()
			if restoreCfg == nil {
				m.logger.Debug("IP monitor: no config for restore check")
				continue
			}
			localID, err2 := restoreCfg.GetLocalNodeUUID()
			if err2 != nil {
				m.logger.Debug("IP monitor: failed to get local node ID for restore check", "error", err2)
				continue
			}
			currentMember := m.members.GetMemberByID(localID)
			if currentMember == nil {
				m.logger.Debug("IP monitor: local member not found for restore check")
				continue
			}
			currentClaim := currentMember.Claim()

			if currentClaim.Status != StatusActive {
				m.logger.Info("IP monitor: IP removed but node is not Active, NOT restoring", "ip", changedIP, "status", currentClaim.Status, "iface", iface)
				continue
			}

			// If an expected IP was removed and we're Active, immediately restore
			for _, exp := range expected {
				ipOnly := exp
				if strings.Contains(exp, "/") {
					ipOnly = strings.Split(exp, "/")[0]
				}
				if ipOnly == changedIP {
					// A release this node was told to perform is not an address
					// that has gone missing, even while the expectation set still
					// derives it from a config that has not caught up
					// (docs/TEST-PLAN.md defect #60).
					if m.restoreSuppressed(iface, exp) {
						m.logger.Info("IP monitor: expected IP was released on request; not restoring", "ip", exp, "iface", iface)
						break
					}
					m.logger.Warn("IP monitor: expected IP removed from Active node; restoring", "ip", exp, "iface", iface, "status", currentClaim.Status)
					m.restoreIP(iface, exp)
					break
				}
			}
		}
	}
}

// restoreIP attempts to restore an IP that was unexpectedly removed on Linux
func (m *IPMonitor) restoreIP(iface string, ip string) {
	m.logger.Debug("IP monitor restore: starting restore", "iface", iface, "ip", ip)

	link, err := netlink.LinkByName(iface)
	if err != nil {
		m.logger.Error("IP monitor restore: failed to get link", "iface", iface, "error", err)
		return
	}
	m.logger.Debug("IP monitor restore: got netlink interface", "iface", iface)

	// Determine CIDR if missing
	cidr := ip
	if !strings.Contains(ip, "/") {
		if strings.Contains(ip, ":") {
			cidr = ip + "/128"
		} else {
			cidr = ip + "/32"
		}
		m.logger.Debug("IP monitor restore: added CIDR notation", "originalIP", ip, "cidr", cidr)
	}

	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		m.logger.Error("IP monitor restore: failed to parse addr", "cidr", cidr, "error", err)
		return
	}
	m.logger.Debug("IP monitor restore: parsed address", "cidr", cidr)

	// Deliberately does not announce, unlike every other path that places an
	// address (docs/TEST-PLAN.md defect #33). This runs on the watcher goroutine,
	// whose netlink channel is 32 deep with no overflow handling, and an arping
	// costs about four seconds — so announcing here would drop address events
	// during exactly the churn that produces them, and a dropped removal is an
	// expected address never restored. That is #4/#8's failure, which is why every
	// announcement in this codebase is batched off a loop like this one.
	//
	// The cost of the silence is small here and only here: the address was taken
	// off this node and is going straight back onto it, so neighbours' ARP entries
	// still point at this node's MAC. A placement that genuinely moves an address
	// between nodes goes through the enforce pass or a bring-up RPC, both of which
	// announce.
	if err := netlink.AddrAdd(link, addr); err != nil {
		// The watcher restores on the removal event, so by the time it gets here
		// another writer may already have put the address back — an add that
		// fails with `file exists` did nothing because there was nothing left to
		// do (docs/TEST-PLAN.md defect #45).
		if network.AddrAddSatisfied(err, func() bool {
			ipOnly, _ := utils.GetCIDR(cidr)
			if ipOnly == nil {
				return false
			}
			ex, eIface, cerr := network.CheckIfIPExists(ipOnly.String())
			return cerr == nil && ex && eIface == iface
		}) {
			m.logger.Debug("IP monitor restore: expected IP was already back", "cidr", cidr, "iface", iface, "error", err)
			return
		}
		m.logger.Error("IP monitor restore: failed to add addr", "cidr", cidr, "iface", iface, "error", err)
		return
	}
	m.logger.Info("IP monitor restore: successfully restored expected IP", "iface", iface, "ip", ip)
}

// periodicReconcile runs a lightweight reconcile loop to enforce expected IPs
func (m *IPMonitor) periodicReconcile() {
	// Run every 30s; exit on stop
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stopChan:
			return
		case <-t.C:
			// Through the same gate as every other caller rather than calling the
			// pass directly, which is how a tick came to run beside a triggered
			// pass on a converging cluster — two dumps, two placement loops over
			// the same missing set, two GARP batches (docs/TEST-PLAN.md defect
			// #63). A tick landing on a pass in flight now queues the follow-up
			// this loop exists to provide, and the pass it queues is the same pass.
			m.TriggerEnforce()
		}
	}
}

// enforceExpectations ensures that the current local role and expectedIPs are reflected on interfaces
func (m *IPMonitor) enforceExpectations() {
	m.logger.Debug("ENFORCE: Starting enforceExpectations")

	// One config snapshot for the whole pass. The mode is consulted three times and
	// the node entry and groups once each, so reading them through separate bare
	// dereferences let a ConfigSync landing mid-tick send the pass down the
	// active-active branch while it resolved groups from the config that replaced it.
	cfg := m.members.Config()
	if cfg == nil {
		m.logger.Error("ENFORCE: no config")
		return
	}

	// Determine local role
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		m.logger.Error("ENFORCE: Failed to get local node ID", "error", err)
		return
	}
	member := m.members.GetMemberByID(localID)
	if member == nil {
		m.logger.Error("ENFORCE: Local member not found", "nodeID", localID)
		return
	}

	// One read of what this node asserts, for the whole pass.
	//
	// This used to read claim.Status directly, eight times, unlocked -- a data
	// race against every writer of it, and the reason `make testrace` went red as
	// soon as ./tests/unit/... entered a CI target (docs/TEST-PLAN.md #90): the
	// file is Linux-only, so it never compiled on a macOS -race run.
	//
	// Reading it once is not just about the lock. A pass that re-read the field
	// could decide "not Active, remove every floating IP" and then, further down,
	// log and act as though it were Active, because a demotion or promotion landed
	// in between -- deciding twice about one pass. That is the mismatch Claim
	// exists to prevent.
	claim := member.Claim()

	m.logger.Info("ENFORCE: Current node status and expectations", "nodeID", localID, "status", StatusToString(claim.Status))

	m.RLock()
	expectations := make(map[string][]string, len(m.expectedIPs))
	for iface, ips := range m.expectedIPs {
		cpy := make([]string, len(ips))
		copy(cpy, ips)
		expectations[iface] = cpy
	}
	m.RUnlock()

	// In active-active the cached set is not trustworthy on its own: several code
	// paths write it, and the one that matters here — a node that was the sole
	// active-passive Active before a mode switch — keeps the whole group until
	// something happens to recompute it. That node then re-adds all 201 addresses
	// every tick and the cluster never converges (docs/TEST-PLAN.md defects #2/#26).
	// Recomputing from the node's own assignments each tick makes the monitor
	// self-correcting regardless of which writer last touched the cache.
	if claim.Status == StatusActive && cfg.Pulse.Mode == "active-active" {
		expectations = m.deriveExpectedIPs(localID, member)
		m.UpdateExpectedIPsAll(expectations)
	}

	m.logger.Info("ENFORCE: Current expectations", "expectations", expectations)

	// Build snapshot of current interface assignments to avoid repeated netlink allocations during checks.
	ipInventory, invErr := network.BuildIPInventory()
	if invErr != nil {
		m.logger.Error("ENFORCE: Failed to build IP inventory snapshot", "error", invErr)
	}

	// Passive/Unknown/Maintenance: remove all floating IPs; Active: ensure missing are added
	if claim.Status != StatusActive {
		m.logger.Info("ENFORCE: Node is not Active, removing floating IPs", "status", StatusToString(claim.Status))

		// In active-active a non-Active status is a transient, not a demotion:
		// every eligible node is Active, so this is a missed health check or a
		// peer's stale broadcast. Dropping every cluster floating IP over one of
		// those took fifty addresses off a node that was only busy, and they were
		// off the cluster entirely until the coordinator noticed and re-placed
		// them (docs/TEST-PLAN.md defects #2/#26, #14). Release only what this
		// node is not assigned; if it really has failed, the coordinator reclaims
		// the rest after the failover limit and that path brings them down.
		if cfg.Pulse.Mode == "active-active" {
			m.releaseUnassignedIPs(localID, m.deriveExpectedIPs(localID, member))
			m.logger.Info("ENFORCE: Completed cleanup for non-Active active-active node")
			return
		}

		// CRITICAL: Passive nodes must remove ALL cluster floating IPs, not just expected IPs
		// This prevents split-brain IP conflicts when a node loses active status
		// Build a complete list of all floating IPs from cluster groups
		allClusterIPs := make(map[string][]string) // iface -> IPs

		// Get local node config to know which interfaces map to which groups
		localNodeCfg, ok := cfg.Nodes[localID]
		if ok && localNodeCfg != nil && localNodeCfg.IPGroups != nil {
			for iface, groups := range localNodeCfg.IPGroups {
				for _, groupName := range groups {
					if groupIPs, exists := cfg.Groups[groupName]; exists {
						allClusterIPs[iface] = append(allClusterIPs[iface], groupIPs...)
					}
				}
			}
		}

		m.logger.Info("ENFORCE: Passive node checking all cluster IPs for cleanup", "clusterIPs", allClusterIPs)

		// Remove any cluster floating IPs found on this passive node
		for iface, ips := range allClusterIPs {
			m.logger.Debug("ENFORCE: Checking interface for cleanup", "iface", iface, "clusterIPs", ips)
			for _, ip := range ips {
				ipOnly, _ := utils.GetCIDR(ip)
				if ipOnly == nil {
					m.logger.Debug("ENFORCE: Skipping invalid IP", "ip", ip)
					continue
				}
				var exists bool
				var foundIface string
				if ipInventory != nil {
					exists, foundIface, _ = ipInventory.Exists(ipOnly.String())
				} else {
					exists, foundIface, _ = network.CheckIfIPExists(ipOnly.String())
				}
				m.logger.Debug("ENFORCE: IP existence check", "ip", ipOnly.String(), "exists", exists, "foundIface", foundIface, "targetIface", iface)
				if exists && foundIface == iface {
					m.logger.Warn("ENFORCE: Removing stale floating IP from passive node", "ip", ip, "iface", iface, "status", StatusToString(claim.Status))
					if err := network.BringIPdown(iface, ip); err != nil {
						m.logger.Error("ENFORCE: Failed to remove floating IP from passive node", "ip", ip, "iface", iface, "error", err)
					} else {
						m.logger.Info("ENFORCE: Successfully removed floating IP from passive node", "ip", ip, "iface", iface)
					}
				} else {
					m.logger.Debug("ENFORCE: IP not found on target interface (nothing to remove)", "ip", ip, "exists", exists, "foundIface", foundIface)
				}
			}
		}
		m.logger.Info("ENFORCE: Completed cleanup for passive node")
		return
	}

	// Active node: bring up missing IPs
	m.logger.Info("ENFORCE: Node is Active, ensuring expected IPs are present", "status", StatusToString(claim.Status))
	for iface, ips := range expectations {
		var missing []string
		m.logger.Debug("ENFORCE: Checking interface for missing IPs", "iface", iface, "expectedIPs", ips)
		for _, ip := range ips {
			ipOnly, _ := utils.GetCIDR(ip)
			if ipOnly == nil {
				m.logger.Debug("ENFORCE: Skipping invalid IP", "ip", ip)
				continue
			}
			var exists bool
			var eIface string
			if ipInventory != nil {
				exists, eIface, _ = ipInventory.Exists(ipOnly.String())
			} else {
				exists, eIface, _ = network.CheckIfIPExists(ipOnly.String())
			}
			m.logger.Debug("ENFORCE: IP existence check for Active node", "ip", ipOnly.String(), "exists", exists, "foundIface", eIface, "targetIface", iface)
			if !exists || eIface != iface {
				missing = append(missing, ip)
				m.logger.Debug("ENFORCE: IP is missing and needs to be brought up", "ip", ip, "exists", exists, "foundIface", eIface)
			}
		}
		// An address this node was told to release is missing on purpose. The
		// expectation it is missing against is derived from the config, which on
		// the node that has just released is the thing still lagging, so without
		// this the pass hands the address straight back (docs/TEST-PLAN.md
		// defect #60).
		missing, released := m.restorableIPs(iface, missing)
		if len(released) > 0 {
			m.logger.Info("ENFORCE: not restoring floating IPs this node was told to release",
				"iface", iface, "count", len(released), "ips", released)
		}
		if len(missing) > 0 {
			m.logger.Info("ENFORCE: Bringing up missing IPs on Active node", "iface", iface, "missingIPs", missing, "status", StatusToString(claim.Status))

			// This pass placed addresses and announced none of them, which run 30
			// caught doing it for the ones that mattered most: node-1's final 72 of
			// a 288-address group came up here, live under a holder that had never
			// announced them (docs/TEST-PLAN.md defect #33, residual half).
			attempts, skipped, announceErr := placeMissingFloatingIPs(iface, missing,
				func(iface, ip string) error {
					m.logger.Info("ENFORCE: About to bring up IP on Active node", "ip", ip, "iface", iface, "status", StatusToString(claim.Status))
					return network.BringIPup(iface, ip)
				}, network.SendGARPBatch)

			for _, attempt := range attempts {
				if attempt.Err != nil {
					m.logger.Error("ENFORCE: Failed to bring up IP on Active node", "ip", attempt.IP, "iface", iface, "error", attempt.Err)
				} else {
					m.logger.Info("ENFORCE: Successfully brought up IP on Active node", "ip", attempt.IP, "iface", iface)
				}
			}
			// The addresses are up and serving whether or not the announcement
			// landed, so this is a warning and never changes what the pass did.
			if announceErr != nil {
				m.logger.Warn("ENFORCE: failed to announce some placed IPs", "iface", iface, "error", announceErr)
			}
			if len(skipped) > 0 {
				// On this logger rather than packages/network's, which nothing calls
				// SetLevel on — a line reported there cannot reach the journal at any
				// logging_level (#61's lesson, #33's positive control).
				m.logger.Debug("ENFORCE: skipped announcing addresses this node no longer holds",
					"iface", iface, "count", len(skipped), "of", len(missing))
			}
		} else {
			m.logger.Debug("ENFORCE: No missing IPs for interface", "iface", iface)
		}
	}

	// Active nodes in active-active must also give up what is no longer theirs.
	// The Active branch only ever added, so a node that handed addresses to a
	// peer kept serving them: after a mode switch the former sole Active sat at
	// 172 addresses against an expectation of 50, every one of the surplus also
	// up on its new owner (docs/TEST-PLAN.md defects #2/#26).
	//
	// This keys off the expectation set, which in this mode is recomputed from
	// the node's own assignments at the top of this function. An earlier attempt
	// at this pass was reverted because that record could not be trusted — a
	// node listed one address while serving a hundred the coordinator had given
	// it, so the pass tore down legitimate traffic. What made the record
	// reliable was fixing its writers: the mode switch now seeds the owner's
	// assignments on every node, and a busy coordinator is no longer declared
	// failed and its addresses re-placed behind its back.
	if cfg.Pulse.Mode == "active-active" {
		m.releaseUnassignedIPs(localID, expectations)
	}

	m.logger.Debug("ENFORCE: Completed enforceExpectations")
}

// releaseUnassignedIPs brings down cluster floating IPs this node is holding but
// is not expected to hold. Only configured group IPs are considered, so the
// node's own addresses are never touched.
//
// The decision is surplusFloatingIPs'; this supplies where each address actually
// is and applies the result. A node the config has no entry for is left alone:
// its expectation set would be empty for want of configuration, not because it
// should be serving nothing, and acting on that would tear down live traffic on
// a cluster mid-sync.
func (m *IPMonitor) releaseUnassignedIPs(localID string, expectations map[string][]string) {
	cfg := m.members.Config()
	if cfg == nil {
		return
	}
	if localNodeCfg, ok := cfg.Nodes[localID]; !ok || localNodeCfg == nil {
		return
	}

	// Deliberately a fresh inventory rather than the one enforceExpectations
	// built at the top of the tick. The Active branch's bring-up loop runs
	// between the two, so that snapshot can be seconds old and every address it
	// moved is one this pass would try to release from where it no longer is
	// (docs/TEST-PLAN.md defect #41).
	inventory, invErr := network.BuildIPInventory()
	if invErr != nil {
		m.logger.Error("ENFORCE: failed to build the IP inventory for the release pass",
			"error", invErr)
		return
	}

	locate := func(ip string) (string, bool) {
		ipOnly, _ := utils.GetCIDR(ip)
		if ipOnly == nil {
			return "", false
		}
		exists, foundIface, _ := inventory.Exists(ipOnly.String())
		return foundIface, exists
	}

	// A live check, not a read of the inventory above: this runs immediately
	// before each bring-down and its whole purpose is to be newer.
	stillHeld := func(iface, ip string) bool {
		ipOnly, _ := utils.GetCIDR(ip)
		if ipOnly == nil {
			return false
		}
		exists, foundIface, err := network.CheckIfIPExists(ipOnly.String())
		return err == nil && exists && foundIface == iface
	}

	surplus := surplusFloatingIPs(cfg.Groups, expectations, locate)
	for iface, ips := range surplus {
		m.logger.Warn("ENFORCE: releasing floating IPs this node is no longer assigned",
			"iface", iface, "count", len(ips))
	}

	attempts := releaseSurplusFloatingIPs(surplus, stillHeld, network.BringIPdown)
	for _, attempt := range attempts {
		switch {
		case attempt.Vanished:
			// Nothing to do and nothing wrong: the address left before this pass
			// reached it, which is the state the pass was trying to produce.
			m.logger.Debug("ENFORCE: unassigned floating IP had already gone",
				"ip", attempt.IP, "iface", attempt.Iface)
		case attempt.Err != nil:
			m.logger.Error("ENFORCE: failed to release unassigned floating IP",
				"ip", attempt.IP, "iface", attempt.Iface, "error", attempt.Err)
		}
	}

	// The bring-downs above went straight to the kernel, so nothing has told this
	// node's own assignment list that the addresses are gone. Without this the
	// list only ever grows: a released address is still reported as held and
	// still counted as this node's load (docs/TEST-PLAN.md defect #58).
	if released := releasedForBookkeeping(attempts); len(released) > 0 {
		if localMember := m.members.GetMemberByID(localID); localMember != nil {
			localMember.RemoveActiveIPs(released)
			m.logger.Info("ENFORCE: dropped released floating IPs from this node's assignments",
				"count", len(released))
		}
	}
}
