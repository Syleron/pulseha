package membership

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/ipam"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
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
	// Demotion orchestration; releases every group IP the node could host
	MakePassive(ctx context.Context, req *rpc.MakePassiveRequest) (*rpc.MakePassiveResponse, error)
	// Re-announcement of the addresses a node is configured to hold, for when the
	// segment's idea of who owns them has gone stale without any address moving
	AnnounceNodeIPs(nodeID string) error
	// Re-broadcast of the current config, for the periodic reconcile below
	RequestConfigReconcile()
}

// reconcileConfigAcrossPeers re-broadcasts the coordinator's config once a
// minute on an otherwise stable cluster, so a peer that missed a config
// broadcast is repaired instead of staying diverged forever.
//
// This is the "it never self-heals" half of docs/TEST-PLAN.md defect #5. A
// mutation's broadcast was fire-and-forget with the RPC error discarded, so a
// single dropped ConfigSync diverged a node permanently: on whitecrane 200 serial
// add-ip calls left node-3 holding precisely the last four it had missed, and it
// stayed that way. The broadcaster now retries, but retries are bounded and a
// peer can be down for longer than they last.
//
// Coordinator-gated, and that gate is the load-bearing part. The receiver applies
// a config wholesale, so if every node re-broadcast its own view, a node that was
// *behind* would push its stale config at a generation its peers had not seen
// from it and they would adopt it — turning the repair into the corruption. One
// speaker per cluster is what makes a re-broadcast safe. clusterCoordinator waits
// out FailOverLimit before appointing anyone, so a node doing bulk IP work does
// not get a second coordinator appointed beside it (the runs 8-14 fix).
func (h *HealthChecker) reconcileConfigAcrossPeers(members map[string]*Member) {
	if h.server == nil || h.members == nil {
		return
	}
	cfg := h.members.Config()
	if cfg == nil {
		return
	}
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return
	}
	if clusterCoordinator(members, h.failoverGrace()) != localID {
		return
	}
	h.logger.Debug("CONFIG_RECONCILE: re-broadcasting config from the coordinator")
	h.server.RequestConfigReconcile()
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
	members     *MemberList
	checkTicker *time.Ticker
	stopChan    chan struct{}
	stopOnce    sync.Once // Ensure we only close stopChan once
	logger      *log.Logger
	// lastPeerStatuses is the previous pass's view of every OTHER member, so a status
	// change can be detected regardless of which node originated it. Only read and
	// written from the health-check goroutine, under the same lock as the pass.
	lastPeerStatuses map[string]MemberStatus

	// ready and stopped are atomic rather than plain fields under the health
	// checker's own RWMutex, because IsRunning is read from a caller that already
	// holds the *server's* write lock — Server.Start's startHealthChecker probe —
	// while performHealthChecks holds this lock for its whole body and calls back
	// into the server for the epoch and the state broadcast. Those two orders are
	// opposite, so the probe and a health-check pass deadlocked whenever the first
	// tick landed before Server.Start returned. Reading these without the lock
	// removes the probe's edge; see #79 for the crossings that remain, and #56 for
	// the same non-reentrancy trap one lock in.
	//
	// Nothing needs the pair to be consistent with each other: IsRunning is
	// advisory, and every state change still happens under the lock.
	ready                 atomic.Bool
	stopped               atomic.Bool // Track if we're stopped
	server                ServerReference
	lastClusterState      string          // Track last cluster state to only log changes
	checksWithoutChange   int             // Counter for periodic status logs
	lastLeaderBroadcast   time.Time       // suppress elections briefly after leader broadcast
	lastTick              time.Time       // last time a check cycle executed
	loggedNoMembers       bool            // Tracks if a no-member condition has already been logged in the current state
	deepCheckCounter      int             // incremented each cycle; triggers cluster-membership gRPC check every 5 cycles
	membershipCheckFailed map[string]bool // nodes the last deep check REJECTED; cleared only when a later deep check confirms

	// membershipUnresolved is nodes whose last deep check could not be
	// completed. It does two things: keeps the node unreachable until a deep
	// check actually concludes, and forces that deep check on the very next
	// cycle instead of waiting for the fifth.
	//
	// Both halves matter, and getting only one of them was a live regression.
	// Latching an unresolved check like a rejection costs four extra cycles of
	// Unknown after a transient deadline -- the lb_api CI flake. But NOT
	// carrying it at all is worse: the four cheap cycles that follow answer on
	// a TCP dial alone, so a frozen-but-listening peer read healthy four cycles
	// in five. Measured on the docker rig: a peer frozen for 14s was reported
	// Active for 6.2s of it. A listening socket outlives a wedged daemon (#56,
	// #79), which is the failure this daemon is most prone to.
	//
	// Re-checking next cycle resolves both: a transient recovers in about one
	// cycle, and a genuinely wedged peer is deep-checked every cycle and stays
	// Unknown for as long as it stays wedged. The cost is one extra gRPC call
	// per cycle per suspect node, and only while it is suspect.
	membershipUnresolved map[string]bool

	// deepCheck is checkClusterMembership, indirected so a test can drive each
	// verdict without a peer to answer it. Same reason IPMonitor indirects
	// enforce and now: the real thing needs a network, and what needs testing
	// here is what the caller does with the answer rather than how it is
	// obtained. nil in the daemon, which uses the method.
	deepCheck       func(*Member) membershipVerdict
	reconcileCycles int // cycles since startup; reconciliation waits for a grace period

	// reconcileInFlight guards the reconciliation pass, which runs off the tick.
	// While a pass is running later ticks skip it rather than queue behind it, so
	// a slow peer cannot accumulate goroutines.
	reconcileInFlight atomic.Bool

	// ipPresence reports whether an address is up on a local interface. Overridden
	// in tests; nil means read the real interfaces.
	ipPresence ipPresenceFunc
}

// ipPresenceFunc reports whether an address is currently on a local interface.
type ipPresenceFunc func(string) (bool, string, error)

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
		membershipUnresolved:  make(map[string]bool),
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

	if h.stopped.Load() {
		h.logger.Debug("Health checker is stopped, reinitializing...")
		h.stopChan = make(chan struct{})
		h.stopped.Store(false)
	}
	h.checkTicker = time.NewTicker(interval)
	h.ready.Store(true)

	// Add initial delay before starting health checks
	h.logger.Debug("Adding initial delay before starting health checks...")
	time.Sleep(500 * time.Millisecond)

	go h.run()
	h.logger.Debug("Health checker is now running")
}

// IsRunning returns true if the health checker is currently running.
//
// Deliberately takes no lock. Server.Start probes this from inside the server's
// write lock, while a health-check pass holds this lock and waits on that same
// server lock — see the note on the ready/stopped fields. Both reads are atomic,
// and the answer is advisory in every caller: it decides whether to call Start,
// which re-checks under the lock.
func (h *HealthChecker) IsRunning() bool {
	return h.ready.Load() && !h.stopped.Load()
}

// Stop halts the health checking process
func (h *HealthChecker) Stop() {
	h.Lock()
	defer h.Unlock()

	h.logger.Debug("Stopping health checker...")

	// Set flags first to prevent new checks from starting
	h.ready.Store(false)
	h.stopped.Store(true)

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
			if !h.ready.Load() || h.stopped.Load() || h.checkTicker == nil {
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
				if !h.ready.Load() || h.stopped.Load() {
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

	// One config snapshot for the whole pass, taken here because no member lock is
	// held yet.
	//
	// The per-member loop below reads this while holding member.Lock() (taken at
	// the reachability check), and MemberList.Config() takes the member-list read
	// lock — so that read inverted against UpdateConfig, which holds the member-list
	// *write* lock and then reaches for a member lock. Two parties, opposite orders,
	// and it deadlocked the daemon: CI caught twelve goroutines stacked behind the
	// member list, including Server.Stop, Promote, ConfigSync and the IP monitor.
	//
	// #71 introduced it and named the right remedy in its own write-up —
	// "snapshotted once per pass" — but left this call site reading per member.
	// Before #71 it was a bare pointer read: racy, and precisely because it took no
	// lock, it could not deadlock. Replacing an unsynchronised read with a lock
	// acquisition inside another lock is the trade that has to be checked, and this
	// is the site where it was not.
	passCfg := h.members.Config()
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
		// A node the deep check *rejects* — it does not know us, or it echoes a
		// different cluster token — stays unreachable until a later deep check
		// confirms it, because a passing TCP dial says nothing about which cluster
		// answered. A node whose deep check could not be *completed* also stays
		// unreachable, but is re-checked on the next cycle rather than the fifth;
		// see membershipUnresolved for why that distinction is not the same one.
		startTime := time.Now()
		var isReachable bool
		// The scheduled deep check, or one forced because this member's last
		// deep check could not be completed. Per member, not per pass: a single
		// slow peer must not drag every other node onto the expensive path.
		if doDeepCheck || h.membershipUnresolved[member.ID] {
			h.logger.Debugf("About to deep-check cluster membership for %s (IP:%s Port:%s)", member.Hostname, member.IP, member.Port)
			verdict := h.membershipOf(member)

			// Only a confirmed membership counts as reachable. Neither a
			// rejection nor an unresolved check falls back to the TCP dial: a
			// listening socket outlives a wedged daemon (#56, #79), so
			// answering "reachable" on the strength of TCP alone would hide
			// exactly the failure this daemon is most prone to.
			isReachable = verdict == membershipConfirmed

			switch verdict {
			case membershipConfirmed:
				delete(h.membershipCheckFailed, member.ID)
				delete(h.membershipUnresolved, member.ID)
			case membershipRejected:
				// A conclusion about the peer's cluster. Latches until a later
				// deep check confirms it, because a passing TCP dial says
				// nothing about which cluster answered.
				h.membershipCheckFailed[member.ID] = true
				delete(h.membershipUnresolved, member.ID)
			default:
				// Not a conclusion at all. Do not manufacture a rejection out
				// of a timeout, and do not clear a real one merely because the
				// peer has since stopped answering -- but do keep this member
				// on the deep path until something is concluded.
				h.membershipUnresolved[member.ID] = true
			}
		} else {
			h.logger.Debugf("About to check connectivity for %s (IP:%s Port:%s)", member.Hostname, member.IP, member.Port)
			isReachable = h.checkNodeConnectivity(member)
			// A node the last deep check placed in a different cluster stays
			// unreachable however well it answers a TCP dial.
			if isReachable && h.membershipCheckFailed[member.ID] {
				h.logger.Debugf("Node %s passes TCP but the last deep check rejected its membership; treating as unreachable", member.Hostname)
				isReachable = false
			}
		}
		responseTime := time.Since(startTime)
		h.logger.Debugf("Connectivity check result for %s: reachable=%v, responseTime=%v", member.Hostname, isReachable,
			responseTime)

		member.Lock()
		// Stamped only when the member actually answered. It used to be stamped
		// here unconditionally, immediately before the branch below decided the
		// member was unreachable — so the field recorded whether a member had
		// ever responded, not when it last did, and every consumer measuring
		// silence with it was reading a constant.
		if isReachable {
			member.LastHCResponse = time.Now()
		}

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

		// Handle auto-failback for previously failed nodes.
		//
		// The pass snapshot, not a fresh Config(): a member lock is held here, and
		// taking the member-list lock under it is what deadlocked against
		// UpdateConfig. See passCfg above.
		hcCfg := passCfg
		if hcCfg == nil {
			continue
		}
		autoFailback := hcCfg.Pulse.AutoFailback
		mode := hcCfg.Pulse.Mode

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
			member.Status = recoveredStatus(mode)
			statusChanges = append(statusChanges, fmt.Sprintf("%s recovered to %s",
				member.Hostname, StatusToString(member.Status)))
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
		h.resetChecksWithoutChangeLocked()

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
		checksWithoutChange := h.incChecksWithoutChangeLocked()

		// Heartbeat convergence nudge every 3 checks (~3s) to re-assert an
		// already-agreed view and keep peers aligned.
		//
		// It used to advance every member's LastResponse here as well, "for
		// consistent display" — including members it had just failed to reach.
		// That is what made LastResponse unable to measure silence, and the
		// alignment this nudge exists for does not depend on it: the broadcast
		// carries member statuses, the epoch and the leader, and no timestamp.
		if h.server != nil && checksWithoutChange%3 == 0 {
			h.logger.Debug("HEALTH_CHECK: Performing heartbeat convergence nudge", "checksWithoutChange",
				checksWithoutChange)
			states := getMemberStatusMap()
			for id, m := range membersSnapshot {
				m.Lock()
				states[id] = m.Status
				m.Unlock()
			}
			// Deliberately the current epoch, not epoch+1. This fires on *unchanged*
			// state, so it carries no decision — it exists to advance LastResponse and
			// re-assert an already-agreed view. Claiming a new epoch made every node's
			// three-second keepalive outrank the last real decision, so a peer holding a
			// stale view could undo a coordinator assignment simply by being the most
			// recent to speak. In active-active that became an endless loop: the
			// coordinator assigned IPs to a node, the node went Active, a peer's next
			// nudge demoted it at a higher epoch, its IPs were stripped, and the
			// coordinator assigned them again ~3s later (docs/TEST-PLAN.md defect #2).
			_ = h.server.BroadcastClusterState(states, h.server.GetClusterEpoch(), h.getCurrentLeaderID(), nil)
			putMemberStatusMap(states)
			h.logger.Debug("HEALTH_CHECK: Heartbeat convergence broadcast completed")
		}

		// Log periodic summary every 60 checks (roughly every minute with 1s interval).
		// Counts up rather than resetting: reconcileActiveActive reads this as the
		// number of consecutive unchanged cycles, and zeroing it here would stand
		// the coordinator down once a minute on a perfectly stable cluster.
		if checksWithoutChange%60 == 0 {
			h.logger.Infof("Cluster stable for %d checks: %s", checksWithoutChange, currentClusterDisplayState)
			h.reconcileConfigAcrossPeers(membersSnapshot)
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
		// Always check for active node failure, not just when passive. Runs off
		// the tick — see startReconcilePass for why.
		h.startReconcilePassLocked()
	} else {
		// Debug why no local member found - this indicates a serious configuration issue
		diagCfg := h.members.Config()
		var localNodeID string
		var err error
		if diagCfg != nil {
			localNodeID, err = diagCfg.GetLocalNodeUUID()
		}
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
		if diagCfg != nil {
			h.logger.Info("HEALTH_CHECK: MemberList config state",
				"local_node_id", localNodeID,
				"cluster_check", diagCfg.ClusterCheck(),
				"node_count_in_config", len(diagCfg.Nodes))
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

// reconcileBudget bounds one reconciliation pass. It exists so a pass that is
// making no progress — a peer that accepts connections and never answers — cannot
// hold the single-flight guard and stop reconciliation indefinitely.
//
// Comfortably above demoteMaxTimeout plus the quorum vote's 30s poll, so the
// backstop only fires for work that is genuinely stuck rather than for a large
// group being released legitimately. It bounds reconciliation latency, not
// health-check responsiveness — the pass no longer runs on the tick.
const reconcileBudget = 3 * time.Minute

// nextReconcileCycle advances and returns the cycle counter, and reports the
// consecutive-unchanged-cycles count alongside it.
//
// Both counters are written by the tick and read by the reconciliation pass, which
// now runs on its own goroutine, so they move under the health checker lock.
func (h *HealthChecker) nextReconcileCycle() (cycles, stable int) {
	h.Lock()
	defer h.Unlock()
	h.reconcileCycles++
	return h.reconcileCycles, h.checksWithoutChange
}

// reconcileCounters reads the two counters the reconciliation thresholds consult.
func (h *HealthChecker) reconcileCounters() (cycles, stable int) {
	h.RLock()
	defer h.RUnlock()
	return h.reconcileCycles, h.checksWithoutChange
}

// resetChecksWithoutChangeLocked zeroes the consecutive-unchanged-cycles counter.
//
// The caller holds h's write lock. performHealthChecks takes it for its whole
// body, so a helper that took it again wedged the health-check goroutine *with
// the lock held* — no ticks, no promotion, no placement (docs/TEST-PLAN.md
// defect #56).
func (h *HealthChecker) resetChecksWithoutChangeLocked() {
	h.checksWithoutChange = 0
}

// incChecksWithoutChangeLocked advances the consecutive-unchanged-cycles counter
// and returns its new value, so the tick reads one consistent number for the rest
// of the cycle rather than re-reading a field the reconciliation pass also
// consults. The caller holds h's write lock — see
// resetChecksWithoutChangeLocked.
func (h *HealthChecker) incChecksWithoutChangeLocked() int {
	h.checksWithoutChange++
	return h.checksWithoutChange
}

// startReconcilePass runs cluster reconciliation off the health-check tick.
//
// Everything the pass reaches can block for tens of seconds: a serial MakePassive
// per extra Active, a quorum vote that polls for 30s, a remote BringDownIPs per
// duplicate address, and the bring-ups redistribution performs. Running that
// inline meant a 1s tick could take a minute, so this node stopped answering its
// own health checks, peers marked it Unknown and elected around it — the same
// "busy node looks dead" failure the batched GARP and the coordinator grace period
// were added to prevent (docs/TEST-PLAN.md defects #2/#26).
//
// At most one pass runs at a time. A tick arriving while one is in flight skips,
// which is why the pass reads the counters live rather than a snapshot: it should
// act on the cluster as it is when it gets there, not as it was several ticks ago.
// The caller holds h's write lock: this is called from performHealthChecks, which
// takes it for its whole body. Calling the exported IsRunning here took the read
// lock on top of that write lock, and sync.RWMutex is not reentrant, so the first
// tick to reach this line wedged the health-check goroutine *with the write lock
// held*. On whitecrane run 24 that stopped reconciliation dead: no node was ever
// promoted, nothing placed the 287-address group, and all four nodes sat
// Standby/Passive with the whole group down (docs/TEST-PLAN.md defect #56).
func (h *HealthChecker) startReconcilePassLocked() {
	if !h.ready.Load() || h.stopped.Load() {
		return
	}
	if !h.reconcileInFlight.CompareAndSwap(false, true) {
		h.logger.Debug("ACTIVE_CHECK: reconciliation still running, skipping this cycle")
		return
	}

	go func() {
		defer h.reconcileInFlight.Store(false)

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.checkForActiveNodeFailure()
		}()

		select {
		case <-done:
		case <-time.After(reconcileBudget):
			// The pass is abandoned, not cancelled — the operations below it are
			// bounded individually, so it will finish on its own. Releasing the
			// guard here keeps a wedged peer from stopping reconciliation for good.
			h.logger.Warn("ACTIVE_CHECK: reconciliation exceeded its budget, releasing it for the next cycle",
				"budget", reconcileBudget)
		}
	}()
}

// checkForActiveNodeFailure checks if the active node has failed and initiates failover
func (h *HealthChecker) checkForActiveNodeFailure() {
	h.logger.Debug("ACTIVE_CHECK: Starting active node failure check")

	h.nextReconcileCycle()

	members := h.members.MembersSnapshot()
	config := h.members.Config()

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

	// Active-passive tolerates exactly one Active node. If more than one is
	// Active the scan above picked one of them arbitrarily, so consolidate now
	// and re-evaluate failover next cycle against a single-Active view.
	if h.enforceSingleActive(members) {
		return
	}

	h.announceOnPeerDemotion(members)

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

// reconcileGraceCycles is the number of health-check cycles to wait after
// startup before making redistribution or consolidation decisions. It gives
// initial health checks and peer self-reports (which carry each node's
// ActiveIPs) time to converge so we don't redistribute IPs that a peer is
// actually still hosting, or demote a node whose reported status is still
// the stale one we started up with.
const reconcileGraceCycles = 10

// Demotion deadline sizing.
//
// MakePassive drops every configured floating address on the target and then
// verifies each one against its interfaces, so its cost scales with the size of
// the group, not with the cluster. A flat 10s was right when demotion only
// released the addresses recorded against a node; now, on the 201-address test
// topology, a healthy but loaded incumbent can legitimately need longer.
//
// Overrunning is not a neutral outcome. A DeadlineExceeded is deliberately read as
// "the peer is alive and may still own its IPs" — the conservative reading that
// keeps a wedged Active from being promoted over — so a deadline that is merely
// too short aborts promotions and consolidations that were safe.
const (
	demoteBaseTimeout  = 10 * time.Second
	demotePerIPTimeout = 100 * time.Millisecond
	demoteMaxTimeout   = 120 * time.Second
)

// DemotionTimeoutFor sizes a MakePassive deadline to the number of floating IPs
// the target has to release and verify. Capped, so a huge or misconfigured group
// cannot produce an effectively unbounded wait.
func DemotionTimeoutFor(ipCount int) time.Duration {
	if ipCount < 0 {
		ipCount = 0
	}
	timeout := demoteBaseTimeout + time.Duration(ipCount)*demotePerIPTimeout
	if timeout > demoteMaxTimeout {
		return demoteMaxTimeout
	}
	return timeout
}

// localNodeID reads this node's UUID through the config-pointer accessor.
//
// Config() is documented as possibly returning nil, and GetLocalNodeUUID calls
// ClusterCheck, which dereferences c.Nodes — so chaining the two panics on a nil
// config. Every other reader in this file takes the pointer into a local and
// guards it; three vote/tie-break sites chained instead, which read as an
// oversight rather than a considered exception. Folding the guard into one
// accessor makes it impossible to forget at a fourth site.
func (h *HealthChecker) localNodeID() (string, error) {
	cfg := h.members.Config()
	if cfg == nil {
		return "", errors.New("member list has no config")
	}
	return cfg.GetLocalNodeUUID()
}

// makePassiveTimeout bounds a demotion issued by the consolidation invariant,
// sized to the group it makes the target release.
func (h *HealthChecker) makePassiveTimeout() time.Duration {
	cfg := h.members.Config()
	if cfg == nil {
		return DemotionTimeoutFor(0)
	}
	total := 0
	for _, ips := range cfg.Groups {
		total += len(ips)
	}
	return DemotionTimeoutFor(total)
}

// enforceSingleActive keeps the active-passive invariant that at most one node
// is Active. Consolidating only when the mode changes cannot hold it: anything
// that promotes a node afterwards — a late BringUpIP reaching a peer that still
// believes the cluster is active-active, an election racing the switch — leaves
// two Actives, and in active-passive every Active expects *all* the group IPs
// on its interfaces, so the two ARP-fight over every floating IP. Checking each
// cycle means such a promotion is undone on the next one.
//
// Only the coordinator acts, so peers can't issue conflicting demotions, and
// the survivor is ConsolidationTarget — the same choice SetMode makes — so the
// decision is stable across cycles and nodes. Demotion goes through the
// MakePassive RPC because that releases every group IP the node could host
// rather than only the ones recorded against it: a node promoted by election
// holds them all while its ActiveIPs is still empty.
//
// Returns true if any node was demoted.
// announceOnPeerDemotion re-announces this node's floating IPs when a peer stops being able to
// hold them, so the segment stops pointing at a node that has just let them go.
//
// A gratuitous ARP is only ever sent by a bring-up and there is no periodic re-announce, so a
// node that has held an address continuously never announces it again. After a two-node split
// both nodes held the group and the one that promoted second announced last; when it is then
// demoted it drops those addresses without announcing anything — bring-down never does — and
// every ARP cache is left pointing at a node that no longer answers. The address is present on
// the survivor and reachable by nobody until the caches age out. See
// docs/adr/0002-two-node-availability-over-safety.md and docs/TEST-PLAN.md #80.
//
// Detected here, on the health-check pass, rather than at either end of the state broadcast.
// Both were tried and both are half of the answer: the ConfigSync receive hook fires only on a
// node that is TOLD of the demotion, and which node that is depends on node-ID ordering
// deciding who survives consolidation — measured across three runs, the surviving Active was
// the receiver once and the originator twice, so one announcement was made in three heals. The
// send side cannot diff reliably either, because callers apply the state to the member list
// before broadcasting it. This pass sees the settled view every tick whoever produced it, which
// is the property the other two placements lack.
//
// Ordering against the peer's release does not matter, which is what makes a tick-based
// detector sufficient: a bring-down never announces, so any announcement made after the peer's
// last bring-up is the last word on the segment regardless of when the release lands.
//
// Edge-triggered against the previous pass, and the server debounces on top of that. The
// trigger has to accept a peer arriving from Unknown, because that is what a healed partition
// produces — this node lost sight of the peer during the split, so its last known status was
// never Active — and that is also what a merely slow peer produces (defects #2/#26), which is
// why the debounce exists rather than a tighter condition here.
func (h *HealthChecker) announceOnPeerDemotion(members map[string]*Member) {
	if h.server == nil {
		return
	}
	cfg := h.members.Config()
	if cfg == nil {
		return
	}
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return
	}

	previous := h.lastPeerStatuses
	current := make(map[string]MemberStatus, len(members))

	localIsActive := false
	demoted := make([]string, 0, 1)

	for id, member := range members {
		member.Lock()
		status := member.Status
		member.Unlock()

		if id == localID {
			localIsActive = status == StatusActive
			continue
		}
		current[id] = status

		if status != StatusPassive && status != StatusMaintenance {
			continue
		}
		// A peer present in the previous view under a different status is the edge. A peer
		// not in it at all is this node's first pass, where there is no transition to react
		// to and announcing would fire on every daemon start.
		if prev, seen := previous[id]; seen && prev != status {
			demoted = append(demoted, id)
		}
	}

	h.lastPeerStatuses = current

	if len(demoted) == 0 || !localIsActive {
		return
	}

	h.logger.Info("ACTIVE_CHECK: peer can no longer hold floating IPs, re-announcing",
		"demoted_peers", demoted)
	if err := h.server.AnnounceNodeIPs(localID); err != nil {
		// The addresses are up here either way; a failed announcement leaves the stale ARP
		// entries rather than creating a new fault, so the pass continues.
		h.logger.Error("ACTIVE_CHECK: failed to re-announce floating IPs after a peer was demoted",
			"error", err)
	}
}

func (h *HealthChecker) enforceSingleActive(members map[string]*Member) bool {
	if cycles, _ := h.reconcileCounters(); cycles < reconcileGraceCycles || h.server == nil {
		return false
	}

	var actives []*Member
	for _, member := range members {
		member.Lock()
		isActive := member.Status == StatusActive
		member.Unlock()
		if isActive {
			actives = append(actives, member)
		}
	}
	if len(actives) < 2 {
		return false
	}

	cfg := h.members.Config()
	if cfg == nil {
		return false
	}
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return false
	}
	if clusterCoordinator(members, h.failoverGrace()) != localID {
		h.logger.Warnf("ACTIVE_CHECK: %d nodes are Active in active-passive mode; waiting for the coordinator to consolidate",
			len(actives))
		return false
	}

	target := ConsolidationTarget(members, h.server.GetLeaderID())
	if target == nil {
		h.logger.Error("ACTIVE_CHECK: no eligible node to consolidate floating IPs onto")
		return false
	}

	h.logger.Warnf("ACTIVE_CHECK: %d nodes are Active in active-passive mode, consolidating onto %s",
		len(actives), target.Hostname)

	// The demotions are issued serially, so the cost is makePassiveTimeout per
	// extra Active. A shared deadline caps the pass regardless of how many there
	// are; whatever is left over is picked up next cycle, since the invariant is
	// re-checked every time.
	deadline := time.Now().Add(reconcileBudget / 2)

	demoted := false
	for _, member := range actives {
		if member.ID == target.ID {
			continue
		}
		if time.Now().After(deadline) {
			h.logger.Warn("ACTIVE_CHECK: consolidation budget spent, remaining Actives will be demoted next cycle",
				"remaining_after", member.Hostname)
			break
		}

		ctx, cancel := context.WithTimeout(context.Background(), h.makePassiveTimeout())
		resp, err := h.server.MakePassive(ctx, &rpc.MakePassiveRequest{NodeId: member.ID})
		cancel()

		switch {
		case err != nil:
			h.logger.Error("ACTIVE_CHECK: failed to demote extra Active node",
				"hostname", member.Hostname, "error", err)
		case !resp.Success:
			h.logger.Error("ACTIVE_CHECK: demotion of extra Active node was rejected",
				"hostname", member.Hostname, "message", resp.Message)
		default:
			h.logger.Warn("ACTIVE_CHECK: demoted extra Active node", "hostname", member.Hostname)
			demoted = true
		}
	}

	if !demoted {
		return false
	}

	// Make the target announce what it already holds. Nothing moved onto it — it was
	// Active throughout — so no bring-up fires and, without this, it never announces.
	// Meanwhile the node just demoted has dropped addresses the segment has learned
	// from it, because its bring-up announced after the target's. Half the time, on
	// node-ID ordering alone, every ARP cache now points at a node that no longer
	// answers, and the group is dark on a healed cluster until the caches expire.
	// That is the recovery creating the outage the split-brain avoided
	// (docs/adr/0002-two-node-availability-over-safety.md).
	//
	// After the demotions, so the announcement is the last word on the segment, and
	// before the state broadcast, which is cheap and local by comparison. Synchronous
	// for that ordering: a fire-and-forget announcement can land before the release it
	// is meant to follow, and a discarded RPC result is what defect #5 cost.
	//
	// Bounded per interface by bringUpTimeoutFor (capped at bringUpMaxTimeout, 120s),
	// the same order as the single MakePassive above it, and this pass already runs
	// server calls of that length under the health-checker lock — see #79 for why the
	// lock held across them is a known and separately-tracked problem.
	if err := h.server.AnnounceNodeIPs(target.ID); err != nil {
		// The addresses are up on the target either way; this leaves the stale ARP
		// entries in place rather than creating a new fault, so the pass continues.
		h.logger.Error("ACTIVE_CHECK: failed to re-announce floating IPs after consolidation",
			"target", target.Hostname, "error", err)
	}

	// Push the corrected state out so demoted nodes stop claiming IPs instead
	// of waiting to notice the change on their own.
	states := getMemberStatusMap()
	for id, member := range members {
		member.Lock()
		states[id] = member.Status
		member.Unlock()
	}
	_ = h.server.BroadcastClusterState(states, h.server.GetClusterEpoch()+1, target.ID, nil)
	putMemberStatusMap(states)

	return true
}

// viewStableCycles is how many consecutive unchanged health-check cycles a node
// needs before it will act as active-active coordinator.
//
// Coordinator is a local decision — the lowest-ID node this node considers
// healthy — so it is only single-writer while every node agrees on who is
// healthy. Bulk IP work breaks that agreement: bringing up a hundred addresses
// makes a node slow enough to answer that peers mark it Unknown, and each peer
// that does then makes itself coordinator. On whitecrane all four nodes
// redistributed at once and left ~170 addresses claimed by more than one owner.
// Requiring a settled view means a node with a transient one stands down, and
// the flap resolves before anyone moves an address (docs/TEST-PLAN.md defects
// #2/#26).
const viewStableCycles = 3

// reconcileActiveActive keeps floating IPs assigned and balanced in
// active-active mode. Group IPs no longer hosted by any healthy node are
// redistributed, and IPs are moved onto underloaded nodes. Only the
// coordinator — the healthy node with the lowest ID — acts, so concurrent
// redistribution from multiple nodes can't race.
func (h *HealthChecker) reconcileActiveActive(members map[string]*Member) {
	cycles, stable := h.reconcileCounters()
	if cycles < reconcileGraceCycles {
		return
	}

	cfg := h.members.Config()
	if cfg == nil {
		return
	}
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return
	}
	if clusterCoordinator(members, h.failoverGrace()) != localID {
		return
	}
	if stable < viewStableCycles {
		h.logger.Debug("ACTIVE_CHECK: cluster view still settling, deferring reconciliation",
			"checksWithoutChange", stable)
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

// clusterCoordinator returns the ID of the node that should orchestrate
// cluster-wide IP decisions — active-active redistribution and active-passive
// consolidation: the healthy (Active or Passive) node with the lowest ID.
// Returns "" when no healthy node exists.
//
// grace is how long a node that has gone Unknown still counts. Handing the role
// over on a single missed check meant the busiest node lost it exactly when it
// mattered: moving fifty addresses takes long enough that peers marked the
// coordinator Unknown mid-batch, and the next node in ID order took over and
// re-placed addresses the first one was still holding (docs/TEST-PLAN.md
// defects #2/#26).
func clusterCoordinator(members map[string]*Member, grace time.Duration) string {
	coordinator := ""
	for id, member := range members {
		member.Lock()
		healthy := member.Status == StatusActive || member.Status == StatusPassive ||
			(member.Status == StatusUnknown && time.Since(member.LastHCResponse) <= grace)
		member.Unlock()
		if healthy && (coordinator == "" || id < coordinator) {
			coordinator = id
		}
	}
	return coordinator
}

// failoverGrace is how long a node may go without answering a health check
// before the cluster acts on its absence — the same limit active-passive
// failover already waits out before moving addresses.
func (h *HealthChecker) failoverGrace() time.Duration {
	cfg := h.members.Config()
	if cfg == nil {
		return 0
	}
	return time.Duration(cfg.Pulse.FailOverLimit) * time.Millisecond
}

// localIPPresence returns a presence check over this node's interfaces, building
// the inventory at most once and only if it is actually consulted.
//
// BuildIPInventory is a full netlink enumeration, and the common case is a pass
// with no duplicates at all, so it must not be built up front — nor once per
// address, which is what made demotion enumerate the interfaces hundreds of times
// before stillHeldIPs was changed to share one inventory.
func (h *HealthChecker) localIPPresence() ipPresenceFunc {
	if h.ipPresence != nil {
		return h.ipPresence
	}

	var (
		inv   *network.IPInventory
		err   error
		built bool
	)
	return func(ip string) (bool, string, error) {
		if !built {
			inv, err = network.BuildIPInventory()
			built = true
		}
		if err != nil {
			return false, "", err
		}
		return inv.Exists(ip)
	}
}

// pickDuplicateSurvivor decides which of two members recorded as holding the same
// address keeps it.
//
// Record order — whichever of the two sorts first by node ID — is a coin flip
// against reality. If the node that actually has the address up is the one that
// loses, the coordinator brings down a live address and leaves the record on a node
// that may not have it up at all, so the address is served by nobody until the next
// orphan sweep re-places it: one avoidable flap per duplicate.
//
// Only the local node's interfaces are readable — no RPC exposes a peer's — so the
// kernel decides whenever the local node is one of the two contenders, and record
// order remains the fallback when neither is local or the interfaces cannot be
// read. That covers the case that matters most in practice: the coordinator running
// this pass is usually itself one of the holders.
func pickDuplicateSurvivor(first, second *Member, ip string, present ipPresenceFunc) (winner, loser *Member) {
	for _, candidate := range []*Member{first, second} {
		if !candidate.IsLocal() {
			continue
		}
		other := first
		if candidate == first {
			other = second
		}

		ipOnly := ip
		if cidr, _ := utils.GetCIDR(ip); cidr != nil {
			ipOnly = cidr.String()
		}
		holds, _, err := present(ipOnly)
		if err != nil {
			// Presence could not be established, so it is no better evidence than
			// record order. Fall through to that.
			break
		}
		if holds {
			return candidate, other
		}
		// The local record is demonstrably stale: the peer is the better owner, and
		// dropping the local record costs nothing because nothing is up here.
		return other, candidate
	}
	return first, second
}

// resolveDuplicateAssignments finds IPs tracked by more than one healthy
// member and removes them everywhere but the surviving owner. Convergence races
// can transiently double-assign an IP; left alone that means two nodes
// ARP-fighting over the same address. Returns true if any duplicate was resolved.
func (h *HealthChecker) resolveDuplicateAssignments(members map[string]*Member) bool {
	resolved := false
	owners := make(map[string]*Member)
	present := h.localIPPresence()

	// Losing addresses are collected per node and released in one call each, after
	// every conflict is known: the survivor of a conflict can be a node already
	// visited, so the loser is not always the one currently being examined.
	surrender := make(map[string][]string)

	candidates := rebalanceCandidates(members)
	for _, node := range candidates {
		node.Lock()
		ips := append([]string{}, node.ActiveIPs...)
		node.Unlock()

		// A node listing the same IP twice is a bookkeeping duplicate, not a
		// conflict: the address is up once and belongs here. Treating it as a
		// conflict brought the node's own legitimate address down and left it
		// hosted by nobody until the next orphan sweep re-placed it — the same
		// address going missing cluster-wide that TC-6 kept measuring
		// (docs/TEST-PLAN.md defects #2/#26). Collapse the list instead.
		seenHere := make(map[string]bool, len(ips))
		deduped := false
		for _, ip := range ips {
			if seenHere[ip] {
				deduped = true
				continue
			}
			seenHere[ip] = true
		}
		if deduped {
			h.logger.Warnf("ACTIVE_CHECK: %s recorded the same IP more than once, collapsing its assignment list",
				node.Hostname)
			unique := make([]string, 0, len(seenHere))
			for _, ip := range ips {
				if seenHere[ip] {
					unique = append(unique, ip)
					delete(seenHere, ip)
				}
			}
			node.Lock()
			node.ActiveIPs = unique
			node.Unlock()
			ips = unique
			resolved = true
		}

		for _, ip := range ips {
			owner, seen := owners[ip]
			if !seen {
				owners[ip] = node
				continue
			}

			winner, loser := pickDuplicateSurvivor(owner, node, ip, present)
			owners[ip] = winner
			surrender[loser.ID] = append(surrender[loser.ID], ip)
			h.logger.Warnf("ACTIVE_CHECK: IP %s assigned to both %s and %s, keeping it on %s",
				ip, owner.Hostname, node.Hostname, winner.Hostname)
		}
	}

	// Released in one call per node. One BringDownIPs per address meant one RPC per
	// address to a remote node, each with the client's own 30s deadline, so a node
	// that had picked up a dozen duplicates could spend minutes here. Iterated in
	// candidate order rather than map order so the work is deterministic.
	for _, node := range candidates {
		ips := surrender[node.ID]
		if len(ips) == 0 {
			continue
		}

		node.Lock()
		for _, ip := range ips {
			node.ActiveIPs = removeIPFromList(node.ActiveIPs, ip)
		}
		node.Unlock()

		if err := node.BringDownIPs(ips); err != nil {
			h.logger.Error("ACTIVE_CHECK: failed to bring down duplicate IPs", "ips", ips,
				"hostname", node.Hostname, "error", err)
		}
		resolved = true
	}
	return resolved
}

// redistributeOrphanedIPs reassigns group IPs that no healthy node currently
// hosts — IPs stranded on failed nodes or lost entirely (e.g. a node
// rebooted and came back empty). Returns true if anything was redistributed.
func (h *HealthChecker) redistributeOrphanedIPs(members map[string]*Member) bool {
	// Collect IPs hosted by healthy nodes; clear stranded assignments from
	// failed nodes so their IPs count as orphaned.
	//
	// A node only counts as failed once it has been silent for the failover
	// limit. Acting on the first missed check reclaimed addresses from a node
	// that was merely busy — and the node that gets busiest is the coordinator
	// part-way through a batch of moves, so its peers declared its addresses
	// orphaned and brought them up alongside the copies it was still serving
	// (docs/TEST-PLAN.md defects #2/#26).
	cfg := h.members.Config()
	if cfg == nil {
		return false
	}
	grace := h.failoverGrace()
	hosted := make(map[string]bool)
	for _, member := range members {
		member.Lock()
		switch {
		case member.Status == StatusActive || member.Status == StatusPassive,
			member.Status == StatusUnknown && time.Since(member.LastHCResponse) <= grace:
			for _, ip := range member.ActiveIPs {
				hosted[ip] = true
			}
		case member.Status == StatusUnknown:
			if len(member.ActiveIPs) > 0 {
				h.logger.Warnf("ACTIVE_CHECK: clearing %d IP(s) stranded on failed node %s (silent for %s)",
					len(member.ActiveIPs), member.Hostname, time.Since(member.LastHCResponse).Round(time.Second))
				member.ActiveIPs = nil
				member.LoadFactor = 0
			}
		}
		member.Unlock()
	}

	orphaned := orphanedGroupIPs(cfg.Groups, hosted)
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

// rebalanceActiveActive moves IPs from over-loaded nodes to under-loaded ones
// until the cluster is balanced (difference <= 1). This is what tops up
// freshly joined and failed-back nodes, which otherwise sit Active but empty
// forever. Returns true if any IP was moved.
//
// Moves are planned in one pass and applied a batch per node pair. Doing them
// one address at a time cost a full IP-failover round trip each — roughly one
// address every eleven seconds on the whitecrane cluster, so the ~150 moves a
// switch out of active-passive needs took about 27 minutes to converge
// (docs/TEST-PLAN.md defects #2/#26).
func (h *HealthChecker) rebalanceActiveActive(members map[string]*Member) bool {
	if h.server == nil {
		return false
	}

	cfg := h.members.Config()
	if cfg == nil {
		return false
	}
	nodes := rebalanceCandidates(members)
	moves := planRebalanceMoves(nodes, cfg)
	if len(moves) == 0 {
		return false
	}

	moved := false
	for _, move := range moves {
		src, dst := nodes[move.Src], nodes[move.Dst]

		// Re-read under the lock: the plan came from a snapshot, and a
		// concurrent ConfigSync self-report may have shrunk the source since.
		// The addresses must come from the batch's own group — an arbitrary tail
		// of ActiveIPs can belong to a group the destination cannot host.
		ips := rebalanceMoveIPs(src, cfg, move.Group, move.Count)
		if len(ips) == 0 {
			continue
		}

		h.logger.Infof("ACTIVE_CHECK: rebalancing %d IP(s) from %s to %s", len(ips), src.Hostname, dst.Hostname)
		if err := h.server.OrchestrateIPFailover(src.ID, dst.ID, ips); err != nil {
			h.logger.Error("ACTIVE_CHECK: rebalance move failed", "count", len(ips), "error", err)
			break
		}

		src.Lock()
		for _, ip := range ips {
			src.ActiveIPs = removeIPFromList(src.ActiveIPs, ip)
		}
		src.Unlock()

		// Skip what the destination already records. A concurrent coordinator,
		// or a self-report that landed mid-move, can have credited it already,
		// and a doubled entry is worse than a missing one: the dedup pass then
		// has to decide whether the address is a conflict.
		dst.Lock()
		held := make(map[string]bool, len(dst.ActiveIPs))
		for _, ip := range dst.ActiveIPs {
			held[ip] = true
		}
		for _, ip := range ips {
			if !held[ip] {
				dst.ActiveIPs = append(dst.ActiveIPs, ip)
			}
		}
		dst.Status = StatusActive
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

// planRebalanceMoves returns every IP move needed to balance nodes, batched per
// source/destination/group triple, or nothing when the assignment is already
// balanced (max-min <= 1). Nodes must be sorted by ID so ties resolve
// deterministically.
//
// Group eligibility constrains the plan in both directions: a destination can
// only receive groups assigned to it, and a source can only give up groups the
// destination will accept. Planning blind to that sent addresses to a node that
// could not bring them up, and rebalanceActiveActive abandons the whole pass on
// the first failed move — so a single node the group had been unassigned from
// stalled all rebalancing (docs/TEST-PLAN.md defect #40).
//
// A nil cfg plans group-blind, which is what a caller with no config to consult
// can safely assume.
func planRebalanceMoves(nodes []*Member, cfg *config.Config) []ipam.Move {
	var index map[string]string
	if cfg != nil {
		index = groupIndex(cfg.Groups)
	}

	snapshots := make([]ipam.Node, 0, len(nodes))
	for _, node := range nodes {
		node.Lock()
		snapshot := ipam.Node{
			Hostname: node.Hostname,
			IPCount:  len(node.ActiveIPs),
			Capacity: node.Capacity,
			Groups:   nodeHostableGroups(cfg, node.ID),
		}
		if index != nil {
			held := make(map[string]int)
			for _, ip := range node.ActiveIPs {
				if group, ok := index[ip]; ok {
					held[group]++
				}
			}
			snapshot.Held = held
		}
		node.Unlock()
		snapshots = append(snapshots, snapshot)
	}
	return ipam.PlanMoves(snapshots)
}

// rebalanceMoveIPs picks up to count addresses of the given group off node, for
// a planned move to hand to the destination. An empty group takes any address,
// which is what a group-blind plan asks for.
//
// Addresses are taken from the end of the list: the tail is the most recently
// assigned, so a rebalance gives back what it most recently took.
func rebalanceMoveIPs(node *Member, cfg *config.Config, group string, count int) []string {
	if count <= 0 {
		return nil
	}

	var index map[string]string
	if group != "" && cfg != nil {
		index = groupIndex(cfg.Groups)
	}

	node.Lock()
	defer node.Unlock()

	picked := make([]string, 0, count)
	for i := len(node.ActiveIPs) - 1; i >= 0 && len(picked) < count; i-- {
		ip := node.ActiveIPs[i]
		if index != nil && index[ip] != group {
			continue
		}
		picked = append(picked, ip)
	}
	return picked
}

// recoveredStatus returns the status a reachable node that was Unknown should
// return to.
//
// Every eligible node is Active in active-active, so recovering to Passive
// there is a demotion the mode has no concept of — and an expensive one. A
// non-Active node has its recorded assignments cleared on the next broadcast
// and every cluster floating IP stripped by the IP monitor, so the coordinator
// sees those addresses orphaned and re-places them, only for the node to go
// Active again on the next BringUpIP. That loop kept addresses moving between
// owners and briefly off the cluster entirely (docs/TEST-PLAN.md defects
// #2/#26). The auto-failback path already makes this distinction; this is the
// same decision with auto-failback off.
func recoveredStatus(mode string) MemberStatus {
	if mode == "active-active" {
		return StatusActive
	}
	return StatusPassive
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

	cfg := h.members.Config()
	if cfg == nil {
		h.logger.Error("Failed to get local node ID for election: no config")
		return
	}
	localNodeID, err := cfg.GetLocalNodeUUID()
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
	cfg := h.members.Config()
	if cfg == nil {
		h.logger.Error("Emergency fallback: no config")
		return
	}
	localNodeID, err := cfg.GetLocalNodeUUID()
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
// membershipOf runs the deep membership check, through the test seam when one
// is installed.
func (h *HealthChecker) membershipOf(member *Member) membershipVerdict {
	if h.deepCheck != nil {
		return h.deepCheck(member)
	}
	return h.checkClusterMembership(member)
}

// membershipVerdict is what a deep membership check concluded, as distinct from
// whether it succeeded.
//
// The distinction is the whole point. This check used to return a bare bool, so
// seven unrelated causes collapsed into one false: an empty IP, a nil config, a
// missing local node id, a client that would not construct, a transport that
// would not connect, a gRPC deadline or Unavailable, a peer that does not have
// us in its memberlist, and a peer echoing a different cluster token.
//
// The caller latches a failure until the next *passing* deep check, five cycles
// later, and the latch exists to keep a node in a foreign cluster marked
// unreachable even when it answers a TCP dial. Only the last two causes are
// that. A two-second deadline on a loaded VM is not a foreign cluster, and
// latching it cost four further cycles of Unknown after the cause had gone --
// which is what made an lb_api CI pipeline report a node unknown on odd
// occasions.
type membershipVerdict int

const (
	// membershipUnverified: the check could not be completed. Says nothing
	// about the peer's cluster, so it must not latch.
	membershipUnverified membershipVerdict = iota
	// membershipConfirmed: the peer answered and agrees it is in this cluster.
	membershipConfirmed
	// membershipRejected: the peer answered, and it is not in this cluster --
	// it does not know this node, or it echoes a different cluster token. This
	// is the only conclusion worth latching.
	membershipRejected
)

func (v membershipVerdict) String() string {
	switch v {
	case membershipConfirmed:
		return "confirmed"
	case membershipRejected:
		return "rejected"
	default:
		return "unverified"
	}
}

func (h *HealthChecker) checkClusterMembership(member *Member) membershipVerdict {
	if member.IP == "" || member.Port == "" {
		return membershipUnverified
	}

	cfg := h.members.Config()
	if cfg == nil {
		h.logger.Warn("checkClusterMembership: no config")
		return membershipUnverified
	}
	localToken := cfg.Pulse.ClusterToken
	localNodeID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		h.logger.Warnf("checkClusterMembership: failed to get local node ID: %v", err)
		return membershipUnverified
	}

	remoteClient, err := client.New()
	if err != nil {
		h.logger.Warnf("checkClusterMembership: failed to create client: %v", err)
		return membershipUnverified
	}
	defer remoteClient.Close()

	if err := remoteClient.Connect(member.IP, member.Port, false); err != nil {
		h.logger.Warnf("checkClusterMembership: failed to connect to %s (%s:%s): %v",
			member.Hostname, member.IP, member.Port, err)
		return membershipUnverified
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := remoteClient.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{
		NodeId:       localNodeID,
		ClusterToken: localToken,
	})
	if err != nil {
		// Unverified, not rejected: a DeadlineExceeded from a loaded peer and an
		// Unavailable from a dead one are indistinguishable here, and neither is
		// a statement about which cluster the peer belongs to.
		h.logger.Warnf("checkClusterMembership: gRPC call to %s failed: %v", member.Hostname, err)
		return membershipUnverified
	}

	if !resp.Success {
		// Rejected: the peer answered and does not have us in its memberlist,
		// which on an appliance whose node id is re-minted by `lbcli setup` is a
		// real and asymmetric condition worth latching.
		h.logger.Warnf("checkClusterMembership: %s rejected membership check: %s", member.Hostname, resp.Message)
		return membershipRejected
	}

	// Verify the remote node echoes a matching token (detects split-cluster scenarios)
	if localToken != "" && resp.ClusterToken != "" && resp.ClusterToken != localToken {
		h.logger.Warnf("checkClusterMembership: %s is in a different cluster (token mismatch)", member.Hostname)
		return membershipRejected
	}

	return membershipConfirmed
}

// checkNodeConnectivity verifies basic node connectivity
func (h *HealthChecker) checkNodeConnectivity(member *Member) bool {
	// Use member's stored IP and Port directly (no config lookup needed)
	if member.IP == "" || member.Port == "" {
		h.logger.Warnf("Node %s has empty IP (%s) or Port (%s)", member.Hostname, member.IP, member.Port)
		return false
	}

	// Try to establish basic connection
	//
	// JoinHostPort rather than the FormatIPv6 + "%s:%s" idiom, for the same reason
	// as the two dial probes in internal/server/server.go: the two are equivalent
	// for every input, but `go vet` cannot see through the helper and reports every
	// "%s:%s" reaching net.Dial as broken for IPv6. This was the last such finding
	// in the tree, and one known-false finding is enough to bury a real one.
	address := net.JoinHostPort(utils.SanitizeIPv6(member.IP), member.Port)
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
			// Two nodes *available*, which is not the same as a two-node cluster and must
			// never be reached from one. availableNodes counts only Active and Passive
			// members, so this branch belongs to a degraded cluster of three or more — the
			// Active has failed and two Passives remain (handlePartialFailure), or two of
			// four are Unknown (attemptVotingElection). A genuine two-node cluster never
			// arrives here at all: attemptVotingElection returns at availableCount < 3 and
			// handlePartialFailure only votes at clusterSize >= 3, so the pair promotes
			// directly and both sides claim the group.
			//
			// That is deliberate, not an oversight. Electing a single owner by node ID means
			// deciding from a node that cannot see its peer, so the winner may be the one
			// whose service network is the broken half — turning a duplicated address into a
			// dark one. See docs/adr/0002-two-node-availability-over-safety.md. Routing the
			// two-node election into this branch would reverse that decision.
			//
			h.logger.Infof("Exactly 2 nodes available, using deterministic tie-breaking")
			if newStatus == StatusActive {
				// Decided about nodeID, the subject of the vote, and never about the node
				// running it (END-2325).
				//
				// The rule used to be `localNodeID < otherNodeID`, which answers "should *I*
				// win" to a question asked about someone else. Via attemptVotingElection the
				// candidate is frequently not the local node — selectBestCandidate gives the
				// local node only +5 against a score built from status, latency and recency —
				// so a coordinator whose own ID sorted higher returned false and blocked the
				// promotion of a perfectly good candidate. In handlePartialFailure that
				// answer is not advisory: `!voteResult` returns, abandoning the failover.
				//
				// Worse than blocking one promotion, it made the answer depend on who asked.
				// Two nodes running this concurrently for the same candidate computed
				// opposite results, which is precisely what a tie-break with no majority
				// behind it must never do. Deciding on the subject is viewer-independent:
				// every node reaches the same verdict about the same candidate. Same lesson
				// as the config tiebreak, which had to become origin-versus-origin rather
				// than sender-versus-receiver (internal/server/config_generation_test.go).
				lowest := ""
				subjectAvailable := false
				for _, member := range membersSnapshot {
					member.Lock()
					isAvailable := member.Status == StatusActive || member.Status == StatusPassive
					memberID := member.ID
					member.Unlock()
					if !isAvailable {
						continue
					}
					if memberID == nodeID {
						subjectAvailable = true
					}
					if lowest == "" || memberID < lowest {
						lowest = memberID
					}
				}

				// A subject that is not one of the two contenders is not in the tie this rule
				// exists to break, so it has nothing to say about it. Allowed rather than
				// refused, which is what the old code did for its own unresolvable case, and
				// the promotion still has to get past confirmPeerReleasedIPs.
				if !subjectAvailable {
					h.logger.Info("2-node tie-breaking: subject is not an available node, allowing",
						"subject", nodeID)
					return true
				}

				shouldWin := nodeID == lowest
				h.logger.Infof("2-node tie-breaking: subject=%s, lowest=%s, shouldWin=%v",
					nodeID, lowest, shouldWin)
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
		localNodeID, err := h.localNodeID()
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

	// Refresh the quorum manager's node count — it is seeded at daemon startup
	// and goes stale as nodes join, which would make StartVotingSession refuse
	// with "requires at least 3 nodes" on a healthy 3+ node cluster.
	quorumManager.UpdateNodeCount(clusterSize)

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
	localNodeID, err := h.localNodeID()
	if err != nil {
		h.logger.Errorf("Failed to get local node ID: %v", err)
	} else {
		// Cast our own vote (we initiated it, so we vote yes)
		err = quorumManager.CastVote(sessionID, localNodeID, quorum.VoteDecisionYes)
		if err != nil {
			h.logger.Errorf("Failed to cast our own vote: %v", err)
		}
	}

	// Broadcast the vote request to other nodes so they can participate —
	// without this the session only ever holds the initiator's vote and can
	// never reach quorum, permanently blocking redistribution.
	h.logger.Info("Broadcasting IP redistribution vote request to cluster nodes...")
	if err := h.server.BroadcastVoteRequest(sessionID, "ip_redistribution", subject, description, 30); err != nil {
		h.logger.Warnf("Failed to broadcast vote request: %v", err)
		// Continue anyway - maybe some nodes are offline but others might still vote
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
