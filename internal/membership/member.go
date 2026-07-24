package membership

import (
	"fmt"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
)

// MemberStatus represents the current state of a member
type MemberStatus int

const (
	StatusUnknown    MemberStatus = iota
	StatusActive
	StatusPassive
	StatusMaintenance // Node is up but excluded from failover promotion
)

// Member defines our member object
type Member struct {
	sync.Mutex
	ID             string
	Hostname       string
	IP             string // Node's IP address
	Port           string // Node's port
	Status         MemberStatus
	LastHCResponse time.Time
	Latency        string
	Score          int
	Client         *client.Client
	HCBusy         bool
	config         *config.Config
	logger         *log.Logger
	memberList     *MemberList

	// Active-Active support
	ActiveIPs  []string // IPs currently hosted by this member
	Capacity   int      // Node capacity for weighted distribution
	LoadFactor float64  // Current load factor (0.0-1.0)
}

// NewMember creates a new member instance
func NewMember(id string, hostname string, cfg *config.Config, logger *log.Logger) *Member {
	if logger == nil {
		logger = log.New(nil)
	}

	member := &Member{
		ID:       id,
		Hostname: hostname,
		Status:   StatusUnknown,
		config:   cfg,
		logger:   logger,
	}

	logger.Debug(fmt.Sprintf("Member instance created successfully for %s (ID: %s)", hostname, id))
	return member
}

// initializeClient initializes the client connection if needed
func (m *Member) initializeClient() error {
	if m.Client != nil {
		return nil
	}

	m.logger.Debug(fmt.Sprintf("Initializing client connection for member %s", m.Hostname))

	// Get node config
	// Prefer lookup by member ID (config Nodes is keyed by ID)
	var node *config.Node
	if n, ok := m.config.Nodes[m.ID]; ok {
		node = n
	}
	if node == nil {
		return fmt.Errorf("no configuration found for member %s", m.Hostname)
	}

	// Create new client
	c, err := client.New()
	if err != nil {
		return fmt.Errorf("failed to create client: %v", err)
	}

	// Connect to the member
	if err := c.Connect(node.IP, node.Port, false); err != nil {
		return fmt.Errorf("failed to connect to member %s: %v", m.Hostname, err)
	}

	m.Client = c
	m.logger.Debug(fmt.Sprintf("Client connection initialized for member %s", m.Hostname))
	return nil
}

// Close properly closes the member's client connection to prevent memory leaks
// Only call this when the member is being permanently removed from the cluster
func (m *Member) Close() {
	m.Lock()
	defer m.Unlock()

	if m.Client != nil && m.Client.Connection != nil {
		m.logger.Debug(fmt.Sprintf("Closing client connection for member %s (permanent removal)", m.Hostname))
		m.Client.Connection.Close()
		m.Client = nil
	}
}

// MakeActive promotes a member to active state.
// In active-passive mode the node receives all floating IPs.
// In active-active mode the node receives its assigned subset of IPs.
func (m *Member) MakeActive(ips []string) error {
	m.Lock()
	defer m.Unlock()

	m.Status = StatusActive
	m.ActiveIPs = ips
	if m.Capacity > 0 {
		m.LoadFactor = float64(len(ips)) / float64(m.Capacity)
	} else {
		m.LoadFactor = 1.0
	}

	return m.BringUpIPs(ips)
}

// AddActiveIPs assigns additional IPs to this member without dropping the
// IPs it already holds. The member is promoted to Active and only the newly
// added IPs are brought up. Used for incremental redistribution in
// active-active mode, where MakeActive's replace semantics would lose track
// of a node's existing assignments.
func (m *Member) AddActiveIPs(ips []string) error {
	m.Lock()
	existing := make(map[string]bool, len(m.ActiveIPs))
	for _, ip := range m.ActiveIPs {
		existing[ip] = true
	}
	var added []string
	for _, ip := range ips {
		if !existing[ip] {
			existing[ip] = true
			m.ActiveIPs = append(m.ActiveIPs, ip)
			added = append(added, ip)
		}
	}
	m.Status = StatusActive
	if m.Capacity > 0 {
		m.LoadFactor = float64(len(m.ActiveIPs)) / float64(m.Capacity)
	} else {
		m.LoadFactor = 1.0
	}
	m.Unlock()

	if len(added) == 0 {
		return nil
	}
	return m.BringUpIPs(added)
}

// BringUpIPs brings up the specified IPs on this member
func (m *Member) BringUpIPs(ips []string) error {
	// Resolve interface per IP using group assignments
	ifaceToIPs, err := m.groupIPsByInterface(ips)
	if err != nil {
		return err
	}

	if m.IsLocal() {
		for iface, ipList := range ifaceToIPs {
			if err := m.bringUpIPsLocally(iface, ipList); err != nil {
				return err
			}
		}
		return nil
	}

	// Remote: send one RPC per interface
	if err := m.initializeClient(); err != nil {
		return fmt.Errorf("failed to initialize client for member %s: %v", m.Hostname, err)
	}
	for iface, ipList := range ifaceToIPs {
		m.logger.Debug("Sending request to bring up IPs", "count", len(ipList), "hostname", m.Hostname, "iface", iface)
		if _, err := m.Client.Send(client.ProtoFunction(client.SendBringUpIP), &rpc.UpIpRequest{Iface: iface, Ips: ipList}); err != nil {
			return err
		}
	}
	return nil
}

// bringUpIPsLocally brings up IPs on the local node
func (m *Member) bringUpIPsLocally(iface string, ips []string) error {
	m.logger.Info("Bringing up IPs on interface", "count", len(ips), "iface", iface)

	// Update the IP monitor if available
	if m.memberList != nil && m.memberList.ipMonitor != nil {
		m.memberList.ipMonitor.UpdateExpectedIPs(iface, ips)
	}

	for _, ip := range ips {
		m.logger.Debug("Bringing up IP on interface", "ip", ip, "iface", iface)

		// Check if IP is already up somewhere else
		exists, existingIface, err := network.CheckIfIPExists(ip)
		if err != nil {
			return fmt.Errorf("failed to check IP existence: %v", err)
		}

		// If IP exists on another interface, bring it down first
		if exists && existingIface != iface {
			m.logger.Warn("IP exists on interface, bringing it down first", "ip", ip, "existingIface", existingIface)
			if err := network.BringIPdown(existingIface, ip); err != nil {
				m.logger.Error("Failed to bring down IP from interface", "ip", ip, "iface", existingIface, "error", err)
				// Continue anyway as the IP might have already been removed
			}
		}

		// Bring up the IP on the specified interface
		if err := network.BringIPup(iface, ip); err != nil {
			return fmt.Errorf("failed to bring up IP %s on interface %s: %v", ip, iface, err)
		}

		// Send gratuitous ARP to update network
		if err := network.SendGARP(iface, ip); err != nil {
			m.logger.Warn("Failed to send GARP", "ip", ip, "iface", iface, "error", err)
			// Don't return error as the IP is still up
		}

		m.logger.Info("Successfully brought up IP on interface", "ip", ip, "iface", iface)
	}

	return nil
}

// BringDownIPs brings down the specified IPs on this member based on configuration
func (m *Member) BringDownIPs(ips []string) error {
	// For passive nodes, prevent continuous BringDownIP calls that cause loops
	m.Lock()
	isLocal := m.IsLocal()
	status := m.Status
	m.Unlock()

	ifaceToIPs, err := m.groupIPsByInterface(ips)
	if err != nil {
		return err
	}

	if isLocal {
		for iface, ipList := range ifaceToIPs {
			// Update the IP monitor if available
			if m.memberList != nil && m.memberList.ipMonitor != nil {
				m.memberList.ipMonitor.RemoveExpectedIPs(iface, ipList)
			}

			if status != StatusActive {
				m.logger.Debug("Passive local node defers bring-down to monitor", "iface", iface, "ips", ipList)
				continue
			}

			for _, ip := range ipList {
				m.logger.Info("Bringing down IP on interface", "ip", ip, "iface", iface)
				if err := network.BringIPdown(iface, ip); err != nil {
					return fmt.Errorf("failed to bring down IP %s on interface %s: %v", ip, iface, err)
				}
			}
		}
		return nil
	}

	// Remote: send one RPC per interface
	if err := m.initializeClient(); err != nil {
		return fmt.Errorf("failed to initialize client for member %s: %v", m.Hostname, err)
	}
	for iface, ipList := range ifaceToIPs {
		m.logger.Debug("Sending request to bring down IPs", "count", len(ipList), "hostname", m.Hostname, "iface", iface)
		if _, err := m.Client.Send(client.ProtoFunction(client.SendBringDownIP), &rpc.DownIpRequest{Iface: iface, Ips: ipList}); err != nil {
			return err
		}
	}
	return nil
}

// groupIPsByInterface maps each IP to the correct interface based on group assignments
func (m *Member) groupIPsByInterface(ips []string) (map[string][]string, error) {
	ifaceToIPs := make(map[string][]string)

	// Find node config by ID
	var nodeCfg *config.Node
	if n, ok := m.config.Nodes[m.ID]; ok {
		nodeCfg = n
	}
	if nodeCfg == nil {
		return nil, fmt.Errorf("node configuration not found for %s", m.ID)
	}

	// Build map group->iface for this node
	groupToIface := make(map[string]string)
	for iface, groups := range nodeCfg.IPGroups {
		for _, g := range groups {
			groupToIface[g] = iface
		}
	}

	// For each IP, find its group in config and interface on this node
	for _, ip := range ips {
		var groupName string
		matched := false
		for g, ipList := range m.config.Groups {
			for _, gip := range ipList {
				if gip == ip {
					groupName = g
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("no group found for IP %s", ip)
		}
		iface, ok := groupToIface[groupName]
		if !ok || iface == "" {
			return nil, fmt.Errorf("group %s not assigned to any interface on node %s", groupName, m.Hostname)
		}
		ifaceToIPs[iface] = append(ifaceToIPs[iface], ip)
	}
	return ifaceToIPs, nil
}

// IsLocal checks if this member is the local node
func (m *Member) IsLocal() bool {
	if m.config == nil {
		if m.logger != nil {
			m.logger.Error("MEMBER: IsLocal() called with nil config",
				"member_id", m.ID,
				"hostname", m.Hostname)
		}
		return false
	}

	localNodeID, err := m.config.GetLocalNodeUUID()
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("MEMBER: IsLocal() GetLocalNodeUUID failed",
				"member_id", m.ID,
				"hostname", m.Hostname,
				"error", err)
		}
		return false
	}

	result := m.ID == localNodeID
	if m.logger != nil {
		m.logger.Debug("MEMBER: IsLocal() check",
			"member_id", m.ID,
			"hostname", m.Hostname,
			"local_node_id", localNodeID,
			"is_local", result)
	}

	return result
}

// RemoveIPs removes the specified IPs from the member's active IPs
func (m *Member) RemoveIPs(ips []string) {
	m.Lock()
	defer m.Unlock()

	// Create a lookup map for IPs to remove
	toRemove := make(map[string]bool)
	for _, ip := range ips {
		toRemove[ip] = true
	}

	// Filter out the IPs to remove
	var newActiveIPs []string
	for _, ip := range m.ActiveIPs {
		if !toRemove[ip] {
			newActiveIPs = append(newActiveIPs, ip)
		}
	}

	// Update active IPs
	m.ActiveIPs = newActiveIPs

	// Only try to bring down IPs that are actually present on the interface
	if m.IsLocal() {
		// Check which IPs actually exist on local interfaces before trying to bring them down
		var ipsToRemove []string
		for _, ip := range ips {
			// Extract IP without CIDR if needed
			ipOnly := ip
			if cidr, _ := utils.GetCIDR(ip); cidr != nil {
				ipOnly = cidr.String()
			}
			exists, _, err := network.CheckIfIPExists(ipOnly)
			if err == nil && exists {
				ipsToRemove = append(ipsToRemove, ip)
			}
		}
		// Only call BringDownIPs if there are actually IPs to remove
		if len(ipsToRemove) > 0 {
			if err := m.BringDownIPs(ipsToRemove); err != nil {
				m.logger.Error("Failed to bring down IPs", "error", err)
			}
		} else {
			m.logger.Debug("No IPs found on interface to remove", "ips", ips)
		}
	} else {
		// Remote node - trust the health checker and try to bring them down anyway
		if err := m.BringDownIPs(ips); err != nil {
			m.logger.Error("Failed to bring down IPs", "error", err)
		}
	}
}

// GetHealthStatus returns detailed health information about the member
func (m *Member) GetHealthStatus() MemberHealth {
	m.Lock()
	defer m.Unlock()

	return MemberHealth{
		Hostname:     m.Hostname,
		Status:       m.Status,
		ActiveIPs:    append([]string{}, m.ActiveIPs...),
		LastResponse: m.LastHCResponse,
		Latency:      m.Latency,
	}
}

// EnterMaintenance transitions this member to maintenance mode.
// If the member is currently active, its IPs are brought down first so the
// cluster can elect a new active node before marking the transition.
func (m *Member) EnterMaintenance() error {
	m.Lock()
	defer m.Unlock()
	if m.Status == StatusMaintenance {
		return nil
	}
	if m.Status == StatusActive {
		// Bring down all hosted IPs before leaving active state
		if len(m.ActiveIPs) > 0 {
			_ = m.BringDownIPs(m.ActiveIPs)
		}
		m.ActiveIPs = nil
		m.LoadFactor = 0
	}
	m.Status = StatusMaintenance
	return nil
}

// ExitMaintenance returns this member to passive state so it can be
// considered for promotion again.
func (m *Member) ExitMaintenance() error {
	m.Lock()
	defer m.Unlock()
	if m.Status != StatusMaintenance {
		return nil
	}
	m.Status = StatusPassive
	return nil
}

// StatusToString converts a MemberStatus to its string representation
func StatusToString(status MemberStatus) string {
	switch status {
	case StatusActive:
		return "Active"
	case StatusPassive:
		return "Passive"
	case StatusMaintenance:
		return "Maintenance"
	case StatusUnknown:
		return "Unknown"
	default:
		return fmt.Sprintf("Unknown(%d)", status)
	}
}
