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
			localID, err := m.members.config.GetLocalNodeUUID()
			if err != nil {
				continue
			}
			localMember := m.members.GetMemberByID(localID)
			if localMember == nil {
				continue
			}

			if upd.NewAddr {
				// Address added
				if localMember.Status != StatusActive {
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
			localID, err2 := m.members.config.GetLocalNodeUUID()
			if err2 != nil {
				m.logger.Debug("IP monitor: failed to get local node ID for restore check", "error", err2)
				continue
			}
			currentMember := m.members.GetMemberByID(localID)
			if currentMember == nil {
				m.logger.Debug("IP monitor: local member not found for restore check")
				continue
			}

			if currentMember.Status != StatusActive {
				m.logger.Info("IP monitor: IP removed but node is not Active, NOT restoring", "ip", changedIP, "status", currentMember.Status, "iface", iface)
				continue
			}

			// If an expected IP was removed and we're Active, immediately restore
			for _, exp := range expected {
				ipOnly := exp
				if strings.Contains(exp, "/") {
					ipOnly = strings.Split(exp, "/")[0]
				}
				if ipOnly == changedIP {
					m.logger.Warn("IP monitor: expected IP removed from Active node; restoring", "ip", exp, "iface", iface, "status", currentMember.Status)
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

	if err := netlink.AddrAdd(link, addr); err != nil {
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
			m.enforceExpectations()
		}
	}
}

// enforceExpectations ensures that the current local role and expectedIPs are reflected on interfaces
func (m *IPMonitor) enforceExpectations() {
	m.logger.Debug("ENFORCE: Starting enforceExpectations")

	// Determine local role
	localID, err := m.members.config.GetLocalNodeUUID()
	if err != nil {
		m.logger.Error("ENFORCE: Failed to get local node ID", "error", err)
		return
	}
	member := m.members.GetMemberByID(localID)
	if member == nil {
		m.logger.Error("ENFORCE: Local member not found", "nodeID", localID)
		return
	}

	m.logger.Info("ENFORCE: Current node status and expectations", "nodeID", localID, "status", StatusToString(member.Status))

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
	if member.Status == StatusActive && m.members.config.Pulse.Mode == "active-active" {
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
	if member.Status != StatusActive {
		m.logger.Info("ENFORCE: Node is not Active, removing floating IPs", "status", StatusToString(member.Status))

		// CRITICAL: Passive nodes must remove ALL cluster floating IPs, not just expected IPs
		// This prevents split-brain IP conflicts when a node loses active status
		// Build a complete list of all floating IPs from cluster groups
		allClusterIPs := make(map[string][]string) // iface -> IPs

		// Get local node config to know which interfaces map to which groups
		localNodeCfg, ok := m.members.config.Nodes[localID]
		if ok && localNodeCfg != nil && localNodeCfg.IPGroups != nil {
			for iface, groups := range localNodeCfg.IPGroups {
				for _, groupName := range groups {
					if groupIPs, exists := m.members.config.Groups[groupName]; exists {
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
					m.logger.Warn("ENFORCE: Removing stale floating IP from passive node", "ip", ip, "iface", iface, "status", StatusToString(member.Status))
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
	m.logger.Info("ENFORCE: Node is Active, ensuring expected IPs are present", "status", StatusToString(member.Status))
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
		if len(missing) > 0 {
			m.logger.Info("ENFORCE: Bringing up missing IPs on Active node", "iface", iface, "missingIPs", missing, "status", StatusToString(member.Status))
			for _, ip := range missing {
				m.logger.Info("ENFORCE: About to bring up IP on Active node", "ip", ip, "iface", iface, "status", StatusToString(member.Status))
				if err := network.BringIPup(iface, ip); err != nil {
					m.logger.Error("ENFORCE: Failed to bring up IP on Active node", "ip", ip, "iface", iface, "error", err)
				} else {
					m.logger.Info("ENFORCE: Successfully brought up IP on Active node", "ip", ip, "iface", iface)
				}
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
	if m.members.config.Pulse.Mode == "active-active" {
		m.releaseUnassignedIPs(localID, expectations, ipInventory)
	}

	m.logger.Debug("ENFORCE: Completed enforceExpectations")
}

// releaseUnassignedIPs brings down cluster floating IPs held on an interface
// that this node is not assigned. Only configured group IPs are considered, so
// the node's own addresses are never touched.
func (m *IPMonitor) releaseUnassignedIPs(localID string, expectations map[string][]string, ipInventory *network.IPInventory) {
	localNodeCfg, ok := m.members.config.Nodes[localID]
	if !ok || localNodeCfg == nil {
		return
	}

	for iface, groups := range localNodeCfg.IPGroups {
		expected := make(map[string]bool, len(expectations[iface]))
		for _, ip := range expectations[iface] {
			expected[ip] = true
		}

		var surplus []string
		for _, groupName := range groups {
			for _, ip := range m.members.config.Groups[groupName] {
				if expected[ip] {
					continue
				}
				ipOnly, _ := utils.GetCIDR(ip)
				if ipOnly == nil {
					continue
				}
				var exists bool
				var foundIface string
				if ipInventory != nil {
					exists, foundIface, _ = ipInventory.Exists(ipOnly.String())
				} else {
					exists, foundIface, _ = network.CheckIfIPExists(ipOnly.String())
				}
				if exists && foundIface == iface {
					surplus = append(surplus, ip)
				}
			}
		}

		if len(surplus) == 0 {
			continue
		}
		m.logger.Warn("ENFORCE: releasing floating IPs this node is no longer assigned",
			"iface", iface, "count", len(surplus))
		for _, ip := range surplus {
			if err := network.BringIPdown(iface, ip); err != nil {
				m.logger.Error("ENFORCE: failed to release unassigned floating IP",
					"ip", ip, "iface", iface, "error", err)
			}
		}
	}
}
