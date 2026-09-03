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

// Claim is what a member asserts about itself: its status, together with the
// Floating IPs it says it holds.
//
// The two are one value because either alone misleads. A status with no
// addresses behind it tells peers a node is serving nothing; addresses recorded
// against a member that has stopped serving keep the cluster from re-placing
// them (docs/TEST-PLAN.md #2/#26/#58). Every defect in that family came from one
// of the pair being written without the other.
//
// A claim is an assertion of ownership, not a report on any interface's
// contents: a member can truthfully claim an address a moment before that
// address is up, because the lock covers the state transition and never the
// network I/O (docs/adr/0004-the-lock-covers-the-state-transition.md).
type Claim struct {
	Status    MemberStatus
	ActiveIPs []string
}

// WithAddresses returns the claim with ips added, ignoring any it already
// claims.
//
// A value method, so it takes no lock and can be used from inside an
// UpdateClaim decision -- which is where it is wanted, since appending to a
// claim's assignment set is the commonest thing such a decision does.
//
// Deduplicating matters beyond tidiness: the resulting set is what gets
// announced, and announcing an address the interface does not hold is what
// #33's stale announce set cost.
func (c Claim) WithAddresses(ips ...string) Claim {
	added := addressesNotIn(c.ActiveIPs, ips)
	if len(added) == 0 {
		return c
	}
	next := make([]string, 0, len(c.ActiveIPs)+len(added))
	next = append(next, c.ActiveIPs...)
	next = append(next, added...)
	c.ActiveIPs = next
	return c
}

// copyIPs returns the claim with its assignment set detached from whatever slice
// the caller passed or the member holds.
//
// Claims cross the membership/server boundary, and the pre-claim code aliased
// instead: MakeActive assigned the caller's slice straight into m.ActiveIPs, so
// a caller that reused its buffer mutated the member's record from outside the
// lock. Nothing depended on that, and nothing should be able to.
func (c Claim) copyIPs() Claim {
	if len(c.ActiveIPs) == 0 {
		c.ActiveIPs = nil
		return c
	}
	ips := make([]string, len(c.ActiveIPs))
	copy(ips, c.ActiveIPs)
	c.ActiveIPs = ips
	return c
}

// Member defines our member object
type Member struct {
	// mu is a named private field rather than an embedded mutex, so Lock() is
	// not part of Member's public surface and nothing outside this package can
	// take it. See docs/adr/0003-instrumented-mutexes.md for why that is worth
	// doing on this type and not on the others.
	mu             pulselock.Mutex
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
	ActiveIPs []string // IPs currently hosted by this member
	Capacity  int      // Node capacity for weighted distribution
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
	m.mu.Lock()
	defer m.mu.Unlock()

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
	// The claim first, and on its own. A caller that wants only the claim --
	// SetMode's consolidation, which deliberately defers the address work until
	// the server lock drops -- calls SetClaim directly and does not come through
	// here at all.
	m.SetClaim(Claim{Status: StatusActive, ActiveIPs: ips})

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
	// The claim change and the addresses it added, decided under one lock. Only
	// what was genuinely new gets brought up: re-announcing an address this
	// member already holds is what defect #33's stale announce set cost.
	var added []string
	m.UpdateClaim(func(current Claim) (Claim, bool) {
		added = addressesNotIn(current.ActiveIPs, ips)
		current.Status = StatusActive
		return current.WithAddresses(ips...), true
	})

	if len(added) == 0 {
		return nil
	}
	// Outside the lock, per docs/adr/0004. See MakeActive for what holding it
	// across a bring-up cost.
	return m.BringUpIPs(added)
}

// addressesNotIn returns the entries of want that held does not already
// contain, in the order they were offered and without duplicates of its own.
func addressesNotIn(held, want []string) []string {
	if len(want) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(held)+len(want))
	for _, ip := range held {
		seen[ip] = true
	}
	var missing []string
	for _, ip := range want {
		if !seen[ip] {
			seen[ip] = true
			missing = append(missing, ip)
		}
	}
	return missing
}

// RemoveActiveIPs drops the given IPs from this member's assignment list,
// leaving the rest untouched. Bookkeeping only: it brings nothing down, because
// its caller is the release pass, which has already done that.
//
// The counterpart to AddActiveIPs, and it exists because the enforce loop
// releases addresses locally without going through the BringDownIP RPC handler,
// so the list that handler maintains was never updated. Every address the pass
// released stayed on the list permanently — reported as held by a node serving
// nothing, and counted as that node's load by placement (docs/TEST-PLAN.md
// defect #58).
//
// The BringDownIP handler used to carry its own copy of this loop, and the only
// thing separating them was that this one also recomputed LoadFactor. That field
// was write-only and is now deleted, so the handler calls this instead
// (END-2339).
func (m *Member) RemoveActiveIPs(ips []string) {
	if len(ips) == 0 {
		return
	}

	removed := make(map[string]bool, len(ips))
	for _, ip := range ips {
		removed[ip] = true
	}

	m.UpdateClaim(func(current Claim) (Claim, bool) {
		remaining := make([]string, 0, len(current.ActiveIPs))
		for _, ip := range current.ActiveIPs {
			if !removed[ip] {
				remaining = append(remaining, ip)
			}
		}
		current.ActiveIPs = remaining
		return current, true
	})
}

// Claim returns what this member currently asserts, read as one consistent
// pair.
//
// Reading Status and ActiveIPs through separate accessors can straddle a write
// and pair a new status with the previous status's addresses, which is the
// mismatch the type exists to prevent.
func (m *Member) Claim() Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimLocked()
}

// SetClaim replaces what this member asserts, both fields together.
//
// Touches nothing else -- no network, no interfaces. A caller that also needs
// the addresses brought up or down calls BringUpIPs or BringDownIPs after this
// returns and outside the lock, per docs/adr/0004.
func (m *Member) SetClaim(c Claim) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setClaimLocked(c)
}

// UpdateClaim reads this member's claim, offers it to decide, and writes back
// what comes out -- all under one acquisition. Reports whether it wrote.
//
// This exists for the callers whose new claim depends on the current one, where
// splitting the read and the write into two acquisitions would open a window for
// something else to move the member in between. ConfigSync applying a peer's
// view is the case that needs it: the decision consults the incoming status, the
// current status, the config's maintenance flag and the epoch, and must not
// commit against a status that has since changed.
//
// The function receives and returns only a Claim, never the *Member, so a
// caller in another package cannot reach any other field or take any other
// lock. Returning false leaves the member untouched.
//
// It runs with the member lock held, so it must not call back into a locking
// Member method -- that is #85's shape exactly. What makes offering this
// acceptable at all is that such a call now announces itself: pulselock panics
// on it under test and reports it on stderr live, where before it wedged the
// member lock in silence (docs/adr/0003-instrumented-mutexes.md).
func (m *Member) UpdateClaim(decide func(current Claim) (Claim, bool)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	next, ok := decide(m.claimLocked())
	if !ok {
		return false
	}
	m.setClaimLocked(next)
	return true
}

// SetActiveIPs replaces the addresses this member claims, leaving its status
// alone.
//
// For the callers that are recording an assignment rather than a role change:
// the coordinator seeding an active-active map, and a peer self-reporting what
// it holds.
func (m *Member) SetActiveIPs(ips []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ActiveIPs = Claim{ActiveIPs: ips}.copyIPs().ActiveIPs
}

// SetEndpoint records where this member is reached, from the config that
// described it.
//
// Not part of the claim -- these say where a node is, not what it asserts about
// itself. Synchronised because loadInitialMembers wrote all three bare, while
// the health-check loop reads Hostname to log against and the client pool reads
// IP and Port to dial: the member is already in the list by the time these are
// set, so a pass could observe a half-described member.
func (m *Member) SetEndpoint(ip, port, hostname string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IP = ip
	m.Port = port
	m.Hostname = hostname
}

// Endpoint returns where this member is reached, as one consistent triple.
func (m *Member) Endpoint() (ip, port, hostname string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.IP, m.Port, m.Hostname
}

// SetLastResponse records when this member was last heard from.
//
// Not part of the claim: a claim is what the member asserts, and this is what we
// observed about it. But it is read under the member lock by three consumers
// that measure silence with it -- clusterCoordinator's grace window,
// redistributeOrphanedIPs' decision to reclaim stranded addresses, and
// selectBestCandidate's recency bonus -- so writing it without the lock races
// all three (docs/TEST-PLAN.md #101).
//
// Exists because un-embedding the mutex made that write impossible to do
// correctly from outside this package, which is the point: the inbound
// HealthCheck handler in internal/server stamps this when a peer calls us, and
// before this it did so bare.
//
// Takes an instant rather than always using time.Now() so that a caller
// reconstructing a known state -- a test, or any future restore path -- can say
// what it means. Production callers pass time.Now().
func (m *Member) SetLastResponse(when time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastHCResponse = when
}

// SetCapacity records how many Floating IPs this member may hold.
//
// Not part of the claim: capacity is a configured limit an operator sets, not
// something the member asserts about its own state. It is read under this lock
// to build the placement snapshots in MemberList and the health checker.
func (m *Member) SetCapacity(capacity int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Capacity = capacity
}

// GetActiveIPs returns a copy of the IPs this member currently hosts.
//
// Callers deciding what a node should hold need this under the member lock --
// the health check loop and the IP monitor both read it while promotions and
// rebalance moves are writing it.
func (m *Member) GetActiveIPs() []string {
	return m.Claim().ActiveIPs
}

// GetStatus returns this member's status, read under the member lock.
//
// Status is written under that lock by promotions, the health check loop and the
// maintenance transitions, so deciding anything from a bare read races with all
// three.
func (m *Member) GetStatus() MemberStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Status
}

// SetStatus records this member's status under the member lock, leaving the
// addresses it claims alone.
//
// Prefer SetClaim where the addresses move too. This is for a genuine
// status-only transition -- ExitMaintenance returning a member to Passive, which
// it reaches holding nothing.
func (m *Member) SetStatus(status MemberStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Status = status
}

// MarkUnreachable records that this member is no longer known to hold any
// floating IPs -- status Unknown with an empty assignment set.
//
// Promotion uses this when an incumbent could not be reached, so the addresses
// it may still be holding can be accounted for elsewhere. The two fields have to
// move together under one lock: a reader seeing Unknown against the old
// ActiveIPs would conclude a down node still owns the group. That is what a
// Claim is, so this is now a named case of one.
func (m *Member) MarkUnreachable() {
	m.SetClaim(Claim{Status: StatusUnknown})
}

// claimLocked and setClaimLocked are the claim accessors for code that already
// holds the member lock, per this codebase's xLocked convention. pulselock is
// not reentrant, so the exported versions cannot be reached from inside it.
func (m *Member) claimLocked() Claim {
	return Claim{Status: m.Status, ActiveIPs: m.ActiveIPs}.copyIPs()
}

func (m *Member) setClaimLocked(c Claim) {
	c = c.copyIPs()
	m.Status = c.Status
	m.ActiveIPs = c.ActiveIPs
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
	m.mu.Lock()
	isLocal := m.IsLocal()
	status := m.Status
	m.mu.Unlock()

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
	m.mu.Lock()
	defer m.mu.Unlock()

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
	m.mu.Lock()
	if m.Status == StatusMaintenance {
		m.mu.Unlock()
		return nil
	}
	// Snapshotted rather than passed by reference, because the field is cleared below and
	// the bring-down runs between the two critical sections.
	var toRelease []string
	if m.Status == StatusActive && len(m.ActiveIPs) > 0 {
		toRelease = append([]string{}, m.ActiveIPs...)
	}
	m.mu.Unlock()

	if len(toRelease) > 0 {
		// Bring down all hosted IPs before leaving active state
		_ = m.BringDownIPs(toRelease)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-checked: the status can have moved while the addresses were coming down, and a node
	// that is no longer Active must not have its assignment list cleared out from under
	// whatever moved it.
	if m.Status == StatusActive {
		m.ActiveIPs = nil
	}
	m.Status = StatusMaintenance
	return nil
}

// ExitMaintenance returns this member to passive state so it can be
// considered for promotion again.
func (m *Member) ExitMaintenance() error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
