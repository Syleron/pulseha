package membership

import (
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
)

// releaseGraceWindow is how long an address this node was told to release stays
// protected from the node's own restore paths.
//
// It has to outlast the config write that says the address stopped being this
// node's share arriving here. On whitecrane a peer restored its share anywhere
// between sub-second and 22 seconds after the release (docs/TEST-PLAN.md defect
// #60), and the enforce loop's own period is 30s, so a minute clears both. It is
// only a backstop: an expectation set recomputed from a config that no longer
// gives this node the address restores nothing, and an explicit re-assignment
// clears the protection immediately.
const releaseGraceWindow = 60 * time.Second

// IPMonitor monitors IP addresses on interfaces and ensures they match the expected configuration
type IPMonitor struct {
	sync.RWMutex
	members     *MemberList
	logger      *log.Logger
	expectedIPs map[string][]string // map[interface][]ips
	// releasedIPs records, per interface, the addresses this node has been told
	// to give up and the moment each record stops applying. Keyed on the address
	// without its mask, since a release and an expectation are not guaranteed to
	// spell the same address the same way.
	releasedIPs map[string]map[string]time.Time
	// now is time.Now, indirected so the grace window can be tested without
	// waiting a minute.
	now      func() time.Time
	stopChan chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewIPMonitor creates a new IP monitor
func NewIPMonitor(members *MemberList, logger *log.Logger) *IPMonitor {
	return &IPMonitor{
		members:     members,
		logger:      logger,
		expectedIPs: make(map[string][]string),
		releasedIPs: make(map[string]map[string]time.Time),
		now:         time.Now,
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
	// Deliberately does not clear the release protection: one of this setter's
	// callers derives the set from the config, which is what lags on a node that
	// has just released, so clearing here would re-arm the defect (see
	// markReleasedLocked). Its other caller brings the addresses up itself, and an
	// address that is up is one no restore path is looking at.
	m.logger.Info("Updated expected IPs", "iface", iface, "ips", ips)
	m.TriggerEnforce()
}

// UpdateExpectedIPsAll replaces the whole expectation map.
//
// Deliberately does not TriggerEnforce: the caller is the enforce loop itself,
// which is about to act on this set, and re-arming from inside it would spin.
//
// Deliberately does not clear the release protection either, unlike the two
// setters above. Its caller derives this set from the config, and on a node that
// has just been told to release an address the config is precisely what has not
// caught up yet — clearing here would re-arm the race the protection exists to
// stop (docs/TEST-PLAN.md defect #60).
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

	// An address given back to this node is no longer an address it was told to
	// release, and must be served now rather than when the grace window runs out.
	// This is the one setter whose callers are unambiguously that statement:
	// BringUpIP calls it per address immediately before putting the address on the
	// interface. The config-derived setters must not clear (see
	// UpdateExpectedIPs/UpdateExpectedIPsAll).
	m.clearReleasedLocked(iface, ips)

	m.logger.Info("Added IPs to interface", "iface", iface, "ips", ips)
	m.TriggerEnforce()
}

// RemoveExpectedIPs removes IPs from the expected list for an interface.
//
// Every caller is a deliberate release — a bring-down RPC, a group edit, a
// rebalance move — so the addresses are also marked as released, which stops
// this node putting them straight back. Dropping the expectation is not enough
// on its own: both restore paths re-derive their expectations from the config,
// and the node being told to release is by construction a node whose config has
// not yet been told the address stopped being its share (see markReleasedLocked).
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
	m.markReleasedLocked(iface, ips)
	m.logger.Info("Removed IPs from interface", "iface", iface, "remaining", updated)
	m.TriggerEnforce()
}

// markReleasedLocked records that this node has been told to give up these
// addresses on iface, so its own restore paths leave them alone until either the
// config agrees or the grace window runs out.
//
// Removing the expectation cannot carry this by itself. Both paths that put an
// address back — the netlink watcher's restore and the enforce pass's bring-up —
// work from an expectation set that is recomputed from the config: in
// active-active from the node's own assignment list, in active-passive from the
// whole group configured on the interface. The node commanded to release is the
// node whose config has not yet been told the address is no longer its share, so
// it removes the expectation, re-derives the same expectation moments later, sees
// the address missing and restores it — undoing the release it was just asked to
// perform, and reporting success for it (docs/TEST-PLAN.md defect #60).
//
// The caller must hold m.Lock().
func (m *IPMonitor) markReleasedLocked(iface string, ips []string) {
	if len(ips) == 0 {
		return
	}
	if m.releasedIPs == nil {
		m.releasedIPs = make(map[string]map[string]time.Time)
	}

	now := m.timeNow()
	released, ok := m.releasedIPs[iface]
	if !ok {
		released = make(map[string]time.Time, len(ips))
		m.releasedIPs[iface] = released
	}
	// Expired records are dropped here rather than on a timer of their own: this
	// is the only path that grows the map.
	for ip, expiry := range released {
		if !expiry.After(now) {
			delete(released, ip)
		}
	}

	expiry := now.Add(releaseGraceWindow)
	for _, ip := range ips {
		released[ipWithoutMask(ip)] = expiry
	}
}

// clearReleasedLocked drops the release protection for the given addresses on
// iface. The caller must hold m.Lock().
func (m *IPMonitor) clearReleasedLocked(iface string, ips []string) {
	released, ok := m.releasedIPs[iface]
	if !ok {
		return
	}
	for _, ip := range ips {
		delete(released, ipWithoutMask(ip))
	}
	if len(released) == 0 {
		delete(m.releasedIPs, iface)
	}
}

// restoreSuppressed reports whether ip must be left down on iface because this
// node was recently told to release it.
func (m *IPMonitor) restoreSuppressed(iface, ip string) bool {
	if m == nil {
		return false
	}
	m.Lock()
	defer m.Unlock()

	released, ok := m.releasedIPs[iface]
	if !ok {
		return false
	}
	key := ipWithoutMask(ip)
	expiry, ok := released[key]
	if !ok {
		return false
	}
	if !expiry.After(m.timeNow()) {
		delete(released, key)
		if len(released) == 0 {
			delete(m.releasedIPs, iface)
		}
		return false
	}
	return true
}

// restorableIPs splits addresses missing from iface into the ones this node
// should put back and the ones it was told to release. Both restore paths go
// through here, so the policy is one decision rather than two.
func (m *IPMonitor) restorableIPs(iface string, missing []string) (restore, released []string) {
	for _, ip := range missing {
		if m.restoreSuppressed(iface, ip) {
			released = append(released, ip)
			continue
		}
		restore = append(restore, ip)
	}
	return restore, released
}

// timeNow is m.now, defaulting to time.Now for an IPMonitor built without one.
func (m *IPMonitor) timeNow() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// ipWithoutMask returns an address with any prefix length stripped, so
// "10.0.0.1/24" and "10.0.0.1" are the same key.
func ipWithoutMask(ip string) string {
	if i := strings.Index(ip, "/"); i >= 0 {
		return ip[:i]
	}
	return ip
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

// releasedForBookkeeping returns the addresses the release pass has established
// this node no longer holds, so they can leave its assignment list.
//
// A release that succeeded and an address that had already vanished are both
// "not held": the pass got the state it wanted either way. A release that failed
// on an address the node still holds is not — dropping it would take the address
// out of every set the next pass computes, stranding it exactly as defect #40
// did, so it stays on the list and gets retried.
func releasedForBookkeeping(attempts []releaseAttempt) []string {
	var released []string
	for _, attempt := range attempts {
		if attempt.Err == nil {
			released = append(released, attempt.IP)
		}
	}
	return released
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

// placeAttempt is the outcome of trying to place one expected address.
type placeAttempt struct {
	Iface string
	IP    string
	// Err is set when the bring-up failed. It says nothing about whether the
	// address is on the interface: #45 is the race where it reports failure for
	// an address that is in fact up, which is why the announcement below decides
	// from kernel state rather than from this.
	Err error
}

// placeMissingFloatingIPs brings up each missing address and announces the whole
// set it attempted, in one batch, letting the announcement decide which of them
// the interface actually holds.
//
// docs/TEST-PLAN.md defect #33, residual half. The over-announcing half was
// announcing an address the node had lost; this is the reverse and the more
// expensive one. The enforce pass placed addresses and announced none of them,
// and run 30 caught it doing so for the addresses that mattered most: node-1's
// final 72 of a 288-address group were placed there, live under a holder that had
// never announced them. Nothing re-announces on its own, so neighbours keep the
// previous owner's MAC until their ARP entries age out: #11/#15's risk, a silent
// partial outage that survives convergence.
//
// The set handed to announce is what this pass attempted, not what it believed
// it achieved. Deciding from the success list is the same staleness the
// over-announcing half had, pointed the other way — a list built during the
// placement loop cannot know about an address that came up after it was written.
// It is safe to offer the whole attempted set because the batch re-reads each
// address against the kernel immediately before its own arping (541111c),
// announcing the ones the interface holds and returning the rest as skipped. The
// kernel decides, at announce time.
//
// Announcement failures come back separately from placement failures: the
// addresses are up and serving either way, and a switch relearns them on the
// next ARP exchange regardless.
func placeMissingFloatingIPs(iface string, missing []string,
	bringUp func(iface, ip string) error,
	announce func(iface string, ips []string) ([]string, error),
) (attempts []placeAttempt, skipped []string, announceErr error) {

	if len(missing) == 0 {
		return nil, nil, nil
	}

	attempts = make([]placeAttempt, 0, len(missing))
	for _, ip := range missing {
		attempts = append(attempts, placeAttempt{Iface: iface, IP: ip, Err: bringUp(iface, ip)})
	}

	skipped, announceErr = announce(iface, missing)
	return attempts, skipped, announceErr
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
