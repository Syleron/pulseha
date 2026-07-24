package membership

import (
	"fmt"
	"sort"
	"sync"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// MemberList defines our member list object
type MemberList struct {
	sync.RWMutex
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
func (m *MemberList) getAvailableNodes() []*Member {
	var available []*Member
	for _, member := range m.Members {
		// Skip nodes that are down, at capacity, or in maintenance
		if member.Status == StatusUnknown || member.Status == StatusMaintenance {
			continue
		}

		// Check if node has capacity for more IPs
		if m.hasAvailableCapacity(member) {
			available = append(available, member)
		}
	}
	sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	return available
}

// hasAvailableCapacity checks if a node can handle more IPs
func (m *MemberList) hasAvailableCapacity(member *Member) bool {
	// If no capacity is set, assume node can handle more IPs
	if member.Capacity == 0 {
		return true
	}

	// Check if node is under its capacity limit
	return len(member.ActiveIPs) < member.Capacity
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
		ipCounts := make(map[string]int)
		nodeCapacities := make(map[string]int)
		for _, node := range nodes {
			ipCounts[node.Hostname] = len(node.ActiveIPs)
			nodeCapacities[node.Hostname] = m.getNodeAvailableCapacity(node)
		}

		for _, ip := range ips {
			var targetNode *Member
			for _, node := range nodes {
				if nodeCapacities[node.Hostname] <= 0 {
					continue
				}
				if targetNode == nil || ipCounts[node.Hostname] < ipCounts[targetNode.Hostname] {
					targetNode = node
				}
			}

			if targetNode == nil {
				m.logger.Warn("No nodes with available capacity for IP", "ip", ip)
				continue
			}

			distribution[targetNode.Hostname] = append(distribution[targetNode.Hostname], ip)
			ipCounts[targetNode.Hostname]++
			nodeCapacities[targetNode.Hostname]--
		}
	}

	return distribution
}

// getNodeAvailableCapacity calculates remaining capacity for a node.
// Returns a sentinel larger than the total configured IP count for unlimited
// nodes (Capacity == 0) so capacity never constrains their distribution.
func (m *MemberList) getNodeAvailableCapacity(member *Member) int {
	if member.Capacity == 0 {
		total := 0
		for _, ips := range m.config.Groups {
			total += len(ips)
		}
		return total + 1
	}
	return member.Capacity - len(member.ActiveIPs)
}

// getActiveNode returns the current active node in the cluster
func (m *MemberList) getActiveNode() *Member {
	for _, member := range m.Members {
		if member.Status == StatusActive {
			return member
		}
	}
	return nil
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
		// Redistribute IPs if member was active
		if len(member.ActiveIPs) > 0 {
			if err := m.RedistributeIPs(member.ActiveIPs); err != nil {
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
			// Redistribute IPs if member was active
			if len(member.ActiveIPs) > 0 {
				if err := m.RedistributeIPs(member.ActiveIPs); err != nil {
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
