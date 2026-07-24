package membership

import (
	"context"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/ipam"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/utils"
	rpc "github.com/syleron/pulseha/rpc"
)

// Object pools for health checker to reduce memory allocations
var (
	memberStatusMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]MemberStatus, 8)
		},
	}
)

// Helper functions for health checker object pools
func getMemberStatusMap() map[string]MemberStatus {
	m := memberStatusMapPool.Get().(map[string]MemberStatus)
	// Clear the map
	for k := range m {
		delete(m, k)
	}
	return m
}

func putMemberStatusMap(m map[string]MemberStatus) {
	if m != nil {
		memberStatusMapPool.Put(m)
	}
}

// ServerReference is an interface for the server methods needed by the health checker
type ServerReference interface {
	// Add methods that the health checker needs to call on the server
	GetQuorumManager() *quorum.QuorumManager
	OrchestrateIPFailover(oldNodeID, newNodeID string, ips []string) error
	// Cluster-state convergence helpers
	GetClusterEpoch() int64
	BroadcastClusterState(memberStates map[string]MemberStatus, epoch int64, leaderID string,
		leases map[string]string) error
	// Leader getters for lease-based failover
	GetLeaderID() string
	GetLeaderLeaseUntil() time.Time
	// IP monitor refresh
	RefreshLocalMonitorExpectedIPs()
	// Vote broadcasting for quorum elections
	BroadcastVoteRequest(sessionID string, voteType, subject, description string, timeoutSeconds int64) error
	// Promotion orchestration
	Promote(ctx context.Context, req *rpc.PromoteRequest) (*rpc.PromoteResponse, error)
}

// HealthCheck represents the result of a health check
type HealthCheck struct {
	IP        string
	Available bool
	Latency   time.Duration
	Error     error
}

// HealthChecker handles health checking for nodes and IPs
type HealthChecker struct {
	sync.RWMutex
	members             *MemberList
	checkTicker         *time.Ticker
	stopChan            chan struct{}
	stopOnce            sync.Once // Ensure we only close stopChan once
	logger              *log.Logger
	ready               bool
	stopped             bool // Track if we're stopped
	server              ServerReference
	lastClusterState    string    // Track last cluster state to only log changes
	checksWithoutChange int       // Counter for periodic status logs
	lastLeaderBroadcast time.Time // suppress elections briefly after leader broadcast
	lastTick            time.Time // last time a check cycle executed
	loggedNoMembers        bool            // Tracks if a no-member condition has already been logged in the current state
	deepCheckCounter       int             // incremented each cycle; triggers cluster-membership gRPC check every 5 cycles
	membershipCheckFailed  map[string]bool // nodes that failed the last deep membership check; cleared only when deep check passes
	aaReconcileCycles      int             // cycles since startup; active-active reconciliation waits for a grace period
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(members *MemberList, logger *log.Logger) *HealthChecker {
	if logger == nil {
		logger = log.New(nil)
	}
	return &HealthChecker{
		members:               members,
		logger:                logger,
		stopChan:              make(chan struct{}),
		membershipCheckFailed: make(map[string]bool),
	}
}

// SetServerReference sets the server reference for the health checker
func (h *HealthChecker) SetServerReference(server ServerReference) {
	h.Lock()
	defer h.Unlock()
	h.server = server
	h.logger.Debug("Server reference set for health checker")
}

// Start begins the health checking process
func (h *HealthChecker) Start(interval time.Duration) {
	h.Lock()
	defer h.Unlock()

	if h.stopped {
		h.logger.Debug("Health checker is stopped, reinitializing...")
		h.stopChan = make(chan struct{})
		h.stopped = false
	}
	h.checkTicker = time.NewTicker(interval)
	h.ready = true

	// Add initial delay before starting health checks
	h.logger.Debug("Adding initial delay before starting health checks...")
	time.Sleep(500 * time.Millisecond)

	go h.run()
	h.logger.Debug("Health checker is now running")
}

// IsRunning returns true if the health checker is currently running
func (h *HealthChecker) IsRunning() bool {
	h.RLock()
	defer h.RUnlock()
	return h.ready && !h.stopped
}

// Stop halts the health checking process
func (h *HealthChecker) Stop() {
	h.Lock()
	defer h.Unlock()

	h.logger.Debug("Stopping health checker...")

	// Set flags first to prevent new checks from starting
	h.ready = false
	h.stopped = true

	// Stop the ticker
	if h.checkTicker != nil {
		h.checkTicker.Stop()
		h.checkTicker = nil
	}

	// Only close stopChan once
	h.stopOnce.Do(func() {
		h.logger.Debug("Closing stop channel...")
		close(h.stopChan)
	})
}

// run executes the health check loop
func (h *HealthChecker) run() {
	h.logger.Debug("Health check loop started")
	for {
		select {
		case <-h.stopChan:
			h.logger.Debug("Health checker stopping")
			return
		default:
			h.RLock()
			if !h.ready || h.stopped || h.checkTicker == nil {
				h.RUnlock()
				return
			}
			ticker := h.checkTicker
			h.RUnlock()

			select {
			case <-ticker.C:
				// Record heartbeat tick
				h.Lock()
				h.lastTick = time.Now()
				h.Unlock()
				h.RLock()
				if !h.ready || h.stopped {
					h.RUnlock()
					return
				}
				h.RUnlock()

				// Removed debug log to reduce noise - health checks run every second
				h.performHealthChecks()
			case <-h.stopChan:
				h.logger.Debug("Health checker stopping")
				return
			}
		}
	}
}

// LastTickTime returns the timestamp of the last check tick
func (h *HealthChecker) LastTickTime() time.Time {
	h.RLock()
	defer h.RUnlock()
	return h.lastTick
}

// performHealthChecks executes health checks on all nodes and their IPs
func (h *HealthChecker) performHealthChecks() {
	defer h.Unlock()
	h.Lock()
	h.logger.Debug("HEALTH_CHECK: Starting health check cycle...")
	h.deepCheckCounter++
	doDeepCheck := h.deepCheckCounter%5 == 0
	membersSnapshot := h.members.MembersSnapshot()
	memberCount := len(membersSnapshot)
	if memberCount == 0 {
		// Use a field to print the "no members" message only once to the logs.
		if !h.loggedNoMembers {
			h.logger.Warn("No members in cluster, skipping health check. " +
				"This message will only be logged once until members are added.")
			h.loggedNoMembers = true
		}
		// No health check is needed when no members exist.
		return
	}
	// Reset the "no members" log field, as we now have members.
	h.loggedNoMembers = false
	// Collect cluster status information for a single consolidated log
	clusterStatus := make([]string, 0, memberCount)
	clusterStatusForComparison := make([]string, 0, memberCount)
	var failedMembers []string
	var statusChanges []string

	// Check if we are a passive node and need to detect active node failure
	var localMember *Member
	for _, m := range membersSnapshot {
		if m.IsLocal() {
			localMember = m
			break
		}
	}

	for _, member := range membersSnapshot {
		// If this is the local node, just update its health check time
		if member.IsLocal() {
			member.Lock()
			member.LastHCResponse = time.Now()
			member.Latency = "0ms"
			member.Unlock()
			// Add to display status (local node)
			clusterStatus = append(clusterStatus, fmt.Sprintf("%s(local/%s)",
				member.Hostname, StatusToString(member.Status)))

			// Add to comparison status (without latency for change detection)
			clusterStatusForComparison = append(clusterStatusForComparison, fmt.Sprintf("%s(%s)",
				member.Hostname, StatusToString(member.Status)))
			continue
		}

		// Store previous state for change detection
		member.Lock()
		wasUnknown := member.Status == StatusUnknown
		member.Unlock()

		// Check node connectivity. Every 5th cycle we do a full cluster-membership gRPC
		// check (verifies the remote node shares our cluster token, catching split-cluster
		// scenarios where a rebooted peer is running its own isolated cluster).
		// All other cycles use a cheap TCP dial for low-latency failure detection.
		//
		// Once a node fails the deep check it stays unreachable until the next deep check
		// passes — a passing TCP dial alone is not enough to clear the failed state.
		startTime := time.Now()
		var isReachable bool
		if doDeepCheck {
			h.logger.Debugf("About to deep-check cluster membership for %s (IP:%s Port:%s)", member.Hostname, member.IP, member.Port)
			isReachable = h.checkClusterMembership(member)
			h.membershipCheckFailed[member.ID] = !isReachable
		} else {
			h.logger.Debugf("About to check connectivity for %s (IP:%s Port:%s)", member.Hostname, member.IP, member.Port)
			isReachable = h.checkNodeConnectivity(member)
			// If the last deep check determined this node is in a foreign cluster,
			// keep it marked unreachable regardless of TCP reachability.
			if isReachable && h.membershipCheckFailed[member.ID] {
				h.logger.Debugf("Node %s passes TCP but failed last membership check; treating as unreachable", member.Hostname)
				isReachable = false
			}
		}
		responseTime := time.Since(startTime)
		h.logger.Debugf("Connectivity check result for %s: reachable=%v, responseTime=%v", member.Hostname, isReachable,
			responseTime)

		member.Lock()
		member.LastHCResponse = time.Now()

		if !isReachable {
			// Mark node as unknown when unreachable
			previousStatus := member.Status
			member.Status = StatusUnknown
			member.Latency = "N/A"
			member.Unlock()

			// Log status change if node went from reachable to unreachable
			if previousStatus != StatusUnknown {
				statusChanges = append(statusChanges, fmt.Sprintf("%s became unreachable (was %s)",
					member.Hostname, StatusToString(previousStatus)))
				// Immediate convergence nudge on status change
				if h.server != nil {
					states := getMemberStatusMap()
					for id, m := range membersSnapshot {
						m.Lock()
						states[id] = m.Status
						m.Unlock()
					}
					_ = h.server.BroadcastClusterState(states, h.server.GetClusterEpoch()+1, h.getCurrentLeaderID(),
						nil)
					putMemberStatusMap(states)
					h.lastLeaderBroadcast = time.Now()
				}
			}

			clusterStatus = append(clusterStatus, fmt.Sprintf("%s(unreachable/%s)",
				member.Hostname, StatusToString(member.Status)))
			clusterStatusForComparison = append(clusterStatusForComparison, fmt.Sprintf("%s(%s)",
				member.Hostname, StatusToString(member.Status)))
			failedMembers = append(failedMembers, member.Hostname)
			continue
		}

		// Node is reachable - update latency once
		member.Latency = fmt.Sprintf("%.2fms", float64(responseTime.Nanoseconds())/1000000)

		// Handle auto-failback for previously failed nodes
		autoFailback := h.members.config.Pulse.AutoFailback
		mode := h.members.config.Pulse.Mode

		if wasUnknown && autoFailback {
			switch mode {
			case "active-passive":
				activeExists := false
				for _, otherMember := range membersSnapshot {
					if otherMember.ID != member.ID && otherMember.Status == StatusActive {
						activeExists = true
						break
					}
				}
				if !activeExists {
					member.Status = StatusActive
					statusChanges = append(statusChanges, fmt.Sprintf("%s promoted to active", member.Hostname))
				} else {
					member.Status = StatusPassive
					statusChanges = append(statusChanges, fmt.Sprintf("%s restored to passive", member.Hostname))
				}
			case "active-active":
				member.Status = StatusActive
				statusChanges = append(statusChanges, fmt.Sprintf("%s restored to active", member.Hostname))
			default:
				member.Status = StatusPassive
				statusChanges = append(statusChanges, fmt.Sprintf("%s restored to passive", member.Hostname))
			}
		} else if member.Status == StatusUnknown {
			member.Status = StatusPassive
			statusChanges = append(statusChanges, fmt.Sprintf("%s recovered to passive", member.Hostname))
		}

		// Add to display status (with latency for display)
		clusterStatus = append(clusterStatus, fmt.Sprintf("%s(%s/%s)",
			member.Hostname, member.Latency, StatusToString(member.Status)))

		// Add to comparison status (without latency for change detection)
		clusterStatusForComparison = append(clusterStatusForComparison, fmt.Sprintf("%s(%s)",
			member.Hostname, StatusToString(member.Status)))

		member.Unlock()

		// Floating IP health checks are disabled; failover decisions are based solely on node health
	}

	// Sort status for consistent comparison (status without latency)
	sort.Strings(clusterStatusForComparison)
	currentClusterStateForComparison := strings.Join(clusterStatusForComparison, ", ")

	// Sort display status for consistent ordering
	sort.Strings(clusterStatus)
	currentClusterDisplayState := strings.Join(clusterStatus, ", ")

	// Only log if the cluster state has changed (ignoring latency variations)
	if currentClusterStateForComparison != h.lastClusterState {
		h.logger.Infof("HEALTH_CHECK: Cluster state changed - %s", currentClusterDisplayState)
		h.logger.Debug("HEALTH_CHECK: Previous state was", "lastState", h.lastClusterState)
		h.lastClusterState = currentClusterStateForComparison
		h.checksWithoutChange = 0

		// Proactively broadcast updated member states so all nodes converge quickly
		if h.server != nil {
			h.logger.Debug("HEALTH_CHECK: Broadcasting cluster state due to health check changes")
			states := getMemberStatusMap()
			for id, m := range membersSnapshot {
				m.Lock()
				states[id] = m.Status
				m.Unlock()
			}
			_ = h.server.BroadcastClusterState(states, h.server.GetClusterEpoch()+1, h.getCurrentLeaderID(), nil)
			putMemberStatusMap(states)
			h.lastLeaderBroadcast = time.Now()
			h.logger.Debug("HEALTH_CHECK: Cluster state broadcast completed")
		}
	} else {
		// Increment counter for unchanged state
		h.checksWithoutChange++

		// Heartbeat convergence nudge every 3 checks (~3s) to advance LastResponse and align peers
		if h.server != nil && h.checksWithoutChange%3 == 0 {
			h.logger.Debug("HEALTH_CHECK: Performing heartbeat convergence nudge", "checksWithoutChange",
				h.checksWithoutChange)
			states := getMemberStatusMap()
			for id, m := range membersSnapshot {
				m.Lock()
				states[id] = m.Status
				// Also advance local LastResponse to now for consistent display
				m.LastHCResponse = time.Now()
				m.Unlock()
			}
			_ = h.server.BroadcastClusterState(states, h.server.GetClusterEpoch()+1, h.getCurrentLeaderID(), nil)
			putMemberStatusMap(states)
			h.logger.Debug("HEALTH_CHECK: Heartbeat convergence broadcast completed")
		}

		// Log periodic summary every 60 checks (roughly every minute with 1s interval)
		if h.checksWithoutChange >= 60 {
			h.logger.Infof("Cluster stable for 60 checks: %s", currentClusterDisplayState)
			h.checksWithoutChange = 0
		}
	}

	// Log any status changes
	for _, change := range statusChanges {
		h.logger.Infof("Status change: %s", change)
	}

	// Log any failed members (already captured in status change, so skip if unchanged)
	// The cluster state change will already indicate when nodes become unreachable

	// Check for active node failure and initiate failover if needed
	if localMember != nil {
		h.logger.Debug("HEALTH_CHECK: Local member has status, checking for active node failure", "hostname",
			localMember.Hostname, "status", StatusToString(localMember.Status))
		// Always check for active node failure, not just when passive
		h.checkForActiveNodeFailure()
	} else {
		// Debug why no local member found - this indicates a serious configuration issue
		localNodeID, err := h.members.config.GetLocalNodeUUID()
		memberCount := len(membersSnapshot)
		var memberIDs []string
		var memberDetails []string
		for id, m := range membersSnapshot {
			memberIDs = append(memberIDs, id)
			// Check each member's IsLocal() status for debugging
			isLocal := m.IsLocal()
			memberDetails = append(memberDetails, fmt.Sprintf("%s(isLocal=%v,hostname=%s)", id, isLocal, m.Hostname))
		}

		h.logger.Warnf("HEALTH_CHECK: No local member found! This indicates config/member mismatch. "+
			"LocalNodeID=%s (err=%v), MemberCount=%d, MemberIDs=%v, MemberDetails=%v",
			localNodeID, err, memberCount, memberIDs, memberDetails)

		// Additional diagnostic logging
		if h.members.config != nil {
			h.logger.Info("HEALTH_CHECK: MemberList config state",
				"local_node_id", localNodeID,
				"cluster_check", h.members.config.ClusterCheck(),
				"node_count_in_config", len(h.members.config.Nodes))
		} else {
			h.logger.Error("HEALTH_CHECK: MemberList config is nil!")
		}
	}
}

// getCurrentLeaderID returns the ID of the current active node if any
func (h *HealthChecker) getCurrentLeaderID() string {
	members := h.members.MembersSnapshot()

	for id, m := range members {
		m.Lock()
		isActive := m.Status == StatusActive
		m.Unlock()
		if isActive {
			return id
		}
	}
	return ""
}

// checkForActiveNodeFailure checks if the active node has failed and initiates failover
func (h *HealthChecker) checkForActiveNodeFailure() {
	h.logger.Debug("ACTIVE_CHECK: Starting active node failure check")

	members := h.members.MembersSnapshot()
	config := h.members.config

	// Find the active node
	var activeMember *Member
	var memberStatuses []string
	for _, member := range members {
		member.Lock()
		isActive := member.Status == StatusActive
		status := StatusToString(member.Status)
		memberStatuses = append(memberStatuses, fmt.Sprintf("%s:%s", member.Hostname, status))
		member.Unlock()
		if isActive {
			activeMember = member
		}
	}

	// In active-active mode, all eligible nodes are StatusActive. The
	// coordinator reconciles orphaned IPs (dead nodes, restarts) and
	// rebalances load onto empty nodes (fresh joins, failback) every cycle.
	if config.Pulse.Mode == "active-active" {
		h.reconcileActiveActive(members)
		return
	}

	if activeMember == nil {
		h.logger.Error("ACTIVE_CHECK: No active node found in cluster, initiating leader election")
		h.electNewActiveNode()
		return
	}

	h.logger.Debug("ACTIVE_CHECK: Active node found", "hostname", activeMember.Hostname, "nodeID", activeMember.ID)

	// Check if the active node has been unreachable for too long
	member := activeMember
	member.Lock()
	timeSinceLastResponse := time.Since(member.LastHCResponse)
	isUnreachable := member.Status == StatusUnknown ||
		timeSinceLastResponse > time.Duration(config.Pulse.FailOverLimit)*time.Millisecond
	hostname := member.Hostname
	activeIPs := member.ActiveIPs
	member.Unlock()

	h.logger.Debug("ACTIVE_CHECK: Active node health status", "hostname", hostname, "timeSinceLastResponse",
		timeSinceLastResponse, "failOverLimit", config.Pulse.FailOverLimit, "isUnreachable", isUnreachable)

	if isUnreachable {
		h.logger.Warn("ACTIVE_CHECK: Active node has been unreachable, initiating failover", "hostname", hostname,
			"timeSinceLastResponse", timeSinceLastResponse, "failOverLimit", config.Pulse.FailOverLimit)

		// Mark the active node as unknown
		member.Lock()
		oldNodeID := member.ID
		activeIPsCopy := append([]string{}, activeIPs...)
		member.Status = StatusUnknown
		member.Unlock()

		// Elect a new active node and transfer IPs
		h.logger.Info("ACTIVE_CHECK: Starting leader election due to failed active node", "failedNode", hostname)
		h.electNewActiveNode()

		// Transfer the failed node's IPs to the new active using server IP helpers
		newActive := h.findActiveNode()
		if newActive != nil && len(activeIPsCopy) > 0 {
			h.logger.Infof("Transferring %d IPs from failed active node to new active", len(activeIPsCopy))
			if h.server != nil {
				if err := h.server.OrchestrateIPFailover(oldNodeID, newActive.ID, activeIPsCopy); err != nil {
					h.logger.Errorf("Failed to transfer IPs to new active node: %v", err)
				} else {
					// Update member IP state
					newActive.Lock()
					newActive.ActiveIPs = append([]string{}, activeIPsCopy...)
					newActive.Unlock()

					member.Lock()
					member.ActiveIPs = nil
					member.Unlock()
				}
			}
		}
	}
}

// aaReconcileGraceCycles is the number of health-check cycles to wait after
// startup before making active-active redistribution decisions. It gives
// initial health checks and peer self-reports (which carry each node's
// ActiveIPs) time to converge so we don't redistribute IPs that a peer is
// actually still hosting.
const aaReconcileGraceCycles = 10

// reconcileActiveActive keeps floating IPs assigned and balanced in
// active-active mode. Group IPs no longer hosted by any healthy node are
// redistributed, and IPs are moved onto underloaded nodes. Only the
// coordinator — the healthy node with the lowest ID — acts, so concurrent
// redistribution from multiple nodes can't race.
func (h *HealthChecker) reconcileActiveActive(members map[string]*Member) {
	h.aaReconcileCycles++
	if h.aaReconcileCycles < aaReconcileGraceCycles {
		return
	}

	localID, err := h.members.config.GetLocalNodeUUID()
	if err != nil {
		return
	}
	if activeActiveCoordinator(members) != localID {
		return
	}

	deduped := h.resolveDuplicateAssignments(members)
	redistributed := h.redistributeOrphanedIPs(members)
	moved := h.rebalanceActiveActive(members)

	// Broadcast so peers converge on the new assignments quickly.
	if (deduped || redistributed || moved) && h.server != nil {
		states := getMemberStatusMap()
		for id, m := range members {
			m.Lock()
			states[id] = m.Status
			m.Unlock()
		}
		_ = h.server.BroadcastClusterState(states, h.server.GetClusterEpoch()+1, h.getCurrentLeaderID(), nil)
		putMemberStatusMap(states)
	}
}

// activeActiveCoordinator returns the ID of the node that should orchestrate
// active-active redistribution: the healthy (Active or Passive) node with the
// lowest ID. Returns "" when no healthy node exists.
func activeActiveCoordinator(members map[string]*Member) string {
	coordinator := ""
	for id, member := range members {
		member.Lock()
		healthy := member.Status == StatusActive || member.Status == StatusPassive
		member.Unlock()
		if healthy && (coordinator == "" || id < coordinator) {
			coordinator = id
		}
	}
	return coordinator
}

// resolveDuplicateAssignments finds IPs tracked by more than one healthy
// member and removes them everywhere but the first owner (lowest node ID).
// Convergence races can transiently double-assign an IP; left alone that
// means two nodes ARP-fighting over the same address. Returns true if any
// duplicate was resolved.
func (h *HealthChecker) resolveDuplicateAssignments(members map[string]*Member) bool {
	resolved := false
	owners := make(map[string]*Member)
	for _, node := range rebalanceCandidates(members) {
		node.Lock()
		ips := append([]string{}, node.ActiveIPs...)
		node.Unlock()
		for _, ip := range ips {
			owner, seen := owners[ip]
			if !seen {
				owners[ip] = node
				continue
			}
			h.logger.Warnf("ACTIVE_CHECK: IP %s assigned to both %s and %s, removing from %s",
				ip, owner.Hostname, node.Hostname, node.Hostname)
			node.Lock()
			node.ActiveIPs = removeIPFromList(node.ActiveIPs, ip)
			node.Unlock()
			if err := node.BringDownIPs([]string{ip}); err != nil {
				h.logger.Error("ACTIVE_CHECK: failed to bring down duplicate IP", "ip", ip,
					"hostname", node.Hostname, "error", err)
			}
			resolved = true
		}
	}
	return resolved
}

// redistributeOrphanedIPs reassigns group IPs that no healthy node currently
// hosts — IPs stranded on failed nodes or lost entirely (e.g. a node
// rebooted and came back empty). Returns true if anything was redistributed.
func (h *HealthChecker) redistributeOrphanedIPs(members map[string]*Member) bool {
	// Collect IPs hosted by healthy nodes; clear stranded assignments from
	// failed nodes so their IPs count as orphaned.
	hosted := make(map[string]bool)
	for _, member := range members {
		member.Lock()
		switch member.Status {
		case StatusActive, StatusPassive:
			for _, ip := range member.ActiveIPs {
				hosted[ip] = true
			}
		case StatusUnknown:
			if len(member.ActiveIPs) > 0 {
				h.logger.Warnf("ACTIVE_CHECK: clearing %d IP(s) stranded on failed node %s",
					len(member.ActiveIPs), member.Hostname)
				member.ActiveIPs = nil
				member.LoadFactor = 0
			}
		}
		member.Unlock()
	}

	orphaned := orphanedGroupIPs(h.members.config.Groups, hosted)
	if len(orphaned) == 0 {
		return false
	}

	// Quorum-gate redistribution the same way partial IP failures are, so a
	// minority partition can't grab IPs the majority side still serves.
	if len(members) >= 3 && !h.initiateIPRedistributionVote(orphaned) {
		h.logger.Warn("ACTIVE_CHECK: quorum vote failed, not redistributing orphaned IPs")
		return false
	}

	h.logger.Warnf("ACTIVE_CHECK: redistributing %d orphaned floating IP(s)", len(orphaned))
	if err := h.members.RedistributeIPs(orphaned); err != nil {
		h.logger.Error("ACTIVE_CHECK: failed to redistribute orphaned IPs", "error", err)
		return false
	}
	return true
}

// orphanedGroupIPs returns all configured group IPs not present in hosted,
// sorted for deterministic redistribution.
func orphanedGroupIPs(groups map[string][]string, hosted map[string]bool) []string {
	var orphaned []string
	for _, ips := range groups {
		for _, ip := range ips {
			if !hosted[ip] {
				orphaned = append(orphaned, ip)
			}
		}
	}
	sort.Strings(orphaned)
	return orphaned
}

// rebalanceActiveActive moves IPs from the most-loaded node to the
// least-loaded one until the cluster is balanced (difference <= 1). This is
// what tops up freshly joined and failed-back nodes, which otherwise sit
// Active but empty forever. Returns true if any IP was moved.
func (h *HealthChecker) rebalanceActiveActive(members map[string]*Member) bool {
	if h.server == nil {
		return false
	}

	totalIPs := 0
	for _, ips := range h.members.config.Groups {
		totalIPs += len(ips)
	}

	moved := false
	// Each successful move shrinks the imbalance, so the total IP count
	// bounds the loop.
	for i := 0; i < totalIPs; i++ {
		src, dst, ip := planRebalanceMove(rebalanceCandidates(members))
		if src == nil {
			break
		}
		h.logger.Infof("ACTIVE_CHECK: rebalancing IP %s from %s to %s", ip, src.Hostname, dst.Hostname)
		if err := h.server.OrchestrateIPFailover(src.ID, dst.ID, []string{ip}); err != nil {
			h.logger.Error("ACTIVE_CHECK: rebalance move failed", "ip", ip, "error", err)
			break
		}
		src.Lock()
		src.ActiveIPs = removeIPFromList(src.ActiveIPs, ip)
		src.Unlock()
		dst.Lock()
		dst.Status = StatusActive
		dst.ActiveIPs = append(dst.ActiveIPs, ip)
		dst.Unlock()
		moved = true
	}
	return moved
}

// rebalanceCandidates returns the healthy members eligible for rebalancing,
// sorted by ID so move planning is deterministic.
func rebalanceCandidates(members map[string]*Member) []*Member {
	var nodes []*Member
	for _, member := range members {
		member.Lock()
		eligible := member.Status == StatusActive || member.Status == StatusPassive
		member.Unlock()
		if eligible {
			nodes = append(nodes, member)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}

// planRebalanceMove returns the next IP move that reduces imbalance across
// nodes, or nils when the assignment is already balanced (max-min <= 1).
// Nodes must be sorted by ID so ties resolve deterministically.
func planRebalanceMove(nodes []*Member) (*Member, *Member, string) {
	snapshots := make([]ipam.Node, 0, len(nodes))
	for _, node := range nodes {
		node.Lock()
		snapshots = append(snapshots, ipam.Node{
			Hostname: node.Hostname,
			IPCount:  len(node.ActiveIPs),
			Capacity: node.Capacity,
		})
		node.Unlock()
	}

	srcIdx, dstIdx, ok := ipam.PlanMove(snapshots)
	if !ok {
		return nil, nil, ""
	}

	src, dst := nodes[srcIdx], nodes[dstIdx]
	src.Lock()
	ip := src.ActiveIPs[len(src.ActiveIPs)-1]
	src.Unlock()
	return src, dst, ip
}

// removeIPFromList returns ips with target removed.
func removeIPFromList(ips []string, target string) []string {
	out := ips[:0]
	for _, ip := range ips {
		if ip != target {
			out = append(out, ip)
		}
	}
	return out
}

// electNewActiveNode elects a new active node using deterministic backoff to prevent races
func (h *HealthChecker) electNewActiveNode() {
	h.logger.Info("ELECTION: Starting leader election process")

	localNodeID, err := h.members.config.GetLocalNodeUUID()
	if err != nil {
		h.logger.Error("Failed to get local node ID for election")
		return
	}

	// Step 1: Calculate deterministic backoff delay to prevent simultaneous elections
	backoffDelay, isCoordinator := h.calculateElectionBackoffWithRole(localNodeID)

	if !isCoordinator {
		// Non-coordinators are purely passive and never promote themselves
		// This follows industry standard (keepalived/VRRP) where backups only monitor
		// If coordinator fails, next health check cycle will elect new coordinator
		h.logger.Info("ELECTION: This node is not the coordinator, monitoring for active node appearance",
			"monitorDuration", backoffDelay+(10*time.Second))

		// Monitor for active node with polling
		deadline := time.Now().Add(backoffDelay + (10 * time.Second))
		pollInterval := 1 * time.Second

		for time.Now().Before(deadline) {
			time.Sleep(pollInterval)

			if h.findActiveNode() != nil {
				h.logger.Info("ELECTION: Active node appeared, election succeeded")
				return
			}
		}

		h.logger.Warn("ELECTION: No active node appeared within timeout. Coordinator may have failed during election. Next health check cycle will recalculate coordinator.")
		return
	}

	// Only coordinators reach this point
	h.logger.Info("ELECTION: This node is the coordinator, proceeding with election immediately")

	if backoffDelay > 0 {
		// Coordinator applies minimal delay to allow cluster state to stabilize
		h.logger.Infof("ELECTION: Coordinator applying brief stabilization delay: %v", backoffDelay)
		time.Sleep(backoffDelay)

		// Check if active node appeared during delay
		if h.findActiveNode() != nil {
			h.logger.Info("ELECTION: Active node appeared during stabilization, aborting election")
			return
		}
	}

	h.logger.Info("ELECTION: Coordinator proceeding with election")

	// Step 2: Select best candidate
	bestCandidate := h.selectBestCandidate()
	if bestCandidate == nil {
		h.logger.Error("ELECTION: No eligible candidates found")
		return
	}

	h.logger.Infof("ELECTION: Selected candidate: %s", bestCandidate.Hostname)

	// Step 3: Try voting first, then promote directly if voting fails
	if h.attemptVotingElection(bestCandidate) {
		h.logger.Info("ELECTION: Voting election succeeded, promoting candidate")
		if h.tryForcePromote(bestCandidate) {
			return
		}
		// Explicitly set status after successful voting
		bestCandidate.Lock()
		bestCandidate.Status = StatusActive
		bestCandidate.Unlock()
		h.logger.Infof("ELECTION: Promoted %s to Active after successful vote", bestCandidate.Hostname)

		// Trigger IP refresh to bring up VIPs after successful voting
		if h.server != nil {
			h.logger.Info("HEALTH_CHECK: Triggering IP refresh after voting success to bring up VIPs")
			h.server.RefreshLocalMonitorExpectedIPs()
		}
	} else {
		h.logger.Info("ELECTION: Voting failed, checking if active node appeared before direct promotion")
		if h.tryForcePromote(bestCandidate) {
			return
		}

		// CRITICAL: Re-check if an active node appeared while we were voting
		// This prevents multiple nodes from promoting themselves simultaneously
		if activeNode := h.findActiveNode(); activeNode != nil {
			h.logger.Info("ELECTION: Active node appeared during voting, aborting promotion", "activeNode",
				activeNode.Hostname)
			return
		}

		h.logger.Info("ELECTION: No active node found, promoting candidate directly")
		// Since we've already coordinated with deterministic backoff, this node
		// is the designated winner and can promote the candidate directly
		bestCandidate.Lock()
		bestCandidate.Status = StatusActive
		bestCandidate.Unlock()
		h.logger.Infof("ELECTION: Promoted %s to Active", bestCandidate.Hostname)

		// Trigger IP refresh to bring up VIPs after promotion
		// This is needed because we disabled automatic refresh in ConfigSync to prevent GARP storms
		// but we still need to bring up VIPs when a node becomes Active after failover
		if h.server != nil {
			h.logger.Info("HEALTH_CHECK: Triggering IP refresh after promotion to bring up VIPs")
			h.server.RefreshLocalMonitorExpectedIPs()
		}
	}
}

// findElectionCoordinator returns the ID of the node that should coordinate elections
func (h *HealthChecker) findElectionCoordinator() string {
	var coordinatorID string
	for nodeID, member := range h.members.MembersSnapshot() {
		member.Lock()
		status := member.Status
		member.Unlock()

		// Only consider available nodes
		if status == StatusPassive || status == StatusUnknown {
			if coordinatorID == "" || nodeID < coordinatorID {
				coordinatorID = nodeID
			}
		}
	}
	return coordinatorID
}

// selectBestCandidate finds the best node to promote to active
func (h *HealthChecker) selectBestCandidate() *Member {
	var bestCandidate *Member
	var bestScore float64 = -1

	for _, member := range h.members.MembersSnapshot() {
		member.Lock()
		status := member.Status
		latencyStr := member.Latency
		lastResponse := member.LastHCResponse
		isLocal := member.IsLocal()
		member.Unlock()

		// Skip if already active
		if status == StatusActive {
			continue
		}

		// Calculate score
		score := float64(0)

		// Base score by status.
		// StatusMaintenance hits the else branch and is skipped — never promoted.
		if status == StatusPassive {
			score += 50
		} else if status == StatusUnknown {
			score += 25
		} else {
			continue
		}

		// Small local preference
		if isLocal {
			score += 5
		}

		// Latency score
		if latencyStr != "N/A" && latencyStr != "" {
			if lat, err := time.ParseDuration(strings.TrimSuffix(latencyStr, "ms") + "ms"); err == nil {
				latencyScore := math.Max(0, 20-(float64(lat.Milliseconds())/50))
				score += latencyScore
			}
		}

		// Recent response bonus
		if !lastResponse.IsZero() {
			recency := time.Since(lastResponse)
			if recency < 5*time.Second {
				score += 10
			}
		}

		// Deterministic tie-breaker
		for i, b := range member.ID {
			if i >= 4 {
				break
			}
			score += float64(b) / 1000.0
		}

		h.logger.Debugf("Candidate %s: score=%.2f, status=%s",
			member.Hostname, score, StatusToString(status))

		if score > bestScore {
			bestCandidate = member
			bestScore = score
		}
	}

	return bestCandidate
}

// waitForCoordinatorElection waits for coordinator to complete election, with timeout fallback
func (h *HealthChecker) waitForCoordinatorElection() {
	timeout := time.After(15 * time.Second)
	checkInterval := time.NewTicker(2 * time.Second)
	defer checkInterval.Stop()

	for {
		select {
		case <-timeout:
			h.logger.Warn("Coordinator election timeout, using emergency fallback")
			h.emergencyFallback()
			return
		case <-checkInterval.C:
			// Check if coordinator succeeded
			for _, member := range h.members.MembersSnapshot() {
				member.Lock()
				status := member.Status
				member.Unlock()
				if status == StatusActive {
					h.logger.Debug("Coordinator election completed successfully")
					return
				}
			}
		}
	}
}

// attemptVotingElection tries the voting system with timeout
func (h *HealthChecker) attemptVotingElection(candidate *Member) bool {
	h.logger.Debug("Attempting voting-based election")

	// Count available nodes for voting
	availableCount := 0
	for _, member := range h.members.MembersSnapshot() {
		member.Lock()
		status := member.Status
		member.Unlock()
		if status == StatusPassive || status == StatusUnknown {
			availableCount++
		}
	}

	if availableCount < 3 {
		h.logger.Debug("Less than 3 nodes available, skipping voting")
		return false
	}

	// Try existing quorum voting with short timeout
	h.logger.Debug("Starting quorum vote with timeout")
	if h.server != nil && h.server.GetQuorumManager() != nil {
		// Use existing voting but with timeout monitoring
		done := make(chan bool, 1)
		go func() {
			result := h.initiateNodeStatusVote(candidate.ID, StatusActive)
			done <- result
		}()

		// Wait for vote or timeout
		select {
		case result := <-done:
			if result {
				h.logger.Debug("Voting succeeded")
				return true
			}
			h.logger.Debug("Voting failed")
			return false
		case <-time.After(8 * time.Second):
			h.logger.Debug("Voting timed out")
			return false
		}
	}

	return false
}

// emergencyFallback handles the case where even coordinator fails
func (h *HealthChecker) emergencyFallback() {
	h.logger.Warn("Emergency fallback: checking if this node should coordinate")

	// Use the same deterministic coordination as main election
	localNodeID, err := h.members.config.GetLocalNodeUUID()
	if err != nil {
		h.logger.Error("Emergency fallback: Failed to get local node ID", "error", err)
		return
	}

	coordinatorID := h.findElectionCoordinator()
	if coordinatorID != localNodeID {
		h.logger.Info("Emergency fallback: Another node should coordinate", "coordinator", coordinatorID, "local",
			localNodeID)
		return
	}

	h.logger.Info("Emergency fallback: This node is coordinator, promoting best candidate")
	candidate := h.selectBestCandidate()
	if candidate != nil {
		candidate.Lock()
		candidate.Status = StatusActive
		candidate.Unlock()
		h.logger.Infof("Emergency fallback: Promoted %s to Active", candidate.Hostname)

		// Trigger IP refresh to bring up VIPs after emergency promotion
		if h.server != nil {
			h.logger.Info("HEALTH_CHECK: Triggering IP refresh after emergency fallback to bring up VIPs")
			h.server.RefreshLocalMonitorExpectedIPs()
		}
	} else {
		h.logger.Error("Emergency fallback failed: no candidates available")
	}
}

// findActiveNode returns the current active node
func (h *HealthChecker) findActiveNode() *Member {
	for _, member := range h.members.MembersSnapshot() {
		if member.Status == StatusActive {
			return member
		}
	}
	return nil
}

// checkClusterMembership makes a gRPC HealthCheck call to the remote node and validates
// that it shares the same cluster token. Returns false if the node is unreachable,
// rejects the token, or is running in a different cluster.
func (h *HealthChecker) checkClusterMembership(member *Member) bool {
	if member.IP == "" || member.Port == "" {
		return false
	}

	localToken := h.members.config.Pulse.ClusterToken
	localNodeID, err := h.members.config.GetLocalNodeUUID()
	if err != nil {
		h.logger.Warnf("checkClusterMembership: failed to get local node ID: %v", err)
		return false
	}

	remoteClient, err := client.New()
	if err != nil {
		h.logger.Warnf("checkClusterMembership: failed to create client: %v", err)
		return false
	}
	defer remoteClient.Close()

	if err := remoteClient.Connect(member.IP, member.Port, false); err != nil {
		h.logger.Warnf("checkClusterMembership: failed to connect to %s (%s:%s): %v",
			member.Hostname, member.IP, member.Port, err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := remoteClient.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{
		NodeId:       localNodeID,
		ClusterToken: localToken,
	})
	if err != nil {
		h.logger.Warnf("checkClusterMembership: gRPC call to %s failed: %v", member.Hostname, err)
		return false
	}

	if !resp.Success {
		h.logger.Warnf("checkClusterMembership: %s rejected membership check: %s", member.Hostname, resp.Message)
		return false
	}

	// Verify the remote node echoes a matching token (detects split-cluster scenarios)
	if localToken != "" && resp.ClusterToken != "" && resp.ClusterToken != localToken {
		h.logger.Warnf("checkClusterMembership: %s is in a different cluster (token mismatch)", member.Hostname)
		return false
	}

	return true
}

// checkNodeConnectivity verifies basic node connectivity
func (h *HealthChecker) checkNodeConnectivity(member *Member) bool {
	// Use member's stored IP and Port directly (no config lookup needed)
	if member.IP == "" || member.Port == "" {
		h.logger.Warnf("Node %s has empty IP (%s) or Port (%s)", member.Hostname, member.IP, member.Port)
		return false
	}

	// Try to establish basic connection
	address := fmt.Sprintf("%s:%s", utils.FormatIPv6(member.IP), member.Port)
	conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
	if err == nil {
		err = conn.Close()
		if err != nil {
			h.logger.Warnf("Warning: Failed to close connection for %s (%s): %v",
				member.Hostname, address, err)
		}
		return true
	}
	h.logger.Warnf("Health check failed for %s (%s): %v", member.Hostname, address, err)
	return false
}

// checkMemberIPs checks all IPs assigned to a member
func (h *HealthChecker) checkMemberIPs(member *Member) []string {
	var failedIPs []string

	// Create channels for concurrent health checks
	results := make(chan HealthCheck, len(member.ActiveIPs))
	var wg sync.WaitGroup

	// Check each IP concurrently
	for _, ip := range member.ActiveIPs {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			result := h.checkIP(ip)
			results <- result
		}(ip)
	}

	// Wait for all checks to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	for result := range results {
		if !result.Available {
			failedIPs = append(failedIPs, result.IP)
		}
	}

	return failedIPs
}

// checkIP performs health check on a single IP
func (h *HealthChecker) checkIP(ip string) HealthCheck {
	start := time.Now()
	h.logger.Debugf("Starting health check for IP: %s", ip)

	// Try to ping the IP with retries
	var lastErr error
	// Reduce retries and timeout for testing
	for i := 0; i < 1; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:80", ip), 500*time.Millisecond)
		if err == nil {
			err = conn.Close()
			if err != nil {
				h.logger.Warnf("Warning: Failed to close connection for %s: %v", ip, err)
			}
			latency := time.Since(start)
			h.logger.Debugf("Health check successful for IP %s (latency: %v)", ip, latency)
			return HealthCheck{
				IP:        ip,
				Available: true,
				Latency:   latency,
				Error:     nil,
			}
		}
		lastErr = err
		h.logger.Debugf("IP check attempt %d to %s failed: %v", i+1, ip, err)
		time.Sleep(100 * time.Millisecond)
	}

	h.logger.Warnf("Health check failed for IP %s: %v", ip, lastErr)
	return HealthCheck{
		IP:        ip,
		Available: false,
		Latency:   0,
		Error:     lastErr,
	}
}

// handlePartialFailure manages the redistribution of failed IPs
func (h *HealthChecker) handlePartialFailure(member *Member, failedIPs []string) {
	h.logger.Infof("Handling partial failure for member %s with %d failed IPs", member.Hostname, len(failedIPs))

	membersSnapshot := h.members.MembersSnapshot()

	// Determine if we should use quorum based on cluster size
	clusterSize := len(membersSnapshot)
	quorumEnabled := clusterSize >= 3

	// Update member status based on mode
	member.Lock()
	switch h.members.config.Pulse.Mode {
	case "active-passive":
		if len(failedIPs) == len(member.ActiveIPs) {
			// All IPs failed in active-passive mode - mark node as down
			h.logger.Warnf("All IPs failed for active node %s, marking as unknown", member.Hostname)

			// If quorum is enabled, we need to initiate a vote before changing status
			if quorumEnabled {
				h.logger.Info("Quorum voting is enabled, initiating vote for node status change")
				member.Unlock() // Unlock before initiating vote

				// Initiate vote through the server component
				voteResult := h.initiateNodeStatusVote(member.ID, StatusUnknown)

				if !voteResult {
					h.logger.Warn("Quorum vote failed, not changing node status")
					return
				}

				h.logger.Info("Quorum vote passed, proceeding with status change")
				member.Lock() // Lock again to continue with status change
			}

			member.Status = StatusUnknown

			// Find a passive node to promote
			for _, otherMember := range membersSnapshot {
				if otherMember.ID != member.ID && otherMember.Status == StatusPassive {
					h.logger.Infof("Promoting passive node %s to active", otherMember.Hostname)

					// If quorum is enabled, we need to initiate a vote before promoting
					if quorumEnabled {
						member.Unlock() // Unlock before initiating vote

						// Initiate vote for promotion
						voteResult := h.initiateNodeStatusVote(otherMember.ID, StatusActive)

						if !voteResult {
							h.logger.Warn("Quorum vote failed, not promoting node")
							return
						}

						h.logger.Info("Quorum vote passed, proceeding with promotion")
						member.Lock() // Lock again to continue
					}

					if h.server != nil {
						if err := h.server.OrchestrateIPFailover(member.ID, otherMember.ID,
							member.ActiveIPs); err != nil {
							h.logger.Errorf("Failed to promote passive node: %v", err)
						} else {
							h.logger.Infof("Passive node %s promoted to active", otherMember.Hostname)
							// Update member IP state
							otherMember.Lock()
							otherMember.ActiveIPs = append([]string{}, member.ActiveIPs...)
							otherMember.Unlock()

							member.Lock()
							member.ActiveIPs = nil
							member.Unlock()
						}
					}
					break
				}
			}
		}

	case "active-active":
		if len(failedIPs) == len(member.ActiveIPs) {
			// All IPs failed in active-active mode - mark as unknown
			h.logger.Warnf("All IPs failed for member %s, marking as unknown", member.Hostname)

			// If quorum is enabled, we need to initiate a vote before changing status
			if quorumEnabled {
				h.logger.Info("Quorum voting is enabled, initiating vote for node status change")
				member.Unlock() // Unlock before initiating vote

				// Initiate vote through the server component
				voteResult := h.initiateNodeStatusVote(member.ID, StatusUnknown)

				if !voteResult {
					h.logger.Warn("Quorum vote failed, not changing node status")
					return
				}

				h.logger.Info("Quorum vote passed, proceeding with status change")
				member.Lock() // Lock again to continue with status change
			}

			member.Status = StatusUnknown
		} else {
			// Partial failure in active-active — node keeps StatusActive with reduced IPs.
			// No status transition needed; update LoadFactor to reflect reduced load.
			h.logger.Infof("Partial IP failure for member %s, updating load factor", member.Hostname)
			if member.Capacity > 0 {
				member.LoadFactor = float64(len(member.ActiveIPs)-len(failedIPs)) / float64(member.Capacity)
			}
		}

	default:
		h.logger.Warnf("Unknown cluster mode %s, defaulting to active-passive behavior", h.members.config.Pulse.Mode)
		if len(failedIPs) == len(member.ActiveIPs) {
			// If quorum is enabled, we need to initiate a vote before changing status
			if quorumEnabled {
				h.logger.Info("Quorum voting is enabled, initiating vote for node status change")
				member.Unlock() // Unlock before initiating vote

				// Initiate vote through the server component
				voteResult := h.initiateNodeStatusVote(member.ID, StatusUnknown)

				if !voteResult {
					h.logger.Warn("Quorum vote failed, not changing node status")
					return
				}

				h.logger.Info("Quorum vote passed, proceeding with status change")
				member.Lock() // Lock again to continue with status change
			}

			member.Status = StatusUnknown
		}
	}

	// Remove failed IPs from member
	h.logger.Debugf("Removing failed IPs from member %s: %v", member.Hostname, failedIPs)
	member.RemoveIPs(failedIPs)
	member.Unlock()

	// If quorum is enabled, we need to initiate a vote before redistributing IPs
	if quorumEnabled {
		h.logger.Info("Quorum voting is enabled, initiating vote for IP redistribution")

		// Initiate vote for IP redistribution
		voteResult := h.initiateIPRedistributionVote(failedIPs)

		if !voteResult {
			h.logger.Warn("Quorum vote failed, not redistributing IPs")
			return
		}

		h.logger.Info("Quorum vote passed, proceeding with IP redistribution")
	}

	// Trigger IP redistribution
	h.logger.Info("Initiating IP redistribution for failed IPs")
	if err := h.members.RedistributeIPs(failedIPs); err != nil {
		h.logger.Errorf("Failed to redistribute IPs after partial failure: %v", err)
	} else {
		h.logger.Info("IP redistribution completed successfully")
	}
}

// initiateNodeStatusVote initiates a quorum vote for a node status change
// Returns true if the vote passes or if quorum voting is not applicable
func (h *HealthChecker) initiateNodeStatusVote(nodeID string, newStatus MemberStatus) bool {
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		h.logger.Infof("Initiating vote for node %s status change to %s (attempt %d/%d)", nodeID,
			statusToString(newStatus), attempt, maxRetries)

		// Check cluster size to determine if voting is needed
		// Count only available/responding nodes for quorum calculation
		availableNodes := 0
		membersSnapshot := h.members.MembersSnapshot()
		for _, member := range membersSnapshot {
			member.Lock()
			isAvailable := member.Status == StatusActive || member.Status == StatusPassive
			member.Unlock()
			if isAvailable {
				availableNodes++
			}
		}

		h.logger.Infof("Available nodes for voting: %d out of %d total", availableNodes, len(membersSnapshot))

		if availableNodes == 1 {
			h.logger.Infof("Only 1 node available, becoming active immediately")
			return true
		} else if availableNodes == 2 {
			// 2-node fallback: use deterministic ID-based tie-breaking
			h.logger.Infof("Exactly 2 nodes available, using deterministic tie-breaking")
			if newStatus == StatusActive {
				// Find the other available node
				localNodeID, err := h.members.config.GetLocalNodeUUID()
				if err != nil {
					h.logger.Error("Failed to get local node ID for tie-breaking", "error", err)
					return false
				}
				var otherNodeID string
				for _, member := range membersSnapshot {
					member.Lock()
					isAvailable := member.Status == StatusActive || member.Status == StatusPassive
					memberID := member.ID
					member.Unlock()
					if isAvailable && memberID != localNodeID {
						otherNodeID = memberID
						break
					}
				}
				if otherNodeID == "" {
					h.logger.Info("No other available node found, allowing Active promotion")
					return true
				}
				// Deterministic rule: smaller node ID wins
				shouldWin := localNodeID < otherNodeID
				h.logger.Infof("2-node tie-breaking: local=%s, other=%s, shouldWin=%v", localNodeID, otherNodeID,
					shouldWin)
				return shouldWin
			}
			return true // Allow non-Active status changes
		} else if availableNodes < 3 {
			h.logger.Debugf("Only %d nodes available, voting not required (need 3+ available)", availableNodes)
			return true
		}

		// Get the server instance from the context
		if h.server == nil {
			h.logger.Warn("Server reference not available, cannot initiate vote")
			return true // Default to allowing the change if we can't vote
		}

		// Get the quorum manager
		quorumManager := h.server.GetQuorumManager()
		if quorumManager == nil {
			h.logger.Warn("Quorum manager not available, cannot initiate vote")
			return true // Default to allowing the change if quorum manager is not available
		}

		// Update the quorum manager with the current count of available nodes
		quorumManager.UpdateNodeCount(availableNodes)

		// Get the node hostname for better logging
		var hostname string
		for _, member := range membersSnapshot {
			if member.ID == nodeID {
				hostname = member.Hostname
				break
			}
		}

		// Create a descriptive subject and description for the vote
		subject := nodeID
		description := fmt.Sprintf("Change node %s (%s) status to %s", hostname, nodeID, statusToString(newStatus))

		// Initiate the vote through the quorum manager
		sessionID, err := quorumManager.StartVotingSession(
			quorum.VoteTypeNodeStatus,
			subject,
			description,
			30*time.Second, // 30 second timeout for votes
		)

		if err != nil {
			h.logger.Errorf("Failed to start voting session: %v", err)
			if attempt < maxRetries {
				h.logger.Infof("Retrying in 2 seconds...")
				time.Sleep(2 * time.Second)
				continue
			}
			return true // Default to allowing the change if we can't start a vote
		}

		h.logger.Infof("Started voting session %s for node status change", sessionID)

		// Get our own node ID to cast our vote
		localNodeID, err := h.members.config.GetLocalNodeUUID()
		if err != nil {
			h.logger.Errorf("Failed to get local node ID: %v", err)
		} else {
			// Cast our own vote (we initiated it, so we vote yes)
			err = quorumManager.CastVote(sessionID, localNodeID, quorum.VoteDecisionYes)
			if err != nil {
				h.logger.Errorf("Failed to cast our own vote: %v", err)
			}
		}

		// Broadcast the vote request to other nodes so they can participate
		h.logger.Infof("Broadcasting vote request to cluster nodes...")
		if err := h.server.BroadcastVoteRequest(sessionID, "node_status", subject, description, 30); err != nil {
			h.logger.Warnf("Failed to broadcast vote request: %v", err)
			// Continue anyway - maybe some nodes are offline but others might still vote
		}

		// Wait for the vote to complete with shorter polling interval
		voteCompleted := false
		for i := 0; i < 30; i++ { // Poll for up to 30 seconds
			time.Sleep(1 * time.Second)

			session, err := quorumManager.GetVotingSession(sessionID)
			if err != nil {
				h.logger.Errorf("Failed to get voting session: %v", err)
				continue
			}

			// Check if the vote has completed
			if session.Result != nil {
				h.logger.Infof("Vote completed: passed=%v, quorum=%v, yes=%d, no=%d, total=%d",
					session.Result.Passed, session.Result.QuorumMet,
					session.Result.YesCount, session.Result.NoCount,
					session.Result.TotalVotes)

				voteCompleted = true
				if session.Result.Passed && session.Result.QuorumMet {
					return true // Vote passed
				}
				break // Vote failed or didn't meet quorum
			}

			// Early termination if we already have enough YES votes to guarantee passage
			yesCount := 0
			for _, vote := range session.Votes {
				if vote.Decision == quorum.VoteDecisionYes {
					yesCount++
				}
			}
			if quorumManager.HasQuorum(yesCount) {
				h.logger.Debugf("Early termination: enough YES votes received (%d)", yesCount)
				break
			}
		}

		if !voteCompleted {
			h.logger.Warnf("Vote timed out on attempt %d", attempt)
			if attempt < maxRetries {
				h.logger.Infof("Retrying vote in 3 seconds...")
				time.Sleep(3 * time.Second)
				continue
			}
		} else {
			// Vote completed but failed, retry if possible
			if attempt < maxRetries {
				h.logger.Infof("Vote failed, retrying in 5 seconds...")
				time.Sleep(5 * time.Second)
				continue
			}
		}
		break
	}

	h.logger.Error("All vote attempts failed after %d retries, aborting election to prevent split-brain", maxRetries)
	h.logger.Error("Manual intervention required - check network connectivity, node health, or use 'pulsectl promote' to force promotion after investigation")
	return false // Block promotion to prevent split-brain scenarios
}

// initiateIPRedistributionVote initiates a quorum vote for IP redistribution
// Returns true if the vote passes or if quorum voting is not applicable
func (h *HealthChecker) initiateIPRedistributionVote(ips []string) bool {
	h.logger.Infof("Initiating vote for redistribution of %d IPs", len(ips))

	// Check cluster size to determine if voting is needed
	clusterSize := len(h.members.MembersSnapshot())
	if clusterSize < 3 {
		h.logger.Debugf("Cluster has only %d nodes, voting not required for IP redistribution", clusterSize)
		return true
	}

	// Get the server instance from the context
	if h.server == nil {
		h.logger.Warn("Server reference not available, cannot initiate vote")
		return true // Default to allowing the change if we can't vote
	}

	// Get the quorum manager
	quorumManager := h.server.GetQuorumManager()
	if quorumManager == nil {
		h.logger.Warn("Quorum manager not available, cannot initiate vote")
		return true // Default to allowing the change if quorum manager is not available
	}

	// Create a descriptive subject and description for the vote
	ipList := ""
	if len(ips) <= 5 {
		ipList = fmt.Sprintf("%v", ips)
	} else {
		ipList = fmt.Sprintf("%v and %d more", ips[:5], len(ips)-5)
	}

	subject := fmt.Sprintf("redistribute-%d-ips", len(ips))
	description := fmt.Sprintf("Redistribute %d IPs: %s", len(ips), ipList)

	// Initiate the vote through the quorum manager
	sessionID, err := quorumManager.StartVotingSession(
		quorum.VoteTypeIPRedistribution,
		subject,
		description,
		30*time.Second, // 30 second timeout for votes
	)

	if err != nil {
		h.logger.Errorf("Failed to start IP redistribution voting session: %v", err)
		return false // Block redistribution if we can't establish proper voting
	}

	h.logger.Infof("Started voting session %s for IP redistribution", sessionID)

	// Get our own node ID to cast our vote
	localNodeID, err := h.members.config.GetLocalNodeUUID()
	if err != nil {
		h.logger.Errorf("Failed to get local node ID: %v", err)
	} else {
		// Cast our own vote (we initiated it, so we vote yes)
		err = quorumManager.CastVote(sessionID, localNodeID, quorum.VoteDecisionYes)
		if err != nil {
			h.logger.Errorf("Failed to cast our own vote: %v", err)
		}
	}

	// Wait for the vote to complete
	// In a production implementation, this would be asynchronous with callbacks
	// For simplicity, we'll use a polling approach here
	for i := 0; i < 30; i++ { // Poll for up to 30 seconds
		time.Sleep(1 * time.Second)

		session, err := quorumManager.GetVotingSession(sessionID)
		if err != nil {
			h.logger.Errorf("Failed to get voting session: %v", err)
			continue
		}

		// Check if the vote has completed
		if session.Result != nil {
			h.logger.Infof("Vote completed: passed=%v, quorum=%v, yes=%d, no=%d, total=%d",
				session.Result.Passed, session.Result.QuorumMet,
				session.Result.YesCount, session.Result.NoCount,
				session.Result.TotalVotes)

			return session.Result.Passed
		}
	}

	h.logger.Warn("IP redistribution vote timed out, blocking redistribution to maintain consistency")
	return false // Block redistribution if voting fails to maintain cluster consistency
}

// calculateElectionBackoff returns a deterministic delay to prevent simultaneous elections
// Lower node IDs (lexicographically) get shorter delays to ensure election ordering
// calculateElectionBackoffWithRole returns delay and whether this node is the election coordinator
func (h *HealthChecker) calculateElectionBackoffWithRole(localNodeID string) (time.Duration, bool) {
	h.logger.Debug("BACKOFF: Calculating election backoff delay and coordinator role")

	// Get list of all available nodes that could participate in election
	var availableNodes []string
	for _, member := range h.members.MembersSnapshot() {
		member.Lock()
		status := member.Status
		nodeID := member.ID
		member.Unlock()

		// Only consider nodes that could potentially become active and are reachable
		if status == StatusPassive {
			availableNodes = append(availableNodes, nodeID)
		}
	}

	if len(availableNodes) <= 1 {
		h.logger.Debug("BACKOFF: Only one available node, this node is coordinator")
		return 0, true
	}

	// Sort node IDs to ensure deterministic ordering
	sort.Strings(availableNodes)

	// Find our position in the sorted list
	position := -1
	for i, nodeID := range availableNodes {
		if nodeID == localNodeID {
			position = i
			break
		}
	}

	if position == -1 {
		h.logger.Warn("BACKOFF: Local node not found in available nodes list, using fallback")
		return 10 * time.Second, false
	}

	// Position 0 is the coordinator
	isCoordinator := (position == 0)

	// Calculate delay:
	// - Coordinator: 0s delay (proceeds immediately after quick stability check)
	// - Non-coordinators: position * 4 seconds (to give coordinator time to complete)
	var delay time.Duration
	if isCoordinator {
		delay = 0
	} else {
		delay = time.Duration(position) * 4 * time.Second
	}

	h.logger.Infof("BACKOFF: Node %s is position %d of %d available nodes, delay: %v, coordinator: %v",
		localNodeID, position+1, len(availableNodes), delay, isCoordinator)

	return delay, isCoordinator
}

// Helper function to convert MemberStatus to string
func statusToString(status MemberStatus) string {
	return StatusToString(status)
}

func (h *HealthChecker) tryForcePromote(candidate *Member) bool {
	if candidate == nil {
		return false
	}
	server := h.server
	if server == nil {
		h.logger.Debug("ELECTION: Server reference unavailable, skipping Promote RPC")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	resp, err := server.Promote(ctx, &rpc.PromoteRequest{
		NodeId:      candidate.ID,
		ForceDemote: true,
	})
	if err != nil {
		h.logger.Warn("ELECTION: Promote RPC failed", "candidate", candidate.Hostname, "error", err)
		return false
	}
	if resp == nil || !resp.Success {
		message := "unknown"
		if resp != nil {
			message = resp.Message
		}
		h.logger.Warn("ELECTION: Promote RPC returned failure", "candidate", candidate.Hostname, "message", message)
		return false
	}

	h.logger.Info("ELECTION: Promote RPC succeeded", "candidate", candidate.Hostname)
	server.RefreshLocalMonitorExpectedIPs()
	return true
}
