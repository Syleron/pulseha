package membership

import (
	"fmt"
	"sort"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/ipam"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/pulselock"
)

// MemberList defines our member list object
type MemberList struct {
	pulselock.RWMutex
	Members   map[string]*Member
	config    *config.Config
	logger    *log.Logger
	ipMonitor *IPMonitor
}

// NewMemberList creates a new member list
func NewMemberList(cfg *config.Config, logger *log.Logger) *MemberList {
	ml := &MemberList{
		Members: make(map[string]*Member),
		config:  cfg,
		logger:  logger,
	}
	return ml
}

// SetIPMonitor sets the IP monitor reference
func (m *MemberList) SetIPMonitor(monitor *IPMonitor) {
	m.Lock()
	defer m.Unlock()
	m.ipMonitor = monitor
}

// Config returns the config the member list currently holds.
//
// UpdateConfig swaps the pointer under the write lock, and every read of it has to
// come through here. Defect #32 made each snapshot's *contents* stable, which
// stopped the observable corruption, but left the pointer read itself racing — the
// health check and enforce passes dereferenced m.config bare on every tick.
// TestConfigPointerIsNotReadWhileUpdateConfigSwapsIt is the driver that exposes it;
// nothing in the suite had previously run UpdateConfig against a read pass, which is
// why -race stayed quiet.
//
// Call this once per pass and work from the returned pointer, rather than calling it
// per field: the value is a snapshot either way, and taking the lock repeatedly
// inside a loop is the cost this is meant to avoid. May return nil — several callers
// already branch on that and must keep seeing it.
func (m *MemberList) Config() *config.Config {
	m.RLock()
	defer m.RUnlock()

	return m.config
}

// UpdateConfig updates the config reference for the member list and all existing members
func (m *MemberList) UpdateConfig(cfg *config.Config) {
	m.Lock()
	defer m.Unlock()

	oldConfig := m.config
	m.config = cfg

	// Get local node ID from both old and new configs for comparison
	var oldLocalID, newLocalID string
	if oldConfig != nil {
		oldLocalID, _ = oldConfig.GetLocalNodeUUID()
	}
	if cfg != nil {
		newLocalID, _ = cfg.GetLocalNodeUUID()
	}

	m.logger.Info("CONFIG_UPDATE: Updating member list config reference",
		"member_count", len(m.Members),
		"old_local_id", oldLocalID,
		"new_local_id", newLocalID)

	// Update config reference for all existing members to prevent stale references
	updatedCount := 0
	for id, member := range m.Members {
		if member != nil {
			// Check if this member's IsLocal() result will change
			wasLocal := false
			if oldConfig != nil {
				oldID, err := oldConfig.GetLocalNodeUUID()
				wasLocal = err == nil && member.ID == oldID
			}

			member.config = cfg

			// Refresh capacity from config so synced changes take effect
			// without recreating the member.
			if cfg != nil {
				if node, ok := cfg.Nodes[id]; ok {
					member.mu.Lock()
					member.Capacity = node.Capacity
					member.mu.Unlock()
				}
			}

			willBeLocal := false
			if cfg != nil {
				newID, err := cfg.GetLocalNodeUUID()
				willBeLocal = err == nil && member.ID == newID
			}

			if wasLocal != willBeLocal {
				m.logger.Warn("CONFIG_UPDATE: Member local status changed",
					"member_id", id,
					"hostname", member.Hostname,
					"was_local", wasLocal,
					"will_be_local", willBeLocal)
			}

			updatedCount++
			m.logger.Debug("CONFIG_UPDATE: Updated config reference for member",
				"member_id", id,
				"hostname", member.Hostname,
				"is_local", willBeLocal)
		}
	}

	m.logger.Info("CONFIG_UPDATE: Completed config update",
		"updated_members", updatedCount,
		"total_members", len(m.Members))
}

// RedistributeIPs handles redistribution of failed IPs to healthy nodes
func (m *MemberList) RedistributeIPs(failedIPs []string) error {
	m.Lock()
	defer m.Unlock()

	return m.redistributeIPsLocked(failedIPs)
}

// redistributeIPsLocked is RedistributeIPs' body, split out because the member
// list lock is not reentrant and RemoveMember redistributes while already
// holding it — calling the exported method there deadlocked the daemon on every
// removal of a node that still held addresses.
//
// Requires m.Lock() held.
func (m *MemberList) redistributeIPsLocked(failedIPs []string) error {
	// Get available nodes for redistribution
	availableNodes := m.getAvailableNodes()
	if len(availableNodes) == 0 {
		return fmt.Errorf("no available nodes for IP redistribution")
	}

	// Handle distribution based on mode
	switch m.config.Pulse.Mode {
	case "active-passive":
		// In active-passive mode, find the active node or promote one
		activeNode := m.getActiveNode()
		if activeNode == nil {
			// No active node, promote the first available node
			activeNode = availableNodes[0]
			if err := activeNode.MakeActive(failedIPs); err != nil {
				return fmt.Errorf("failed to promote node to active: %v", err)
			}
		} else {
			// Assign all IPs to the active node
			if err := activeNode.BringUpIPs(failedIPs); err != nil {
				return fmt.Errorf("failed to assign IPs to active node: %v", err)
			}
		}

	case "active-active":
		// Calculate IP distribution based on node capacity
		distribution := m.calculateIPDistribution(failedIPs, availableNodes)

		// Apply the new IP assignments
		for _, node := range availableNodes {
			ips := distribution[node.Hostname]
			if len(ips) == 0 {
				continue
			}

			// Merge with the node's existing assignments; MakeActive would
			// replace them and lose track of IPs the node already hosts.
			if err := node.AddActiveIPs(ips); err != nil {
				m.logger.Error("Failed to assign IPs to node", "hostname", node.Hostname, "error", err)
				// Continue with other nodes even if one fails
				continue
			}
		}

	default:
		return fmt.Errorf("invalid cluster mode: %s", m.config.Pulse.Mode)
	}

	return nil
}

// getAvailableNodes returns a list of nodes that can accept new IPs,
// sorted by node ID so distribution decisions are deterministic.
// Capacity and group eligibility are deliberately not checked here:
// active-passive assigns all IPs to a single node regardless of capacity, and
// active-active placement enforces both in ipam.Distribute, which knows which
// group each address belongs to.
// Status is read under each member's own lock: the member list lock this runs
// under does not cover it, because promotions and the health check loop write
// Status while holding only the member lock.
func (m *MemberList) getAvailableNodes() []*Member {
	var available []*Member
	for _, member := range m.Members {
		member.mu.Lock()
		status := member.Status
		member.mu.Unlock()

		// Skip nodes that are down or in maintenance
		if status == StatusUnknown || status == StatusMaintenance {
			continue
		}
		available = append(available, member)
	}
	sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	return available
}

// calculateIPDistribution determines how to distribute IPs across available nodes
func (m *MemberList) calculateIPDistribution(ips []string, nodes []*Member) map[string][]string {
	distribution := make(map[string][]string)
	if len(nodes) == 0 {
		return distribution
	}

	switch m.config.Pulse.Mode {
	case "active-passive":
		// All IPs go to the active node
		activeNode := m.getActiveNode()
		if activeNode != nil {
			distribution[activeNode.Hostname] = ips
		}

	case "active-active":
		// Balance by IP count: each IP goes to the node currently holding the
		// fewest IPs (existing assignments plus IPs handed out in this pass),
		// ties broken by node order (sorted by ID for determinism).
		//
		// Only nodes the address's group is assigned to are candidates. A node
		// without the group cannot bring the address up — AddActiveIPs refuses
		// it — and the refusal used to end the address's journey: unassigning a
		// group left the coordinator picking the node it had just been removed
		// from, over and over, with 61 addresses down cluster-wide behind a
		// passing quorum vote (docs/TEST-PLAN.md defect #40).
		index := groupIndex(m.config.Groups)
		placements := make([]ipam.IP, 0, len(ips))
		for _, ip := range ips {
			placements = append(placements, ipam.IP{Addr: ip, Group: index[ip]})
		}

		// ActiveIPs and Capacity are both written under the member lock —
		// AddActiveIPs appends, UpdateConfig refreshes capacity — so the counts
		// feeding placement have to be snapshotted under it, not read raw.
		snapshots := make([]ipam.Node, 0, len(nodes))
		for _, node := range nodes {
			node.mu.Lock()
			ipCount := len(node.ActiveIPs)
			capacity := node.Capacity
			node.mu.Unlock()

			snapshots = append(snapshots, ipam.Node{
				Hostname: node.Hostname,
				IPCount:  ipCount,
				Capacity: capacity,
				Groups:   nodeHostableGroups(m.config, node.ID),
			})
		}

		// Aggregate per group rather than per address. Unassigning a group from
		// every node leaves its whole set unplaceable, and one line per address
		// was 245 warnings per reclaim cycle on the test cluster.
		assignments, unplaced := ipam.Distribute(placements, snapshots)
		if len(unplaced) > 0 {
			byGroup := make(map[string]int, len(unplaced))
			for _, ip := range unplaced {
				byGroup[index[ip]]++
			}
			groups := make([]string, 0, len(byGroup))
			for group := range byGroup {
				groups = append(groups, group)
			}
			sort.Strings(groups)
			for _, group := range groups {
				m.logger.Warn("No eligible node with available capacity for floating IPs",
					"group", group, "count", byGroup[group], "nodes_considered", len(nodes))
			}
		}
		distribution = assignments
	}

	return distribution
}

// groupIndex maps every configured floating IP to the group it belongs to.
// Group names are visited in sorted order, so an address configured into more
// than one group resolves to the same group on every node.
func groupIndex(groups map[string][]string) map[string]string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	index := make(map[string]string)
	for _, name := range names {
		for _, ip := range groups[name] {
			if _, seen := index[ip]; !seen {
				index[ip] = name
			}
		}
	}
	return index
}

// nodeHostableGroups returns the groups a node may host, as ipam expects them:
// the union of the groups mapped to its interfaces.
//
// A nil result means unrestricted, and is returned only when the config has no
// entry for the node at all. Absent configuration is not evidence that a node
// cannot host a group — a cluster whose config has not finished syncing would
// otherwise have every placement refused — whereas an *empty* mapping is
// evidence, because unassigning a node's last group deletes the interface entry
// outright.
func nodeHostableGroups(cfg *config.Config, nodeID string) map[string]bool {
	if cfg == nil {
		return nil
	}
	nodeCfg, ok := cfg.Nodes[nodeID]
	if !ok || nodeCfg == nil {
		return nil
	}

	groups := make(map[string]bool)
	for _, assigned := range nodeCfg.IPGroups {
		for _, group := range assigned {
			groups[group] = true
		}
	}
	return groups
}

// getActiveNode returns the current active node in the cluster.
//
// Callers hold the member list lock, which covers the map but not Status — that
// is written under the member's own lock, so it is snapshotted under it here.
func (m *MemberList) getActiveNode() *Member {
	for _, member := range m.Members {
		member.mu.Lock()
		status := member.Status
		member.mu.Unlock()

		if status == StatusActive {
			return member
		}
	}
	return nil
}

// ConsolidationTarget picks the single node that should hold every floating IP
// once the cluster runs in active-passive mode. Preference order:
//
//  1. the most-loaded Active node — it already holds the most IPs, so the
//     fewest have to move;
//  2. the current leader, if it is healthy;
//  3. the lowest-ID healthy node.
//
// Ties break on the lowest node ID so the choice is deterministic rather than
// dependent on map iteration order. Failed and maintenance nodes are never
// selected. Returns nil when no healthy node exists.
func ConsolidationTarget(members map[string]*Member, leaderID string) *Member {
	var mostLoadedActive, lowestHealthy, healthyLeader *Member
	mostLoadedIPs := -1

	for id, member := range members {
		member.mu.Lock()
		status := member.Status
		ipCount := len(member.ActiveIPs)
		member.mu.Unlock()

		if status == StatusUnknown || status == StatusMaintenance {
			continue
		}

		if lowestHealthy == nil || id < lowestHealthy.ID {
			lowestHealthy = member
		}
		if id == leaderID {
			healthyLeader = member
		}

		if status != StatusActive {
			continue
		}
		if ipCount > mostLoadedIPs || (ipCount == mostLoadedIPs && id < mostLoadedActive.ID) {
			mostLoadedActive = member
			mostLoadedIPs = ipCount
		}
	}

	if mostLoadedActive != nil {
		return mostLoadedActive
	}
	if healthyLeader != nil {
		return healthyLeader
	}
	return lowestHealthy
}

// AddMember adds a new member to the member list
func (m *MemberList) AddMember(nodeID, hostname, bindIP, bindPort string) error {
	m.Lock()
	defer m.Unlock()

	m.logger.Debug("Starting AddMember process", "id", nodeID)

	// Check if member already exists
	if _, exists := m.Members[nodeID]; exists {
		m.logger.Warn("Member already exists in member list", "id", nodeID)
		return nil
	}

	// Create new member instance — start in maintenance so the operator can
	// verify the node before it becomes eligible for failover promotion.
	m.logger.Debug("Creating new member instance", "hostname", hostname, "id", nodeID)
	member := &Member{
		ID:       nodeID,
		Hostname: hostname,
		IP:       bindIP,
		Port:     bindPort,
		Status:   StatusMaintenance,
		config:   m.config,
		logger:   m.logger,
	}

	// Seed capacity from config so distribution decisions respect it immediately
	if m.config != nil {
		if node, ok := m.config.Nodes[nodeID]; ok {
			member.Capacity = node.Capacity
		}
	}

	// Set back-reference so member methods can access the list (e.g., during MakeActive)
	member.memberList = m

	m.logger.Debug("Member instance created successfully", "hostname", hostname, "id", nodeID)

	// Add member to list
	m.Members[nodeID] = member

	m.logger.Info("Successfully added member to member list", "hostname", hostname, "id", nodeID)
	return nil
}

// AddMemberQuiet adds a member with just the ID for backward compatibility
func (m *MemberList) AddMemberQuiet(id string) error {
	// Get node config
	node, ok := m.config.Nodes[id]
	if !ok {
		m.logger.Error("No configuration found for member ID", "id", id)
		return fmt.Errorf("no configuration found for member ID %s", id)
	}

	return m.AddMember(id, node.Hostname, node.IP, node.Port)
}

// GetMemberByID returns a member by ID
func (m *MemberList) GetMemberByID(id string) *Member {
	m.RLock()
	defer m.RUnlock()
	return m.Members[id]
}

// GetMemberByHostname returns a member by hostname
func (m *MemberList) GetMemberByHostname(hostname string) *Member {
	m.RLock()
	defer m.RUnlock()
	for _, member := range m.Members {
		if member.Hostname == hostname {
			return member
		}
	}
	return nil
}

// GetMemberCount returns the total number of members in the list
func (m *MemberList) GetMemberCount() int {
	m.RLock()
	defer m.RUnlock()
	return len(m.Members)
}

// Clear removes all members from the list.
func (m *MemberList) Clear() {
	m.Lock()
	defer m.Unlock()
	m.Members = make(map[string]*Member)
}

// RemoveMember removes a member from the list
// The id parameter can be either a node ID or a hostname
func (m *MemberList) RemoveMember(id string) error {
	m.Lock()
	defer m.Unlock()

	// First try to find by node ID
	if member, exists := m.Members[id]; exists {
		// Found by node ID
		m.logger.Debug("Removing member", "id", id)
		// Redistribute IPs if member was active. GetActiveIPs reads under the
		// member lock, and the locked variant is required here because this
		// already holds the member list lock.
		if held := member.GetActiveIPs(); len(held) > 0 {
			if err := m.redistributeIPsLocked(held); err != nil {
				m.logger.Error("Failed to redistribute IPs for removed member", "hostname", member.Hostname, "error", err)
			}
		}

		// TODO: Add selective connection cleanup - connections needed for voting
		// member.Close()

		// Remove member
		delete(m.Members, id)
		return nil
	}

	// If not found by ID, try to find by hostname
	for _, member := range m.Members {
		if member.Hostname == id {
			// Found by hostname
			m.logger.Debug("Removing member by hostname", "hostname", id, "id", member.ID)
			// Redistribute IPs if member was active (see the by-ID branch above).
			if held := member.GetActiveIPs(); len(held) > 0 {
				if err := m.redistributeIPsLocked(held); err != nil {
					m.logger.Error("Failed to redistribute IPs for removed member", "hostname", member.Hostname, "error", err)
				}
			}

			// TODO: Add selective connection cleanup - connections needed for voting
			// member.Close()

			// Remove member
			delete(m.Members, member.ID)
			return nil
		}
	}

	return fmt.Errorf("member with ID or hostname %s not found", id)
}

// GetMemberByIdentifier returns a member by either node ID or hostname
func (m *MemberList) GetMemberByIdentifier(identifier string) *Member {
	m.RLock()
	defer m.RUnlock()
	// First try by ID
	if member, exists := m.Members[identifier]; exists {
		return member
	}

	// Then try by hostname
	for _, member := range m.Members {
		if member.Hostname == identifier {
			return member
		}
	}

	return nil
}
