package membership

import (
	"fmt"
	"net"
	"slices"
	"sort"
	"sync"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
)

// IPMonitor monitors IP addresses on interfaces and ensures they match the expected configuration
type IPMonitor struct {
	sync.RWMutex
	members     *MemberList
	logger      *log.Logger
	expectedIPs map[string][]string // map[interface][]ips
	stopChan    chan struct{}
	stopOnce    sync.Once
	done        chan struct{}
}

// NewIPMonitor creates a new IP monitor
func NewIPMonitor(members *MemberList, logger *log.Logger) *IPMonitor {
	return &IPMonitor{
		members:     members,
		logger:      logger,
		expectedIPs: make(map[string][]string),
		stopChan:    make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Start begins monitoring IP addresses
func (m *IPMonitor) Start() error {
	if m == nil {
		return nil
	}
	m.Lock()
	defer m.Unlock()

	// Initialize the expected IPs from the current member
	if err := m.initializeExpectedIPs(); err != nil {
		return fmt.Errorf("failed to initialize expected IPs: %v", err)
	}

	// Start platform-specific event monitoring (pure event-driven)
	go m.monitorLoop()
	// Start periodic reconcile as a safety net
	go m.periodicReconcile()

	m.logger.Info("IP monitor started")
	return nil
}

// TriggerEnforce performs an immediate expectations check asynchronously.
func (m *IPMonitor) TriggerEnforce() {
	if m == nil {
		return
	}
	m.logger.Debug("TRIGGER: TriggerEnforce called")
	select {
	case <-m.stopChan:
		m.logger.Debug("TRIGGER: Skipping enforce - monitor stopped")
		return
	default:
		m.logger.Debug("TRIGGER: Launching enforceExpectations goroutine")
		go m.enforceExpectations()
	}
}

// Stop stops the IP monitor
func (m *IPMonitor) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopChan)
		m.logger.Info("IP monitor stopped")
	})
}

// UpdateExpectedIPs updates the list of expected IPs for an interface
func (m *IPMonitor) UpdateExpectedIPs(iface string, ips []string) {
	if m == nil {
		return
	}
	m.Lock()
	defer m.Unlock()

	// Create a copy of the IPs slice to avoid external modifications
	ipsCopy := make([]string, len(ips))
	copy(ipsCopy, ips)
	slices.Sort(ipsCopy)

	m.expectedIPs[iface] = ipsCopy
	m.logger.Info("Updated expected IPs", "iface", iface, "ips", ips)
	m.TriggerEnforce()
}

// UpdateExpectedIPsAll replaces the whole expectation map.
//
// Deliberately does not TriggerEnforce: the caller is the enforce loop itself,
// which is about to act on this set, and re-arming from inside it would spin.
func (m *IPMonitor) UpdateExpectedIPsAll(expected map[string][]string) {
	if m == nil {
		return
	}
	m.Lock()
	defer m.Unlock()

	replacement := make(map[string][]string, len(expected))
	for iface, ips := range expected {
		ipsCopy := make([]string, len(ips))
		copy(ipsCopy, ips)
		slices.Sort(ipsCopy)
		replacement[iface] = ipsCopy
	}
	m.expectedIPs = replacement
}

// AddExpectedIPs adds IPs to the expected list for an interface
func (m *IPMonitor) AddExpectedIPs(iface string, ips []string) {
	if m == nil {
		return
	}
	m.Lock()
	defer m.Unlock()

	m.expectedIPs[iface] = append(m.expectedIPs[iface], ips...)
	slices.Sort(m.expectedIPs[iface])
	m.expectedIPs[iface] = slices.Compact(m.expectedIPs[iface])

	m.logger.Info("Added IPs to interface", "iface", iface, "ips", ips)
	m.TriggerEnforce()
}

// RemoveExpectedIPs removes IPs from the expected list for an interface
func (m *IPMonitor) RemoveExpectedIPs(iface string, ips []string) {
	if m == nil {
		return
	}
	m.Lock()
	defer m.Unlock()

	// Create a map for quick lookup
	toRemove := make(map[string]bool)
	for _, ip := range ips {
		toRemove[ip] = true
	}

	// Filter out the IPs to remove
	current := m.expectedIPs[iface]
	var updated []string
	for _, ip := range current {
		if !toRemove[ip] {
			updated = append(updated, ip)
		}
	}

	m.expectedIPs[iface] = updated
	m.logger.Info("Removed IPs from interface", "iface", iface, "remaining", updated)
	m.TriggerEnforce()
}

// ClearExpectedIPs removes all expected IPs for an interface
func (m *IPMonitor) ClearExpectedIPs(iface string) {
	if m == nil {
		return
	}
	m.Lock()
	defer m.Unlock()

	delete(m.expectedIPs, iface)
	m.logger.Info("Cleared all expected IPs", "iface", iface)
	m.TriggerEnforce()
}

// GetExpectedIPs returns the expected IPs for an interface (read-only copy)
func (m *IPMonitor) GetExpectedIPs(iface string) []string {
	if m == nil {
		return []string{}
	}
	m.RLock()
	defer m.RUnlock()

	if ips, exists := m.expectedIPs[iface]; exists {
		// Return a copy to prevent external modification
		result := make([]string, len(ips))
		copy(result, ips)
		return result
	}
	return []string{}
}

// deriveExpectedIPs returns iface -> floating IPs the given Active member should
// hold, according to config and cluster mode.
//
// In active-passive the sole Active owns every IP of every group mapped to the
// interface. In active-active the groups are shared, so the node owns only the
// subset assigned to it; expecting the whole group there made each Active node's
// enforce tick re-add all of it, undoing the coordinator's rebalance moves as
// fast as they were made (docs/TEST-PLAN.md defects #2/#26).
//
// The member's own ActiveIPs is the authority for that subset. It is safe to
// trust: BroadcastClusterState carries statuses and leases but never assignments,
// so no peer can overwrite what this node knows it was given.
func (m *IPMonitor) deriveExpectedIPs(nodeID string, member *Member) map[string][]string {
	nodeCfg, ok := m.members.config.Nodes[nodeID]
	if !ok || nodeCfg == nil {
		return nil
	}

	// nil means "no restriction"; an empty non-nil map means "assigned nothing".
	// Collapsing the two is what let an unassigned node claim the whole group.
	var assigned map[string]bool
	if m.members.config.Pulse.Mode == "active-active" {
		assigned = make(map[string]bool)
		for _, ip := range member.GetActiveIPs() {
			assigned[ip] = true
		}
	}

	expected := make(map[string][]string, len(nodeCfg.IPGroups))
	for iface, groups := range nodeCfg.IPGroups {
		var ifaceIPs []string
		for _, g := range groups {
			ips, ok := m.members.config.Groups[g]
			if !ok {
				m.logger.Warn("IP monitor: group not found in config", "group", g, "iface", iface)
				continue
			}
			for _, ip := range ips {
				if assigned == nil || assigned[ip] {
					ifaceIPs = append(ifaceIPs, ip)
				}
			}
		}
		if len(ifaceIPs) > 0 {
			expected[iface] = ifaceIPs
		}
	}
	return expected
}

// surplusFloatingIPs returns, per interface, the configured floating IPs this
// node is currently holding there but is not expected to hold. locate reports
// the interface an address is up on, and whether it is up at all.
//
// Every configured group is scanned, not just the groups still assigned to the
// node. Scoping the scan to assigned groups was the quieter half of
// docs/TEST-PLAN.md defect #40: unassigning a group removed it from the loop
// entirely, so the 61 addresses the node was still serving fell outside every
// set the pass could compute. The node logged "Current expectations
// expectations=map[]" and released nothing, on every tick, while `unassign`
// reported success — the operator-visible lie of that defect.
//
// Addresses outside every configured group are never touched: those are the
// node's own, not cluster floating IPs.
func surplusFloatingIPs(groups map[string][]string, expectations map[string][]string,
	locate func(ip string) (iface string, held bool)) map[string][]string {

	expected := make(map[string]map[string]bool, len(expectations))
	for iface, ips := range expectations {
		set := make(map[string]bool, len(ips))
		for _, ip := range ips {
			set[ip] = true
		}
		expected[iface] = set
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	surplus := make(map[string][]string)
	for _, name := range names {
		for _, ip := range groups[name] {
			iface, held := locate(ip)
			if !held || iface == "" || expected[iface][ip] {
				continue
			}
			surplus[iface] = append(surplus[iface], ip)
		}
	}
	for iface := range surplus {
		sort.Strings(surplus[iface])
	}
	return surplus
}

// releaseAttempt is the outcome of trying to release one surplus address.
type releaseAttempt struct {
	Iface string
	IP    string
	// Vanished means the address was already gone, so there was nothing to
	// release. The pass got the state it wanted; another hand produced it.
	Vanished bool
	// Err is set only when the release of an address the node still holds
	// genuinely failed — the case worth an error in the log.
	Err error
}

// releaseSurplusFloatingIPs brings down each surplus address, checking that the
// node still holds it immediately before the attempt and, if the attempt fails,
// immediately after.
//
// docs/TEST-PLAN.md defect #41: the release pass decided from an IP inventory
// snapshot taken at the top of the enforce tick, before the Active branch's
// bring-up loop, so by the time it ran the snapshot could be seconds old and
// addresses had moved. Releasing one that had already gone fails with
// `cannot assign requested address` — a no-op, logged at error level, which is
// exactly the noise that would hide a release that mattered.
//
// stillHeld must be a live check, not a read of whatever snapshot chose the
// surplus set; the point is to be newer than that snapshot. Consulting it again
// after a failure is what distinguishes "lost the race" from "could not do it":
// the residual window between the check and the kernel call cannot be closed,
// only classified.
func releaseSurplusFloatingIPs(surplus map[string][]string,
	stillHeld func(iface, ip string) bool,
	bringDown func(iface, ip string) error) []releaseAttempt {

	ifaces := make([]string, 0, len(surplus))
	for iface := range surplus {
		ifaces = append(ifaces, iface)
	}
	sort.Strings(ifaces)

	var attempts []releaseAttempt
	for _, iface := range ifaces {
		for _, ip := range surplus[iface] {
			if !stillHeld(iface, ip) {
				attempts = append(attempts, releaseAttempt{Iface: iface, IP: ip, Vanished: true})
				continue
			}
			err := bringDown(iface, ip)
			if err != nil && !stillHeld(iface, ip) {
				attempts = append(attempts, releaseAttempt{Iface: iface, IP: ip, Vanished: true})
				continue
			}
			attempts = append(attempts, releaseAttempt{Iface: iface, IP: ip, Err: err})
		}
	}
	return attempts
}

// initializeExpectedIPs initializes the expected IPs from the current member
func (m *IPMonitor) initializeExpectedIPs() error {
	m.logger.Debug("IP monitor: starting initializeExpectedIPs")

	// Get the local node ID
	localNodeID, err := m.members.config.GetLocalNodeUUID()
	if err != nil {
		m.logger.Error("IP monitor init: failed to get local node ID", "error", err)
		return fmt.Errorf("failed to get local node ID: %v", err)
	}
	m.logger.Debug("IP monitor init: got local node ID", "nodeID", localNodeID)

	// Resolve local member and node config
	localMember := m.members.GetMemberByID(localNodeID)
	if localMember == nil {
		m.logger.Error("IP monitor init: local member not found", "nodeID", localNodeID)
		return fmt.Errorf("local member not found")
	}
	m.logger.Debug("IP monitor init: found local member", "status", localMember.Status)

	nodeCfg, ok := m.members.config.Nodes[localNodeID]
	if !ok || nodeCfg == nil {
		m.logger.Error("IP monitor init: local node configuration not found", "nodeID", localNodeID)
		return fmt.Errorf("local node configuration not found")
	}
	m.logger.Debug("IP monitor init: found node config", "ipGroups", nodeCfg.IPGroups)

	// Reset expected IPs first
	m.expectedIPs = make(map[string][]string)
	m.logger.Debug("IP monitor init: reset expected IPs map")

	if localMember.Status == StatusActive {
		m.logger.Info("IP monitor init: node is Active, setting up expected IPs")
		for iface, ips := range m.deriveExpectedIPs(localNodeID, localMember) {
			m.expectedIPs[iface] = ips
			m.logger.Info("IP monitor init: set expected IPs for interface", "iface", iface, "ips", ips)
		}
		m.logger.Info("IP monitor initialization complete for Active node", "expected_ifaces", len(m.expectedIPs), "expectedIPs", m.expectedIPs)
	} else {
		// If we're not active, ensure no expected IPs and clean up any floating IPs
		m.logger.Info("IP monitor init: node is not Active, cleaning up floating IPs", "status", localMember.Status)
		m.cleanupFloatingIPsOnRestart(nodeCfg)
		m.logger.Info("IP monitor initialization complete for non-Active node", "status", localMember.Status, "expected_ifaces", 0)
	}

	m.TriggerEnforce()
	m.logger.Debug("IP monitor: initializeExpectedIPs complete")
	return nil
}

// cleanupFloatingIPsOnRestart removes any floating IPs that might be left over from before restart
func (m *IPMonitor) cleanupFloatingIPsOnRestart(nodeCfg *config.Node) {
	m.logger.Debug("IP monitor cleanup: starting cleanup for non-Active node")

	// Build list of all floating IPs that this node could potentially manage
	var allFloatingIPs []string
	for ifaceName, groups := range nodeCfg.IPGroups {
		m.logger.Debug("IP monitor cleanup: checking interface", "iface", ifaceName, "groups", groups)
		for _, group := range groups {
			if ips, ok := m.members.config.Groups[group]; ok {
				allFloatingIPs = append(allFloatingIPs, ips...)
				m.logger.Debug("IP monitor cleanup: found IPs in group", "group", group, "ips", ips)
			} else {
				m.logger.Debug("IP monitor cleanup: group not found", "group", group)
			}
		}
	}

	if len(allFloatingIPs) == 0 {
		m.logger.Info("IP monitor cleanup: no floating IPs to check")
		return
	}

	m.logger.Info("IP monitor cleanup: checking for floating IPs to clean up", "count", len(allFloatingIPs), "ips", allFloatingIPs)

	// Check each floating IP and remove if found on any interface
	for _, ip := range allFloatingIPs {
		m.logger.Debug("IP monitor cleanup: checking IP", "ip", ip)

		// Extract IP without CIDR if needed
		ipOnly := ip
		if cidr, err := utils.GetCIDR(ip); err == nil && cidr != nil {
			ipOnly = cidr.String()
		}

		exists, iface, err := network.CheckIfIPExists(ipOnly)
		if err != nil {
			m.logger.Debug("IP monitor cleanup: error checking IP existence", "ip", ip, "error", err)
			continue
		}

		if exists {
			m.logger.Info("IP monitor cleanup: found floating IP on interface, removing", "ip", ip, "iface", iface)
			if err := network.BringIPdown(iface, ip); err != nil {
				m.logger.Warn("IP monitor cleanup: failed to remove floating IP", "ip", ip, "iface", iface, "error", err)
			} else {
				m.logger.Info("IP monitor cleanup: successfully removed floating IP", "ip", ip, "iface", iface)
			}
		} else {
			m.logger.Debug("IP monitor cleanup: floating IP not found on any interface", "ip", ip)
		}
	}

	m.logger.Debug("IP monitor cleanup: cleanup complete")
}

// monitor loop is provided by platform-specific file (e.g., ip_monitor_linux.go)

// getInterfaceIPs gets all IPs assigned to an interface
func (m *IPMonitor) getInterfaceIPs(iface string) ([]string, error) {
	// Get the interface
	intf, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("interface not found: %v", err)
	}

	// Get addresses
	addrs, err := intf.Addrs()
	if err != nil {
		return nil, fmt.Errorf("failed to get addresses: %v", err)
	}

	// Extract IPs
	var ips []string
	for _, addr := range addrs {
		// Parse the address
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ips = append(ips, ipNet.IP.String())
	}

	return ips, nil
}
