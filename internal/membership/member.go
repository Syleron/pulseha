package membership

import (
	"fmt"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/rpc"
)

// MemberStatus represents the current state of a member
type MemberStatus int

// These ordinals are a wire contract, not an implementation detail: `member_states`
// is encoded as `int(MemberStatus)` in both broadcast paths and decoded straight
// back into a MemberStatus with no range validation, so the numbers here must match
// rpc.MemberStatusEnum exactly (asserted by TestMemberStatusOrdinalsMatchTheProto).
//
// 3 is deliberately skipped rather than closed up. It belonged to the removed
// StatusPartialActive, and the proto retired it properly with `reserved 3` while
// keeping MAINTENANCE = 4. Letting iota slide Maintenance down into the hole breaks
// a rolling upgrade both ways — a new peer's Maintenance(3) is read as
// PartialActive by an old binary, and an old peer's Maintenance(4) becomes an
// undefined status that matches no arm of redistributeOrphanedIPs' switch, so the
// node's ActiveIPs are neither counted as hosted nor cleared and the coordinator
// redistributes addresses it may still be holding. Nothing indexes by MemberStatus,
// so the gap is free.
const (
	StatusUnknown MemberStatus = 0
	StatusActive  MemberStatus = 1
	StatusPassive MemberStatus = 2
	// 3 is reserved for the removed StatusPartialActive — see above.
	StatusMaintenance MemberStatus = 4 // Node is up but excluded from failover promotion
)

// Member defines our member object
type Member struct {
	pulselock.Mutex
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
	m.Status = StatusActive
	m.ActiveIPs = ips
	if m.Capacity > 0 {
		m.LoadFactor = float64(len(ips)) / float64(m.Capacity)
	} else {
		m.LoadFactor = 1.0
	}
	m.Unlock()

	// Deliberately outside the lock. Bringing up a large group touches the network
	// for every address, and every reader of this member's status — health check
	// responses included — needs the same lock. Holding it across the bring-up made
	// an Active node with a big group look dead to its peers (docs/TEST-PLAN.md
	// defects #4/#8). The state above is already committed, so a concurrent reader
	// sees this node as Active with its IPs assigned while they are coming up, which
	// is the honest answer: it owns them.
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

// RemoveActiveIPs drops the given IPs from this member's assignment list,
// leaving the rest untouched. Bookkeeping only: it brings nothing down, because
// its caller is the release pass, which has already done that.
//
// The counterpart to AddActiveIPs, and the reason it exists separately from the
// BringDownIP RPC handler's inline equivalent: the enforce loop releases
// addresses locally without going through that handler, so the list it maintains
// was never updated. Every address the pass released stayed on the list
// permanently — reported as held by a node serving nothing, and counted as that
// node's load by placement (docs/TEST-PLAN.md defect #58).
func (m *Member) RemoveActiveIPs(ips []string) {
	if len(ips) == 0 {
		return
	}

	removed := make(map[string]bool, len(ips))
	for _, ip := range ips {
		removed[ip] = true
	}

	m.Lock()
	defer m.Unlock()

	remaining := make([]string, 0, len(m.ActiveIPs))
	for _, ip := range m.ActiveIPs {
		if !removed[ip] {
			remaining = append(remaining, ip)
		}
	}
	m.ActiveIPs = remaining

	// The same formula AddActiveIPs uses, so the two stay consistent: with no
	// capacity configured the load factor is not meaningful.
	if m.Capacity > 0 {
		m.LoadFactor = float64(len(m.ActiveIPs)) / float64(m.Capacity)
	} else {
		m.LoadFactor = 1.0
	}
}

// GetActiveIPs returns a copy of the IPs this member currently hosts.
//
// Callers deciding what a node should hold need this under the member lock —
// the health check loop and the IP monitor both read it while promotions and
// rebalance moves are writing it.
func (m *Member) GetActiveIPs() []string {
	m.Lock()
	defer m.Unlock()
	if len(m.ActiveIPs) == 0 {
		return nil
	}
	ips := make([]string, len(m.ActiveIPs))
	copy(ips, m.ActiveIPs)
	return ips
}

// GetStatus returns this member's status, read under the member lock.
//
// Status is written under that lock by promotions, the health check loop and the
// maintenance transitions, so deciding anything from a bare read races with all
// three.
func (m *Member) GetStatus() MemberStatus {
	m.Lock()
	defer m.Unlock()
	return m.Status
}

// SetStatus records this member's status under the member lock.
func (m *Member) SetStatus(status MemberStatus) {
	m.Lock()
	defer m.Unlock()
	m.Status = status
}

// MarkUnreachable records that this member is no longer known to hold any
// floating IPs — status Unknown with an empty assignment set.
//
// Promotion uses this when an incumbent could not be reached, so the addresses
// it may still be holding can be accounted for elsewhere. The two fields have to
// move together under one lock: a reader seeing Unknown against the old ActiveIPs
// would conclude a down node still owns the group.
func (m *Member) MarkUnreachable() {
	m.Lock()
	defer m.Unlock()
	m.Status = StatusUnknown
	m.ActiveIPs = nil
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
		return fmt.Errorf("failed to initialize client for member %s: %w", m.Hostname, err)
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

	// Announcement is deferred until every address is up, then done in one batch.
	// Per-IP GARP inside this loop made the loop take four seconds an address, so a
	// large group kept this node unresponsive long enough for peers to elect a
	// replacement while it still held every IP (docs/TEST-PLAN.md defects #4/#8).
	//
	// Deferred, but not skippable: the loop gives up on the first address it cannot
	// bring up, and the ones before it are already on the interface. Returning
	// without announcing leaves them live and unreachable — peers still have the old
	// MAC — so the announcement runs on every exit. A failure to announce is logged
	// rather than returned, since the addresses are up either way.
	//
	// The set announced is every address this call got as far as attempting, not
	// the ones the loop believed it placed. A bring-up that reports failure for an
	// address the kernel does in fact hold is #45's race, and deciding from the
	// success list leaves such an address live and unannounced — #33's residual
	// half. Offering the attempted set is safe because the batch re-reads each
	// address against the kernel immediately before its own arping, announcing what
	// the interface holds and returning the rest as skipped: the kernel decides, at
	// announce time, which is later than any list this loop could keep.
	attempted := make([]string, 0, len(ips))
	announceAttempted := func() {
		if len(attempted) == 0 {
			return
		}
		skipped, err := network.SendGARPBatch(iface, attempted)
		if err != nil {
			m.logger.Warn("Failed to announce some IPs", "iface", iface, "error", err)
		}
		if len(skipped) > 0 {
			// On the daemon's logger, not the network package's: see the same
			// report in Server.BringUpIP (#33/#61).
			m.logger.Debug("Skipped announcing addresses this node no longer holds",
				"iface", iface, "count", len(skipped), "of", len(attempted))
		}
	}

	for _, ip := range ips {
		m.logger.Debug("Bringing up IP on interface", "ip", ip, "iface", iface)

		// Check if IP is already up somewhere else
		exists, existingIface, err := network.CheckIfIPExists(ip)
		if err != nil {
			announceAttempted()
			return fmt.Errorf("failed to check IP existence: %w", err)
		}

		// If IP exists on another interface, bring it down first
		if exists && existingIface != iface {
			m.logger.Warn("IP exists on interface, bringing it down first", "ip", ip, "existingIface", existingIface)
			if err := network.BringIPdown(existingIface, ip); err != nil {
				m.logger.Error("Failed to bring down IP from interface", "ip", ip, "iface", existingIface, "error", err)
				// Continue anyway as the IP might have already been removed
			}
		}

		// Recorded before the attempt, not after: the address is a candidate for
		// announcement from the moment this node tries to place it here.
		attempted = append(attempted, ip)

		// Bring up the IP on the specified interface
		if err := network.BringIPup(iface, ip); err != nil {
			announceAttempted()
			return fmt.Errorf("failed to bring up IP %s on interface %s: %w", ip, iface, err)
		}

		m.logger.Info("Successfully brought up IP on interface", "ip", ip, "iface", iface)
	}

	// Announce the whole set. A failure here leaves the addresses up and serving,
	// so it is logged rather than returned.
	announceAttempted()

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
		return fmt.Errorf("failed to initialize client for member %s: %w", m.Hostname, err)
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
// The bring-down happens with the lock RELEASED. BringDownIPs takes the member lock itself
// to read IsLocal and Status, and Member embeds a plain sync.RWMutex, which is not reentrant
// — so holding it across that call deadlocked `pulsectl node maintenance` against an Active
// node holding addresses, which is the case the command exists for. The goroutine then held
// the member lock forever, so anything else touching that member wedged behind it.
//
// The ordering the comment above promises is preserved: the addresses are down before the
// status is marked, so peers see a node that has stopped serving before they see one that is
// ineligible.
func (m *Member) EnterMaintenance() error {
	m.Lock()
	if m.Status == StatusMaintenance {
		m.Unlock()
		return nil
	}
	// Snapshotted rather than passed by reference, because the field is cleared below and
	// the bring-down runs between the two critical sections.
	var toRelease []string
	if m.Status == StatusActive && len(m.ActiveIPs) > 0 {
		toRelease = append([]string{}, m.ActiveIPs...)
	}
	m.Unlock()

	if len(toRelease) > 0 {
		// Bring down all hosted IPs before leaving active state
		_ = m.BringDownIPs(toRelease)
	}

	m.Lock()
	defer m.Unlock()
	// Re-checked: the status can have moved while the addresses were coming down, and a node
	// that is no longer Active must not have its assignment list cleared out from under
	// whatever moved it.
	if m.Status == StatusActive {
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
