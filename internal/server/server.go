package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/packages/utils"
	rpc "github.com/syleron/pulseha/rpc"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Object pools to reduce memory allocations
var (
	stringMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]string, 8)
		},
	}
	statusMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]membership.MemberStatus, 8)
		},
	}
	stringSliceMapPool = sync.Pool{
		New: func() interface{} {
			return make(map[string][]string, 8)
		},
	}
)

// Helper functions to manage object pools
func getStringMap() map[string]string {
	m := stringMapPool.Get().(map[string]string)
	// Clear the map
	for k := range m {
		delete(m, k)
	}
	return m
}

func putStringMap(m map[string]string) {
	if m != nil {
		stringMapPool.Put(m)
	}
}

func getStatusMap() map[string]membership.MemberStatus {
	m := statusMapPool.Get().(map[string]membership.MemberStatus)
	// Clear the map
	for k := range m {
		delete(m, k)
	}
	return m
}

func putStatusMap(m map[string]membership.MemberStatus) {
	if m != nil {
		statusMapPool.Put(m)
	}
}

func getStringSliceMap() map[string][]string {
	m := stringSliceMapPool.Get().(map[string][]string)
	// Clear the map
	for k := range m {
		delete(m, k)
	}
	return m
}

func putStringSliceMap(m map[string][]string) {
	if m != nil {
		stringSliceMapPool.Put(m)
	}
}

// isDemotion reports whether a status transition means this node has actually
// been demoted and must release its floating IPs.
//
// StatusUnknown deliberately does not count. It means peers have lost sight of
// the node, not that the node lost its role — and the node that goes Unknown is
// by definition the one already under load, so treating it as a demotion makes a
// healthy Active strip every floating IP over a transient health-check blip.
// Recovery is bounded by the per-IP GARP, so a large group then stays down for
// minutes. Unknown is left to the health checker to resolve into a real state.
func isDemotion(old, new membership.MemberStatus) bool {
	if old != membership.StatusActive {
		return false
	}
	return new == membership.StatusPassive || new == membership.StatusMaintenance
}

// callerAddr returns the network address of the gRPC caller for audit logging.
// Group membership is cluster-wide persisted state; any client (pulsectl, the
// appliance API over the unix socket, or a peer node) can mutate it, so log
// who asked — without this, externally triggered removals are indistinguishable
// from daemon-internal ones (see the floating IP group wipe incident).
func callerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

// Server represents the PulseHA server
type Server struct {
	pulselock.RWMutex
	config      *config.Config
	logger      *log.Logger
	memberList  *membership.MemberList
	healthCheck *membership.HealthChecker

	// One config repair at a time, no more often than configRepairInterval. See
	// startConfigRepair: envelope syncs arrive every few seconds, so a mismatch
	// the pull cannot fix must not become a pull per envelope.
	repairInFlight   atomic.Bool
	lastConfigRepair atomic.Pointer[time.Time]

	// Who the config-authoritative node is. nil in production, where it reads the
	// health checker; see clusterCoordinatorID.
	coordinatorID func() string

	// Every peer's last reported config fingerprint, which is what lets this node
	// tell whether it or the coordinator is the diverged one. See
	// noteConfigDivergence.
	peerHashMu pulselock.Mutex
	peerHashes map[string]peerHash
	ipMonitor  *membership.IPMonitor

	// peerBringUp coalesces per-address peer bring-ups (defect #37); see
	// peerBringUpQueue for why it is built lazily.
	peerBringUpMu pulselock.Mutex
	peerBringUp   *peerBringUpBatcher

	// vipReconcile coalesces the post-load VIP reconcile, which loadInitialMembers
	// schedules on every full ConfigSync (defect #65). Lazily built for the same
	// reason as peerBringUp.
	vipReconcileMu pulselock.Mutex
	vipReconcile   *vipReconciler

	quorumManager *quorum.QuorumManager
	quorumHandler *quorum.RPCHandler
	grpcServer    *grpc.Server
	cliServer     *grpc.Server
	rpc.UnimplementedCLIServer
	rpc.UnimplementedServerServer
	// Convergence state
	clusterEpoch int64
	leaderID     string
	// Rate limiting for refresh calls
	lastRefresh      time.Time
	leaderLeaseUntil time.Time
	// Connection pool for peer clients
	peerClients map[string]*client.Client
	clientMutex pulselock.RWMutex
	// Unix socket path used by the CLI gRPC server
	cliSocketPath string
	// reconfigureMu serializes Reconfigure() so concurrent callers (such as
	// ConfigSync-triggered reconfigures) don't race on the cluster listener
	// bind to IP:port.
	reconfigureMu pulselock.Mutex

	// clusterInitMu serializes CreateCluster and InitiateJoin to prevent a
	// TOCTOU race where both pass the ClusterCheck() guard concurrently and
	// both activate as the "first node", causing dual-active in active-passive mode.
	clusterInitMu pulselock.Mutex

	// Config propagation ordering (docs/TEST-PLAN.md defects #5 and #38).
	//
	// configStamp is a Lamport clock over the cluster's config, paired with the ID
	// of the node whose mutation produced that content: it versions the config's
	// *content*, not the node holding it. A mutation sets the version to one above
	// everything this node has seen and stamps itself as the origin, and applying a
	// peer's config adopts that config's stamp, so every node holding the same
	// content agrees on both fields. A payload is stale if its version is below the
	// one already held, no matter which node sent it.
	//
	// It replaced a per-sender generation, which ordered each node's snapshots
	// against that node's own previous ones and nothing else. That caught a
	// reordered delivery (#5) but was structurally blind to a peer that was simply
	// behind (#38): the coordinator re-broadcasts once a minute and is usually not
	// the node taking the mutations, so its stale view arrived on a sequence of its
	// own, passed the guard, and was applied wholesale — erasing adds that had
	// already reported success, on every node at once. Worse, the counter only
	// moved on a node's *own* mutations, so a coordinator that had never mutated
	// stayed at 0 and broadcast unversioned, which the receiver applies
	// unconditionally. Adopting the version on apply closes both.
	//
	// In-memory only and never persisted: the appliance rewrites config.json out
	// from under us (defect #3), so a number on disk would be wrong exactly when
	// that happens. The cost is that a just-restarted node reads 0 until its first
	// full ConfigSync, so a mutation issued on it in that window is rejected by
	// peers as stale. That is logged (see broadcastConfigToPeersOnce) rather than
	// silent, and it is the correct answer: a node that is behind must not push a
	// config, because ConfigSync applies one wholesale.
	//
	// Atomic rather than lock-guarded because every group mutation calls
	// markConfigDirty while holding s.Lock() through a defer — taking the write
	// lock to bump this would self-deadlock on a non-reentrant mutex and hang
	// every add-ip. HandleNodeJoin calls it holding no lock at all, so the
	// counter has to be safe under either. A single pointer rather than two atomics
	// because the version and its origin are only meaningful together: a reader
	// that saw a new version against a previous origin would order the two
	// concurrent mutations differently from its peers, which is the divergence the
	// pair exists to prevent.
	configStamp atomic.Pointer[configStamp]

	// broadcastTrigger coalesces config broadcasts. It has capacity 1 and is
	// signalled rather than written to: a single broadcaster goroutine drains
	// it, so concurrent mutations collapse into one broadcast of the final
	// state instead of racing each other onto the wire.
	broadcastTrigger chan struct{}
	broadcastStop    chan struct{}
	broadcastOnce    sync.Once

	// unpropagated is the config this node committed and could not push to every
	// peer, plus the retry that will (docs/TEST-PLAN.md defect #43). Guarded by
	// its own mutex: the broadcaster writes it at the end of a pass and reads it
	// to schedule the next one, and neither point can take s.RWMutex, which the
	// mutation that started the broadcast may still hold.
	propagationMu pulselock.Mutex
	unpropagated  *unpropagatedConfig

	// clusterListenSrv/clusterListenAddr record what the cluster gRPC listener is
	// currently serving, so Reconfigure can tell a config-only change from one
	// that actually moves the bind address (defect #31). Guarded by their own
	// mutex because the serving goroutine clears them when Serve returns, off any
	// path that holds s.RWMutex.
	clusterListenMu   pulselock.Mutex
	clusterListenSrv  *grpc.Server
	clusterListenAddr string

	// onAsyncReconfigure, when non-nil, is called once the Reconfigure that a
	// full ConfigSync spawns has returned. A test seam, nil in production.
	//
	// ConfigSync replies before that goroutine has re-read the config file, and
	// every harness in this package points the package-level
	// config.CONFIG_LOCATION at its own t.TempDir(). A test that finishes while
	// the goroutine is still inside config.Load() therefore lets the *next*
	// test's setup write the global the goroutine is reading — a real data race
	// the detector flags intermittently, in the harness rather than in the
	// daemon. Waiting on the reconfigure instead of on the config pointer is what
	// makes it deterministic: the pointer it swaps to cannot be distinguished
	// from the one ConfigSync itself installed when the goroutine wins the race.
	//
	// Set it before the first ConfigSync and never afterwards; the goroutine
	// reads it without synchronisation.
	onAsyncReconfigure func()

	// lastAnnounce debounces re-announcements, guarded by its own mutex because it is
	// written from the health-check pass and must not queue behind whatever else holds the
	// server lock.
	announceMu   pulselock.Mutex
	lastAnnounce time.Time

	// asyncReconfigures counts the Reconfigure goroutines full ConfigSyncs have
	// spawned and not yet finished, so awaitAsyncReconfigures can wait them out.
	//
	// This is the bound onAsyncReconfigure above does not provide. That hook
	// makes one reconfigure observable to a caller that chooses to wait; this
	// makes every in-flight one waitable from a single place, which is what a
	// test harness needs — thirteen of the fourteen ConfigSync call sites in this
	// package's tests never waited, and each left a goroutine reading
	// config.CONFIG_LOCATION past the end of the test that started it.
	asyncReconfigures sync.WaitGroup

	// configRepairs is the same bound for the repair pulls startConfigRepair
	// spawns. A repair outlives a short test easily -- it makes a gRPC round trip
	// to the coordinator -- and the race detector catches it writing as the test's
	// own cleanup runs.
	configRepairs sync.WaitGroup
}

// awaitAsyncReconfigures blocks until every Reconfigure goroutine spawned by a
// full ConfigSync has returned. Used by the test harnesses in this package to
// keep a goroutine from outliving the test that started it; harmless in
// production, where nothing calls it.
func (s *Server) awaitAsyncReconfigures() {
	s.asyncReconfigures.Wait()
}

// awaitConfigRepairs blocks until every repair pull has returned. Same purpose
// and same caveat as awaitAsyncReconfigures: for the test harnesses, harmless in
// production.
func (s *Server) awaitConfigRepairs() {
	s.configRepairs.Wait()
}

// NewServer creates a new PulseHA server instance
func NewServer(cfg *config.Config, logger *log.Logger, memberList *membership.MemberList, healthCheck *membership.HealthChecker) *Server {
	// Create the quorum manager
	quorumMgr := quorum.NewQuorumManager(cfg, logger)

	// Create the quorum RPC handler
	quorumHandler := quorum.NewRPCHandler(quorumMgr, logger)

	// Create IP monitor - re-enabled with clean architectural separation from health checker
	ipMonitor := membership.NewIPMonitor(memberList, logger)
	memberList.SetIPMonitor(ipMonitor)
	// Create server
	s := &Server{
		config:        cfg,
		logger:        logger,
		memberList:    memberList,
		healthCheck:   healthCheck,
		ipMonitor:     ipMonitor, // Re-enabled with clean architectural separation
		quorumManager: quorumMgr,
		quorumHandler: quorumHandler,
		clusterEpoch:  0,
		leaderID:      "",
		peerClients:   make(map[string]*client.Client), // Initialize connection pool

		broadcastTrigger: make(chan struct{}, 1),
		broadcastStop:    make(chan struct{}),
	}

	// Set server reference in health checker
	healthCheck.SetServerReference(s)

	return s
}

// Start initializes and starts the server components
func (s *Server) Start() error {
	s.Lock()
	defer s.Unlock()

	// Verify config is loaded
	s.logger.Debug("Verifying server configuration...")
	if s.config == nil {
		return fmt.Errorf("server config is nil")
	}

	// Owns every outbound config push, so that concurrent mutations coalesce
	// into one ordered broadcast instead of racing each other (defect #5).
	s.startConfigBroadcaster()

	// Load initial members from config
	s.logger.Debug("Loading initial members from configuration...")
	if s.memberList == nil {
		return fmt.Errorf("member list is nil")
	}
	if err := s.loadInitialMembers(s.config); err != nil {
		return fmt.Errorf("failed to load initial members: %v", err)
	}

	// Start CLI server on a Unix socket.
	// Test mode gets a per-instance temp path to avoid conflicts between concurrent in-process servers.
	socketPath := config.CLI_SOCKET_PATH
	if os.Getenv("PULSEHA_TEST") == "true" {
		socketPath = fmt.Sprintf("/tmp/pulseha-test-%s.sock", uuid.New().String()[:8])
	}
	s.cliSocketPath = socketPath
	s.logger.Debug("Starting CLI gRPC server", "socket", socketPath)

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return fmt.Errorf("failed to create socket directory %s: %v", filepath.Dir(socketPath), err)
	}
	// Only remove the existing socket if it is stale. If another daemon is already
	// listening on it, refuse to start rather than silently steal its socket.
	if _, statErr := os.Stat(socketPath); statErr == nil {
		conn, dialErr := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return fmt.Errorf("another process is already listening on %s; refusing to start", socketPath)
		}
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove stale socket %s: %v", socketPath, err)
		}
	}

	s.cliServer = grpc.NewServer()
	// Register both CLI and Server services on the local listener so local operations (e.g., ConfigSync) work pre-cluster
	rpc.RegisterServerServer(s.cliServer, s)
	rpc.RegisterCLIServer(s.cliServer, s)
	cliListener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on Unix socket %s: %v", socketPath, err)
	}
	// Restrict access to the socket owner only
	if err := os.Chmod(socketPath, 0600); err != nil {
		s.logger.Warn("Failed to set socket permissions", "socket", socketPath, "error", err)
	}
	go func() {
		s.logger.Debug("Serving CLI gRPC", "addr", cliListener.Addr().String())
		if err := s.cliServer.Serve(cliListener); err != nil {
			s.logger.Error("CLI server failed", "error", err)
		}
	}()

	// Attempt to start cluster server ONLY if configuration is present
	var localNode config.Node
	localNode, err = s.config.GetLocalNodeForBinding()
	if err == nil {
		if err := s.startClusterListener(localNode); err != nil {
			return err
		}
	} else {
		s.logger.Info("No cluster binding configuration found; cluster RPC server not started.", "cli_socket", s.cliSocketPath)
	}

	// Generate certificates if they don't exist
	s.logger.Debug("Checking/Generating TLS certificates...")
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %v", err)
	}
	if os.Getenv("PULSEHA_TEST") != "true" {
		if err := security.GenerateCertificates(hostname); err != nil {
			s.logger.Warn("Failed to generate certificates, continuing without TLS", "error", err)
		}
	} else {
		s.logger.Debug("PULSEHA_TEST=true: skipping certificate generation")
	}

	// Set server reference in health checker
	s.logger.Debug("Setting server reference in health checker...")
	if s.healthCheck != nil {
		s.healthCheck.SetServerReference(s)
		s.logger.Debug("Server reference set in health checker")
	} else {
		s.logger.Warn("Health checker is nil, cannot set server reference")
	}

	// Initialize quorum manager with node count
	s.logger.Debug("Initializing quorum manager...")
	if s.quorumManager != nil {
		nodeCount := s.memberList.GetMemberCount()
		s.quorumManager.UpdateNodeCount(nodeCount)
		s.quorumManager.Start()
		s.logger.Debug("Quorum manager started", "node_count", nodeCount)
	} else {
		s.logger.Warn("Quorum manager is nil, quorum voting will not be available")
	}

	// Register quorum RPC handlers if available
	if s.quorumHandler != nil {
		s.logger.Debug("Registering quorum RPC handlers...")
	} else {
		s.logger.Warn("Quorum handler is nil, quorum RPC methods will not be available")
	}

	// Only start the cluster loops if we have a configured cluster.
	//
	// This used to be preceded by an unconditional startHealthChecker() and an unconditional
	// ipMonitor.Start(), which between them made the guard below decorative: the checker was
	// started either way, and the monitor was started at the one moment it could not possibly
	// succeed — before any cluster exists — logging an Error and never being retried
	// (docs/TEST-PLAN.md defect #83). startHealthChecker now owns both, and every path that
	// creates or joins a cluster already calls it.
	if s.config.ClusterCheck() {
		s.startHealthChecker()
	} else {
		s.logger.Debug("No cluster configured; waiting for explicit resync after join")
	}

	return nil
}

// startClusterListener starts the gRPC server that handles inter-node RPC on the configured bind address
func (s *Server) startClusterListener(localNode config.Node) error {
	s.logger.Debugf("Starting cluster RPC server on %s:%s...", utils.FormatIPv6(localNode.IP), localNode.Port)

	// Create gRPC server if needed
	if s.grpcServer == nil {
		s.grpcServer = grpc.NewServer()
		rpc.RegisterServerServer(s.grpcServer, s)
		// Also register CLI RPCs on the cluster listener to support remote operations like Join
		rpc.RegisterCLIServer(s.grpcServer, s)
	}

	address := fmt.Sprintf("%s:%s", utils.FormatIPv6(localNode.IP), localNode.Port)
	listenCtx, cancelListen := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelListen()
	listener, err := listenTCPReuseAddr(listenCtx, address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", address, err)
	}

	// If bound to an ephemeral port, record the actual port in config
	if localNode.Port == "0" {
		if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
			actualPort := strconv.Itoa(tcpAddr.Port)
			if localID, e := s.config.GetLocalNodeUUID(); e == nil {
				if n := s.config.Nodes[localID]; n != nil {
					n.Port = actualPort
					_ = s.config.Save()
					s.logger.Debugf("Updated local node port to actual bound port: %s", actualPort)
				}
			}
			// Record what was actually bound rather than "port 0". Reconfigure
			// compares its config-derived address against this record, and the
			// config now holds the real port, so a stale "0" would make every
			// reconfigure look like a move of the bind address.
			address = fmt.Sprintf("%s:%s", utils.FormatIPv6(localNode.IP), actualPort)
		}
	}

	// Capture the gRPC server pointer locally so a concurrent Reconfigure()
	// swapping s.grpcServer doesn't race with this goroutine's read.
	grpcSrv := s.grpcServer
	s.setClusterListener(grpcSrv, address)
	go func() {
		s.logger.Debug("Serving cluster gRPC", "addr", listener.Addr().String())
		if err := grpcSrv.Serve(listener); err != nil {
			s.logger.Error("Cluster gRPC server failed", "error", err)
		}
		// Forget the record if this is still the serving instance, so a listener
		// that died on its own is rebound by the next Reconfigure rather than
		// skipped as still-serving. A GracefulStop from Reconfigure has already
		// replaced the pointer by the time it gets here, so this leaves the new
		// listener's record alone.
		s.clearClusterListener(grpcSrv)
	}()

	return nil
}

// setClusterListener records the address a gRPC server instance is serving.
func (s *Server) setClusterListener(srv *grpc.Server, address string) {
	s.clusterListenMu.Lock()
	defer s.clusterListenMu.Unlock()
	s.clusterListenSrv, s.clusterListenAddr = srv, address
}

// clusterListenerServing reports whether a live listener is already serving
// address, which is Reconfigure's licence to leave it alone.
func (s *Server) clusterListenerServing(address string) bool {
	s.clusterListenMu.Lock()
	defer s.clusterListenMu.Unlock()
	return s.clusterListenSrv != nil && s.clusterListenAddr == address
}

// clearClusterListener forgets the record if srv is still the serving instance.
func (s *Server) clearClusterListener(srv *grpc.Server) {
	s.clusterListenMu.Lock()
	defer s.clusterListenMu.Unlock()
	if s.clusterListenSrv == srv {
		s.clusterListenSrv, s.clusterListenAddr = nil, ""
	}
}

// listenTCPReuseAddr is net.Listen("tcp", addr) with SO_REUSEADDR (and
// best-effort SO_REUSEPORT) set on the underlying socket so that re-binding
// the same IP:port immediately after Close() succeeds even while the kernel
// still holds the previous endpoint in TIME_WAIT.
func listenTCPReuseAddr(ctx context.Context, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, addr string, c syscall.RawConn) error {
			var sockErr error
			ctlErr := c.Control(func(fd uintptr) {
				if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
					sockErr = err
					return
				}
				// SO_REUSEPORT is best-effort hardening on top of SO_REUSEADDR;
				// it allows rebind in additional kernel states. Failure is non-fatal
				// (e.g. legacy kernels return ENOPROTOOPT, seccomp may strip it).
				if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
					log.Debug("SO_REUSEPORT setsockopt failed (best-effort, continuing)", "addr", addr, "error", err)
				}
			})
			if ctlErr != nil {
				return ctlErr
			}
			return sockErr
		},
	}
	return lc.Listen(ctx, "tcp", address)
}

// startHealthChecker starts the health checker with the configured interval
func (s *Server) startHealthChecker() {
	s.logger.Debug("Starting health checker...")
	if s.healthCheck == nil {
		s.logger.Error("Health checker is nil, cannot start")
		return
	}

	// Check if health checker is already running
	if s.healthCheck.IsRunning() {
		s.logger.Debug("Health checker is already running")
		return
	}

	// Get interval from config or use default
	interval := 5 * time.Second
	if s.config.Pulse.HealthCheckInterval > 0 {
		interval = time.Duration(s.config.Pulse.HealthCheckInterval) * time.Millisecond
		s.logger.Infof("Using configured health check interval: %v", interval)
	} else {
		s.logger.Infof("Using default health check interval: %v", interval)
	}

	s.logger.Info("Initializing health checker", "interval", interval)
	s.healthCheck.Start(interval)
	s.logger.Info("Health checker started successfully")

	// The IP monitor is started here, with the health checker, because the two share a
	// precondition and a trigger: neither can run without a configured cluster, and both
	// become startable at the same moments — daemon start on an already-configured node,
	// cluster create, join, resync, node removal. Every one of those already calls this
	// function, so pairing them is what stops a seventh caller from starting one and not the
	// other, which is how defect #83 happened: the monitor's one Start call sat in
	// Server.Start, ran before any cluster existed, failed, and was never retried.
	//
	// Idempotent on both sides, so the repeat calls this attracts cost nothing.
	if s.ipMonitor != nil && !s.ipMonitor.IsRunning() {
		if err := s.ipMonitor.Start(); err != nil {
			// Not fatal, and on a node that has not joined a cluster yet it is not even
			// wrong — the next caller retries. Debug rather than Error for that reason.
			s.logger.Debug("IP monitor not started yet", "error", err)
		}
	}
}

// Stop gracefully shuts down the server
func (s *Server) Stop() {
	s.logger.Info("Stopping PulseHA server")

	// Before the teardown below starts mutating state we no longer want to
	// propagate, and so an in-flight retry loop cannot outlive the server.
	s.stopConfigBroadcaster()

	// Best-effort convergence hint: broadcast we're going down so peers can elect a new active
	func() {
		// Build states with local set to Unknown; clear leader to allow election
		localID, _ := s.config.GetLocalNodeUUID()
		states := getStatusMap()
		membersSnapshot := s.memberList.MembersSnapshot()
		for id, m := range membersSnapshot {
			if id == localID {
				states[id] = membership.StatusUnknown
			} else {
				states[id] = m.GetStatus()
			}
		}
		_ = s.BroadcastClusterState(states, s.GetClusterEpoch()+1, "", nil)
		putStatusMap(states)
	}()

	// Gather local VIPs to tear down without holding the server lock
	ifaceToIPs := getStringSliceMap()
	defer putStringSliceMap(ifaceToIPs)
	if s.config != nil {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil {
			s.RLock()
			if node := s.config.Nodes[localID]; node != nil {
				for iface, groups := range node.IPGroups {
					var ips []string
					for _, g := range groups {
						if gips, ok := s.config.Groups[g]; ok {
							ips = append(ips, gips...)
						}
					}
					if len(ips) > 0 {
						copied := make([]string, len(ips))
						copy(copied, ips)
						ifaceToIPs[iface] = copied
					}
				}
			}
			s.RUnlock()
		}
	}

	// Best-effort: drop configured VIPs without taking the server lock to avoid deadlocks during shutdown
	for iface, ips := range ifaceToIPs {
		for _, ip := range ips {
			_ = network.BringIPdown(iface, ip)
		}
	}

	// Stop background workers first (no server lock needed)
	if s.healthCheck != nil {
		s.healthCheck.Stop()
	}
	if s.ipMonitor != nil {
		s.ipMonitor.Stop()
	}
	if s.quorumManager != nil {
		s.quorumManager.Stop()
	}

	// Close all peer connections to prevent goroutine leak
	s.closePeerClients()

	// Swap out gRPC servers under a short lock, then stop outside lock to avoid deadlocks
	var oldSrv *grpc.Server
	var oldCli *grpc.Server
	s.Lock()
	oldSrv = s.grpcServer
	oldCli = s.cliServer
	s.grpcServer = nil
	s.cliServer = nil
	s.Unlock()
	if oldSrv != nil {
		oldSrv.GracefulStop()
	}
	if oldCli != nil {
		oldCli.GracefulStop()
	}

	// Remove the CLI Unix socket
	if s.cliSocketPath != "" {
		os.Remove(s.cliSocketPath)
	}

	s.logger.Info("PulseHA server stopped")
}

// loadInitialMembers loads members from config into the member list
// loadInitialMembers builds the member list from a config snapshot.
//
// The config is a parameter rather than a field read, because the callers do not
// agree about locking: Start holds the write lock across its call, while
// ConfigSync, HandleNodeJoin, ResyncNetwork and InitiateJoin do not. Reading
// s.config here therefore raced Reconfigure's pointer swap, which this branch's
// own TestConfigSyncFullConfigAdoptsTheEpochAndItsLeader caught -- it drives a
// ConfigSync whose async reconfigure swaps the config while this function is
// still walking it, and it failed 9 runs in 60 under -race on Linux.
//
// Snapshotting inside would have deadlocked rather than fixed it: currentConfig
// takes the read lock and Start already holds the write lock. So each caller
// passes the snapshot it is entitled to read, which is the only form of this that
// is correct from all five.
func (s *Server) loadInitialMembers(cfg *config.Config) error {
	s.logger.Info("Beginning initial member loading process...")

	if cfg == nil {
		s.logger.Error("FATAL: Cannot load members - config is nil!")
		return fmt.Errorf("config is nil")
	}
	s.logger.Debug("Config validation passed")

	if cfg.Nodes == nil {
		s.logger.Info("No nodes section found in config, starting with empty member list")
		return nil
	}
	s.logger.Debug("Nodes section found in config")

	// Deduplicate nodes by hostname and IP:Port to avoid duplicate members lingering after joins
	{
		localID, _ := cfg.GetLocalNodeUUID()
		seenByHost := make(map[string]string)
		seenByEP := make(map[string]string)
		var toDelete []string
		for id, n := range cfg.Nodes {
			if n == nil {
				continue
			}
			// Prefer non-placeholder IDs over placeholder 'peer'
			isPlaceholder := id == "peer" || id == "" || id == n.Hostname
			ep := fmt.Sprintf("%s:%s", utils.FormatIPv6(n.IP), n.Port)
			if prev, ok := seenByHost[n.Hostname]; ok && prev != id {
				// Decide which to delete: drop placeholder or non-local duplicate
				if isPlaceholder || id != localID {
					toDelete = append(toDelete, id)
					s.logger.Warn("Removing duplicate node by hostname", "hostname", n.Hostname, "duplicate_id", id, "kept_id", prev)
				} else {
					toDelete = append(toDelete, prev)
					s.logger.Warn("Removing duplicate node by hostname", "hostname", n.Hostname, "duplicate_id", prev, "kept_id", id)
					seenByHost[n.Hostname] = id
				}
			} else {
				seenByHost[n.Hostname] = id
			}
			if prev, ok := seenByEP[ep]; ok && prev != id {
				if isPlaceholder || id != localID {
					toDelete = append(toDelete, id)
					s.logger.Warn("Removing duplicate node by endpoint", "endpoint", ep, "duplicate_id", id, "kept_id", prev)
				} else {
					toDelete = append(toDelete, prev)
					s.logger.Warn("Removing duplicate node by endpoint", "endpoint", ep, "duplicate_id", prev, "kept_id", id)
					seenByEP[ep] = id
				}
			} else {
				seenByEP[ep] = id
			}
		}
		if len(toDelete) > 0 {
			for _, id := range toDelete {
				delete(cfg.Nodes, id)
				_ = s.memberList.RemoveMember(id)
			}
			_ = cfg.Save()
		}
	}

	nodeCount := len(cfg.Nodes)
	s.logger.Infof("Found %d node(s) in configuration", nodeCount)

	// Log the actual nodes found
	s.logger.Info("Nodes in configuration:")
	for id, node := range cfg.Nodes {
		s.logger.Infof("  - %s (IP: %s, Port: %s)", id, node.IP, node.Port)
	}

	for id, node := range cfg.Nodes {
		s.logger.Infof("Processing node: %s", id)

		// Check if member already exists
		if existingMember := s.memberList.GetMemberByID(id); existingMember != nil {
			s.logger.Debugf("Member %s already exists in member list, skipping", id)
			continue
		}

		if err := s.memberList.AddMember(id, node.Hostname, node.IP, node.Port); err != nil {
			s.logger.Error("FATAL: Failed to add member", "id", id, "error", err)
			return fmt.Errorf("failed to add member %s: %v", id, err)
		}
		s.logger.Infof("Successfully added member: %s", id)

		// Get the member we just added
		if member := s.memberList.GetMemberByID(id); member != nil {
			s.logger.Debugf("Verified member %s exists in member list", id)

			// Set member details from config. Under the member lock: the member
			// is already in the list, so a health-check pass can be reading
			// Hostname to log against and IP/Port to dial while this writes them.
			member.SetEndpoint(node.IP, node.Port, node.Hostname)

			// Determine initial status based on node role and cluster state
			localNodeID, err := cfg.GetLocalNodeUUID()
			isLocalNode := err == nil && id == localNodeID

			// Decided first, written once. These branches used to assign
			// member.Status and member.ActiveIPs directly, with no lock at all,
			// while the health check loop and the IP monitor read both -- the
			// residual #48 left open.
			//
			// Default to Unknown for all nodes initially; health checks will
			// determine the actual status.
			initial := membership.Claim{Status: membership.StatusUnknown}
			var reason string
			switch {
			case !isLocalNode:
				// Remote nodes start as Unknown until health checks establish connection
				reason = "Setting remote node %s as Unknown (will be determined via health checks)"
			case node.Maintenance:
				// Restore maintenance state across restarts before applying any other default
				initial.Status = membership.StatusMaintenance
				reason = "Restoring local node %s to Maintenance (persisted in config)"
			case cfg.Pulse.Mode == "active-passive" && len(cfg.Nodes) == 1:
				// Single node cluster - should be active
				initial.Status = membership.StatusActive
				reason = "Setting single node %s as Active"
			case cfg.Pulse.Mode == "active-passive":
				// Multi-node cluster - start as passive, election will determine active
				initial.Status = membership.StatusPassive
				reason = "Setting local node %s as Passive (election will determine active)"
			default:
				// Active-active mode - start as passive
				initial.Status = membership.StatusPassive
				reason = "Setting local node %s as Passive in active-active mode"
			}
			// A zero Claim carries no addresses, so the branches that used to
			// clear ActiveIPs still do. The three that left them alone are
			// unaffected, and the reason is the guard at the top of this loop:
			// a member already in the list is skipped with `continue`, so
			// everything reaching here was just created by AddMember and holds
			// nothing. loadInitialMembers runs again after every full ConfigSync,
			// so that skip is what makes writing a whole claim here safe.
			member.SetClaim(initial)
			s.logger.Infof(reason, id)

			// No longer try to determine status based on node order
			// Let the health checks and election process handle this

			ip, port, hostname := member.Endpoint()
			s.logger.Debugf("Set initial details for member %s: IP=%s, Port=%s, Hostname=%s, Status=%s",
				id, ip, port, hostname, membership.StatusToString(member.GetStatus()))
		} else {
			s.logger.Warnf("Member %s was not found in member list after adding!", id)
		}
	}

	s.logger.Info("All members loaded successfully from configuration")
	s.logger.Debugf("Final member list contains %d members", s.memberList.GetMemberCount())

	// After members are loaded, perform one-shot VIP reconcile on local node.
	// The config half of the decision is taken here, synchronously; see
	// snapshotVIPGroups for why it cannot wait for the pass.
	//
	// Scheduled rather than spawned: this function runs on every full ConfigSync,
	// so a burst of mutations used to put one whole-share bring-up — and one
	// whole-share announcement — on every peer per mutation (docs/TEST-PLAN.md
	// defect #65). See vipReconciler.
	if localID, err := cfg.GetLocalNodeUUID(); err == nil {
		groupIPs, activeActive := s.snapshotVIPGroups(cfg, localID)
		s.vipReconcileQueue().Schedule(vipReconcileSnapshot{
			localID:      localID,
			groupIPs:     groupIPs,
			activeActive: activeActive,
		})
	}

	return nil
}

// snapshotVIPGroups captures the config half of the post-load VIP reconcile:
// whether the cluster is active-active, and every floating IP configured on
// each of the local node's interfaces.
//
// It is taken synchronously, before the reconcile goroutine sleeps, because
// ConfigSync also spawns Reconfigure() -> config.Reload(), which unmarshals a
// freshly read file straight over the live *Config. Touching s.config after the
// sleep is a data race against that rewrite — the config the reconcile is for
// is the one loaded here, so read it here.
func (s *Server) snapshotVIPGroups(cfg *config.Config, localID string) (map[string][]string, bool) {
	node := cfg.Nodes[localID]
	if node == nil {
		return nil, false
	}

	groupIPs := make(map[string][]string, len(node.IPGroups))
	for iface, groups := range node.IPGroups {
		var ips []string
		for _, g := range groups {
			if gips, ok := cfg.Groups[g]; ok {
				ips = append(ips, gips...)
			}
		}
		if len(ips) > 0 {
			groupIPs[iface] = ips
		}
	}
	return groupIPs, cfg.Pulse.Mode == "active-active"
}

// reconcileVIPPlan turns that snapshot into what the local node should do now:
// iface -> addresses, and whether they are to be claimed or released. Member
// state is read here rather than in the snapshot because ConfigSync applies the
// incoming statuses and assignments after loadInitialMembers returns — reading
// them early would act on the pre-sync view.
//
// The claim set is mode-aware. In active-passive the Active node owns every IP
// of every group mapped to the interface; in active-active the group is shared,
// so it owns only its assigned subset. Claiming the whole group regardless of
// mode made this a third whole-group claimant alongside the monitor's enforce
// loop and the consolidation path — and it runs 500ms after every full
// ConfigSync, so in active-active each Active peer re-claimed all 201 whitecrane
// addresses and releaseUnassignedIPs had to undo it a tick later
// (docs/TEST-PLAN.md defect #30 — the mode-blindness of #2/#26 at a site those
// fixes missed).
//
// The release set stays deliberately whole-group: a node that has just been
// demoted may be holding anything, including addresses it was never assigned,
// and the point of the pass is to leave it holding none.
func (s *Server) reconcileVIPPlan(localID string, groupIPs map[string][]string, activeActive bool) (map[string][]string, bool) {
	member := s.memberList.GetMemberByID(localID)
	if member == nil {
		return nil, false
	}

	// Under the member lock: ConfigSync applies incoming member states
	// concurrently with this reconcile, and reading the status bare raced with
	// that write.
	claim := member.GetStatus() == membership.StatusActive

	if !claim || !activeActive {
		return groupIPs, claim
	}

	plan := make(map[string][]string, len(groupIPs))
	for iface, ips := range groupIPs {
		if mine := s.filterToAssigned(localID, ips); len(mine) > 0 {
			plan[iface] = mine
		}
	}
	return plan, true
}

// HandleNodeJoin processes a new node joining the cluster
func (s *Server) HandleNodeJoin(ctx context.Context, req *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	s.logger.Infof("Handling join request from node: %s", req.Hostname)
	s.logger.Debugf("Join request details - NodeID: %s, BindIP: %s, BindPort: %s, Token provided: %v",
		req.NodeId, req.BindIp, req.BindPort, req.Token != "")

	// Serialize with CreateCluster and InitiateJoin to prevent a TOCTOU race
	// where the tokenless first-node branch below (or two concurrent tokenless
	// Joins) both observe an empty member list and both initialize the cluster.
	s.clusterInitMu.Lock()
	defer s.clusterInitMu.Unlock()

	// Check if this is initial cluster creation
	if s.memberList.GetMemberCount() == 0 && req.Token == "" {
		s.logger.Info("Initializing new cluster with first node: ", req.Hostname)

		// Node ID must be provided
		if req.NodeId == "" {
			return &rpc.JoinResponse{
				Success: false,
				Message: "node_id is required",
			}, nil
		}
		nodeID := req.NodeId
		s.logger.Debugf("Using node_id: %s", nodeID)

		// Add the node to the member list
		if err := s.memberList.AddMember(nodeID, req.Hostname, req.BindIp, req.BindPort); err != nil {
			s.logger.Error("Failed to add member to member list", "error", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: fmt.Sprintf("failed to add member: %v", err),
			}, nil
		}

		// Set the cluster token
		s.config.Pulse.ClusterToken = uuid.New().String()
		s.logger.Debugf("Generated cluster token: %s", s.config.Pulse.ClusterToken)

		// Save the config
		if err := s.config.Save(); err != nil {
			s.logger.Error("Failed to save config", "error", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: fmt.Sprintf("failed to save config: %v", err),
			}, nil
		}

		return &rpc.JoinResponse{
			Success: true,
			NodeId:  nodeID,
			Message: "Successfully initialized new cluster",
		}, nil
	}

	// Validate cluster token for existing cluster
	if os.Getenv("PULSEHA_TEST") != "true" {
		s.logger.Debugf("Validating cluster token for join...")
		clusterToken := s.config.Pulse.ClusterToken // Direct read - config token shouldn't change during join
		s.logger.Debugf("Expected token: %s, Received token: %s", clusterToken, req.Token)

		if req.Token != clusterToken {
			s.logger.Warn("Invalid cluster join token received")
			return &rpc.JoinResponse{
				Success: false,
				Message: "Invalid cluster token",
			}, nil
		}
		s.logger.Debugf("Token validation passed")
	} else {
		s.logger.Debug("PULSEHA_TEST=true: skipping token validation for join")
	}

	// Node ID must be provided
	if req.NodeId == "" {
		return &rpc.JoinResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
	}
	nodeID := req.NodeId
	s.logger.Debugf("Using node_id: %s", nodeID)

	// Add the node to the member list
	s.logger.Debugf("Adding member %s to member list...", nodeID)
	if err := s.memberList.AddMember(nodeID, req.Hostname, req.BindIp, req.BindPort); err != nil {
		s.logger.Error("Failed to add member to member list", "error", err)
		return &rpc.JoinResponse{
			Success: false,
			Message: fmt.Sprintf("failed to add member: %v", err),
		}, nil
	}
	s.logger.Debugf("Member %s added to member list successfully", nodeID)

	// Add node to config (need to lock config access)
	s.logger.Debugf("About to update config...")
	// Use config's lock instead of server's lock to avoid deadlock
	s.config.Lock()
	s.logger.Debugf("Config lock acquired, updating nodes...")
	if s.config.Nodes == nil {
		s.config.Nodes = make(map[string]*config.Node)
	}
	// Deduplicate any existing node with same hostname or IP:Port
	for existingID, existing := range s.config.Nodes {
		if existing == nil {
			continue
		}
		if existing.Hostname == req.Hostname || (existing.IP == req.BindIp && existing.Port == req.BindPort) {
			if existingID != nodeID {
				s.logger.Warn("Join detected duplicate node entry; replacing existing entry", "existing_id", existingID, "hostname", existing.Hostname, "ip", existing.IP, "port", existing.Port, "new_id", nodeID)
				delete(s.config.Nodes, existingID)
				// Also remove from member list to avoid stale member
				_ = s.memberList.RemoveMember(existingID)
			}
		}
	}
	s.config.Nodes[nodeID] = &config.Node{
		Hostname:    req.Hostname,
		IP:          req.BindIp,
		Port:        req.BindPort,
		IPGroups:    make(map[string][]string),
		Maintenance: true,
	}
	s.logger.Debugf("Config updated, releasing config lock...")
	s.config.Unlock()
	s.logger.Debugf("Config lock released")

	s.logger.Infof("Successfully joined node %s (ID: %s) to cluster", req.Hostname, nodeID)

	// Save the config synchronously to ensure it's available for health checks
	s.logger.Debugf("Saving config with new member %s...", nodeID)
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config after successful join", "error", err)
		// Still return success since member was added to memberList
	} else {
		s.logger.Debugf("Config saved successfully after node %s joined", req.Hostname)
		// Defer/best-effort broadcast in a goroutine to avoid blocking join RPC
		s.markConfigDirty()
	}

	// Post-join: ensure health checker is running and member list is initialized promptly (async)
	go func() {
		s.startHealthChecker()
		if err := s.loadInitialMembers(s.currentConfig()); err != nil {
			s.logger.Warn("Join receiver failed to load members immediately", "error", err)
		}
	}()

	// Trigger a quick convergence broadcast asynchronously
	go func() {
		states := getStatusMap()
		membersSnapshot := s.memberList.MembersSnapshot()
		for id, m := range membersSnapshot {
			states[id] = m.GetStatus()
		}
		_ = s.broadcastNextEpoch(states)
		putStatusMap(states)
	}()

	// Marshal the full cluster configuration to send to the joining node
	s.logger.Info("JOIN: Preparing cluster configuration for joining node", "nodeID", nodeID)
	configBytes, err := json.Marshal(s.config)
	if err != nil {
		s.logger.Error("JOIN: Failed to marshal cluster config for join response", "error", err)
		// Still allow join even if config sync fails - it will sync later
		configBytes = nil
	} else {
		s.logger.Info("JOIN: Successfully marshaled cluster config", "configSize", len(configBytes))
		// Log a preview of the config
		var preview map[string]interface{}
		if err := json.Unmarshal(configBytes, &preview); err == nil {
			if nodes, ok := preview["nodes"].(map[string]interface{}); ok {
				s.logger.Info("JOIN: Config contains nodes", "nodeCount", len(nodes))
				for id := range nodes {
					s.logger.Info("JOIN: Config includes node", "nodeID", id)
				}
			}
		}
	}

	s.logger.Info("JOIN: Sending JoinResponse to joining node",
		"success", true,
		"nodeID", nodeID,
		"configIncluded", configBytes != nil,
		"configSize", len(configBytes))

	return &rpc.JoinResponse{
		Success:       true,
		NodeId:        nodeID,
		Message:       "Successfully joined cluster",
		ClusterConfig: configBytes,
	}, nil
}

// HandleNodeLeave handles the node leave RPC call
func (s *Server) HandleNodeLeave(ctx context.Context, req *rpc.LeaveRequest) (*rpc.LeaveResponse, error) {
	// Validate
	if req == nil || req.NodeId == "" {
		return &rpc.LeaveResponse{Success: false, Message: "missing node_id"}, nil
	}
	nodeID := req.NodeId

	// Resolve local node ID
	localNodeID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		return &rpc.LeaveResponse{Success: false, Message: "Unable to get local node: " + err.Error()}, nil
	}

	// Fast path: remote removal (not local)
	if nodeID != localNodeID {
		// Remove from member list and config under lock
		s.Lock()
		if err := s.memberList.RemoveMember(nodeID); err != nil {
			s.Unlock()
			s.logger.Error("Failed to remove member", "error", err)
			return &rpc.LeaveResponse{Success: false, Message: "Failed to remove member: " + err.Error()}, nil
		}
		delete(s.config.Nodes, nodeID)
		s.Unlock()
		return &rpc.LeaveResponse{Success: true, Message: fmt.Sprintf("Successfully removed node %s from the cluster", nodeID)}, nil
	}

	// Local leave: gather peers and VIPs without holding the server lock
	var peers []struct{ ip, port string }
	ifaceToIPs := make(map[string][]string)
	func() {
		s.RLock()
		defer s.RUnlock()
		for id, n := range s.config.Nodes {
			if id == localNodeID || n == nil {
				continue
			}
			peers = append(peers, struct{ ip, port string }{ip: n.IP, port: n.Port})
		}
		if node := s.config.Nodes[localNodeID]; node != nil {
			for iface, groups := range node.IPGroups {
				var ips []string
				for _, g := range groups {
					if gips, ok := s.config.Groups[g]; ok {
						ips = append(ips, gips...)
					}
				}
				if len(ips) > 0 {
					copied := make([]string, len(ips))
					copy(copied, ips)
					ifaceToIPs[iface] = copied
				}
			}
		}
	}()

	// Notify peers best-effort (no server lock held)
	for _, p := range peers {
		remoteClient, err := client.New()
		if err != nil {
			continue
		}
		defer remoteClient.Close()
		if err := remoteClient.Connect(p.ip, p.port, false); err != nil {
			continue
		}
		pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = remoteClient.Server().Remove(pctx, &rpc.RemoveRequest{NodeId: localNodeID})
		pcancel()
	}

	// Tear down VIPs best-effort directly via network package to avoid nested locks
	for iface, ips := range ifaceToIPs {
		for _, ip := range ips {
			_ = network.BringIPdown(iface, ip)
		}
	}

	// Now mutate local state under lock
	s.Lock()
	// Clear member list
	s.memberList.Clear()
	// Wipe cluster configuration
	s.config.Nodes = make(map[string]*config.Node)
	s.config.Groups = make(map[string][]string)
	s.config.Pulse.LocalNode = ""
	s.config.Pulse.ClusterToken = ""
	_ = s.config.Save()
	// Stop cluster gRPC; keep CLI alive
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
		s.grpcServer = nil
	}
	if s.healthCheck != nil {
		s.healthCheck.Stop()
	}
	s.Unlock()

	return &rpc.LeaveResponse{Success: true, Message: "Successfully left the cluster"}, nil
}

// deriveMemberStatus maps a member's stored health, plus how many floating IPs
// it is recorded as holding, to the status reported to an operator.
//
// Active carried two independent facts — "this daemon is healthy and eligible"
// and "this node is serving floating IPs" — which came apart the moment
// active-active existed. Standby separates them for display: healthy, eligible
// for promotion, serving nothing.
//
// Standby is an active-active status and nothing else, which makes Active
// mode-relative: in active-passive it means "elected", in active-active it means
// "serving at least one address" (docs/adr/0001-standby-is-active-active-only.md).
// The alternative was worse. This used to trust a node's record of itself in
// either mode, so in active-passive the only row that could read Standby was the
// row you happened to be asking about: a healthy elected node holding nothing
// called itself Standby while its peer called it Active, and both were answering
// from the same healthy cluster (END-2289). A status an operator cannot compare
// between two nodes is not a status.
//
// Tenancy is derived here and nowhere else. It is not stored on the member, not
// put on the wire between nodes, and not consulted by any placement or demotion
// decision, because a stored copy would be the #1/#21 defect with a new name:
// MakePassive returned success having released nothing, since ActiveIPs was
// empty on a node that in fact held every address.
//
// selfReportsAssignments is the reason this takes three arguments rather than
// two. An empty assignment list only means "holds nothing" where the list is
// knowledge; elsewhere it means "this node does not know". Peers self-report
// their hosted IPs over ConfigSync in active-active only (see the sender at the
// buildFullConfigPayload self-report and its application in ConfigSync), so
// every member's list is current in that mode and no member's list is in the
// other. In active-passive there is no self-report and a node promoted by
// election "holds them all while its ActiveIPs is still empty"
// (internal/membership/health_check.go) — reporting that node as Standby would
// be the most misleading answer available.
func deriveMemberStatus(status membership.MemberStatus, assignedIPs int, selfReportsAssignments bool) rpc.MemberStatusEnum {
	switch status {
	case membership.StatusActive:
		if assignedIPs == 0 && selfReportsAssignments {
			return rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY
		}
		return rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE
	case membership.StatusPassive:
		return rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE
	case membership.StatusMaintenance:
		return rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE
	default:
		return rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN
	}
}

// GetClusterStatus returns the current status of all nodes
func (s *Server) GetClusterStatus(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	s.RLock()
	defer s.RUnlock()

	// Read once rather than per member: this loop runs for every node in the
	// cluster and the answer is the same for all of them.
	selfReportsAssignments := s.config.Pulse.Mode == "active-active"

	var members []*rpc.Member
	membersSnapshot := s.memberList.MembersSnapshot()
	for _, member := range membersSnapshot {
		health := member.GetHealthStatus()
		st := deriveMemberStatus(health.Status, len(health.ActiveIPs), selfReportsAssignments)

		// The stored time, not time.Now(). This reported "now" for any member
		// that had ever responded, so a node unreachable for days showed a last
		// response of a moment ago — the one field an operator would use to tell
		// a blip from a corpse, carrying no information at all.
		lastResp := ""
		if !health.LastResponse.IsZero() {
			lastResp = health.LastResponse.Format(time.RFC3339)
		}

		ip, port, _ := member.Endpoint()
		members = append(members, &rpc.Member{
			Hostname:     health.Hostname,
			Status:       st,
			ActiveIps:    health.ActiveIPs,
			LastResponse: lastResp,
			Latency:      health.Latency,
			Ip:           ip,
			Port:         port,
			NodeId:       member.ID,
		})
	}

	// Build groups information from server config so clients don't need local config
	var groups []*rpc.GroupInfo
	for groupName, ips := range s.config.Groups {
		group := &rpc.GroupInfo{
			Name: groupName,
			Ips:  ips,
		}

		// Find assignments for this group
		for id, node := range s.config.Nodes {
			if id == "peer" || (node != nil && node.Hostname == "peer") {
				continue
			}
			for iface, assignedGroups := range node.IPGroups {
				for _, g := range assignedGroups {
					if g == groupName {
						group.Assignments = append(group.Assignments, &rpc.GroupAssignment{
							Interface: iface,
							NodeId:    id,
						})
					}
				}
			}
		}

		groups = append(groups, group)
	}

	return &rpc.StatusResponse{
		Members: members,
		Groups:  groups,
		Mode:    s.config.Pulse.Mode,
	}, nil
}

// Status implements the CLI.Status RPC method
func (s *Server) Status(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	return s.GetClusterStatus(ctx, req)
}

// Join handles the CLI Join RPC call
func (s *Server) Join(ctx context.Context, req *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	s.logger.Info("Received CLI Join request", "hostname", req.Hostname, "tokenProvided", req.Token != "")

	resp, err := s.HandleNodeJoin(ctx, req)
	if err != nil {
		s.logger.Error("CLI Join request failed", "error", err)
	} else {
		s.logger.Info("CLI Join request completed", "success", resp.Success, "message", resp.Message)
	}
	return resp, err
}

// Leave handles the CLI Leave RPC call
func (s *Server) Leave(ctx context.Context, req *rpc.LeaveRequest) (*rpc.LeaveResponse, error) {
	s.logger.Info("Received CLI Leave request", "node_id", req.NodeId)

	if !s.config.ClusterCheck() {
		return &rpc.LeaveResponse{Success: false, Message: "no cluster configured; nothing to leave"}, nil
	}

	// If no node_id provided, default to local node
	if req.NodeId == "" {
		if id, err := s.config.GetLocalNodeUUID(); err == nil {
			req.NodeId = id
		} else {
			return &rpc.LeaveResponse{Success: false, Message: "Unable to determine local node: " + err.Error()}, nil
		}
	}

	// Get the member
	member := s.memberList.GetMemberByID(req.NodeId)
	if member == nil {
		return &rpc.LeaveResponse{
			Success: false,
			Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
		}, nil
	}

	// We can't leave ourself from a cluster
	localNodeID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		return &rpc.LeaveResponse{
			Success: false,
			Message: "Unable to get local node: " + err.Error(),
		}, nil
	}

	// If this is the local node, we need to stop the server
	if member.ID == localNodeID {
		s.logger.Info("Leaving cluster as local node")

		// Snapshot peers and VIPs without holding the server lock
		var peers []struct {
			id   string
			ip   string
			port string
		}
		ifaceToIPs := make(map[string][]string)
		func() {
			s.RLock()
			defer s.RUnlock()
			for id, node := range s.config.Nodes {
				if id == localNodeID || node == nil {
					continue
				}
				peers = append(peers, struct {
					id   string
					ip   string
					port string
				}{id: id, ip: node.IP, port: node.Port})
			}
			if node := s.config.Nodes[localNodeID]; node != nil {
				for iface, groups := range node.IPGroups {
					var ips []string
					for _, g := range groups {
						if gips, ok := s.config.Groups[g]; ok {
							ips = append(ips, gips...)
						}
					}
					if len(ips) > 0 {
						copied := make([]string, len(ips))
						copy(copied, ips)
						ifaceToIPs[iface] = copied
					}
				}
			}
		}()

		// Deterministic removal: contact a peer to coordinate the cluster-wide removal
		// This ensures all nodes are updated before we leave
		var coordinated bool
		var lastErr error

		// If we're the last node in the cluster, no coordination is needed
		if len(peers) == 0 {
			s.logger.Info("Last node in cluster - no coordination needed")
			coordinated = true
		} else {
			for _, peer := range peers {
				s.logger.Info("Requesting cluster-coordinated removal", "coordinator", peer.id)
				remoteClient, err := client.New()
				if err != nil {
					s.logger.Warn("Failed to create client for coordinator", "peer", peer.id, "error", err)
					lastErr = err
					continue
				}
				if err := remoteClient.Connect(peer.ip, peer.port, false); err != nil {
					s.logger.Warn("Failed to connect to coordinator", "peer", peer.id, "error", err)
					remoteClient.Close()
					lastErr = err
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				resp, err := remoteClient.Server().CoordinateRemoval(ctx, &rpc.CoordinateRemovalRequest{NodeId: localNodeID})
				cancel()
				remoteClient.Close()

				if err != nil {
					s.logger.Warn("Failed to coordinate removal with peer", "peer", peer.id, "error", err)
					lastErr = err
					continue
				}

				if !resp.Success {
					s.logger.Warn("Peer rejected removal coordination", "peer", peer.id, "message", resp.Message)
					lastErr = fmt.Errorf("removal rejected: %s", resp.Message)
					continue
				}

				s.logger.Info("Cluster-coordinated removal successful", "coordinator", peer.id, "updated_nodes", resp.UpdatedNodes)
				coordinated = true
				break
			}
		}

		// If coordination failed with all peers, fail the leave operation
		if !coordinated {
			if lastErr != nil {
				return &rpc.LeaveResponse{
					Success: false,
					Message: fmt.Sprintf("Failed to coordinate cluster removal: %v", lastErr),
				}, nil
			}
			return &rpc.LeaveResponse{
				Success: false,
				Message: "No peers available to coordinate removal",
			}, nil
		}

		// Stop background workers after successful coordination
		if s.healthCheck != nil {
			s.healthCheck.Stop()
		}
		if s.ipMonitor != nil {
			s.ipMonitor.Stop()
		}

		// Best-effort: drop all configured VIPs on local node directly via network package
		for iface, ips := range ifaceToIPs {
			for _, ip := range ips {
				_ = network.BringIPdown(iface, ip)
			}
		}

		// Remove all members from member list locally (leave into clean, non-clustered state)
		s.memberList.Clear()

		// Wipe local cluster configuration (nodes and groups) and clear local
		// identifiers, under the server lock.
		//
		// These four ran unguarded. Worth being precise about why, because a
		// flow-insensitive read of this function suggests otherwise: the
		// s.RLock() above is inside a func(){...}() whose deferred unlock fires
		// when that closure returns, so by here nothing is held at all.
		//
		// s.Lock() rather than s.config.Lock(), for local consistency: the
		// closure above reads s.config.Nodes under the server lock, so the read
		// and the write in this one function now use the same mutex. Which lock
		// owns the config's contents cluster-wide is genuinely undecided --
		// thirty-seven writes across four incompatible disciplines -- and is its
		// own ticket rather than something to settle in passing here.
		//
		// Scoped to the four assignments. Save() writes to disk and takes the
		// config's own lock; holding the server lock across it would stall every
		// other operation on this daemon for a file write, which is #4/#8's
		// lesson.
		s.Lock()
		s.config.Nodes = make(map[string]*config.Node)
		s.config.Groups = make(map[string][]string)
		s.config.Pulse.LocalNode = ""
		s.config.Pulse.ClusterToken = ""
		s.Unlock()

		if err := s.config.Save(); err != nil {
			s.logger.Warn("Failed to save config after leave", "error", err)
		}

		// Stop the cluster (inter-node) gRPC server only; keep CLI server alive for further config
		if s.grpcServer != nil {
			s.grpcServer.GracefulStop()
			s.grpcServer = nil
		}

		return &rpc.LeaveResponse{
			Success: true,
			Message: "Successfully left the cluster",
		}, nil
	}

	// Remote node removal - use CoordinateRemoval for cluster-wide consistency
	s.logger.Info("Removing remote node via coordinated removal", "node_id", member.ID, "hostname", member.Hostname)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Call CoordinateRemoval locally (this node becomes the coordinator)
	resp, err := s.CoordinateRemoval(ctx, &rpc.CoordinateRemovalRequest{NodeId: member.ID})
	if err != nil {
		return &rpc.LeaveResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to coordinate removal: %v", err),
		}, nil
	}

	if !resp.Success {
		return &rpc.LeaveResponse{
			Success: false,
			Message: resp.Message,
		}, nil
	}

	return &rpc.LeaveResponse{
		Success: true,
		Message: resp.Message,
	}, nil
}

// Promote handles the CLI Promote RPC call
func (s *Server) Promote(ctx context.Context, req *rpc.PromoteRequest) (*rpc.PromoteResponse, error) {
	localNodeID, _ := s.config.GetLocalNodeUUID()
	s.logger.Info("PROMOTE: Received promote request",
		"target_node", req.NodeId,
		"local_node", localNodeID,
		"force_demote", req.GetForceDemote())

	// Fast-fail validation checks
	if !s.config.ClusterCheck() {
		s.logger.Warn("PROMOTE: Rejecting - no cluster configured")
		return &rpc.PromoteResponse{
			Success: false,
			Message: "no cluster configured",
		}, nil
	}

	if req.NodeId == "" {
		s.logger.Warn("PROMOTE: Rejecting - node_id is required")
		return &rpc.PromoteResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
	}

	// Verify target node exists
	member := s.memberList.GetMemberByID(req.NodeId)
	if member == nil {
		s.logger.Warn("PROMOTE: Rejecting - node not found", "node_id", req.NodeId)
		return &rpc.PromoteResponse{
			Success: false,
			Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
		}, nil
	}

	// Check if already active
	if member.GetStatus() == membership.StatusActive {
		s.logger.Info("PROMOTE: Node is already active", "node_id", req.NodeId)
		return &rpc.PromoteResponse{
			Success: true,
			Message: fmt.Sprintf("Node %s is already active", req.NodeId),
		}, nil
	}

	// All validation passed - kick off async promotion
	s.logger.Info("PROMOTE: Validation passed, starting asynchronous promotion",
		"target_node", req.NodeId,
		"target_hostname", member.Hostname)

	go s.performPromotionAsync(req.NodeId, req.Ips, req.GetForceDemote())

	// Return immediately - actual work happens asynchronously
	// Frontend will poll Status to see the final result
	return &rpc.PromoteResponse{
		Success: true,
		Message: fmt.Sprintf("Promotion of node %s initiated - check status for completion", req.NodeId),
	}, nil
}

// ipPresenceFunc reports whether an address is currently on a local interface.
type ipPresenceFunc func(string) (bool, string, error)

// filterStillHeld returns those of ips that present reports as still on an interface.
//
// An address whose presence cannot be determined is reported as still held. This is the
// conservative direction: the result gates whether another node may claim these addresses,
// so a false "it's gone" dual-homes the group, while a false "still there" only declines a
// promotion the incumbent is already serving.
func filterStillHeld(ips []string, present ipPresenceFunc) []string {
	remaining := make([]string, 0)
	for _, ip := range ips {
		exists, _, err := present(ip)
		if err != nil || exists {
			remaining = append(remaining, ip)
		}
	}
	return remaining
}

// stillHeldIPs returns those of ips still present on a local interface, or an error if that
// could not be established at all.
//
// The interface inventory is built once for the whole set rather than per address: this runs
// with the full floating-IP group as input, so a per-address rebuild meant hundreds of full
// netlink enumerations per demotion. A failure to build it is returned as an error rather than
// folded into the result, so the caller can distinguish "these are still up" from "I could not
// look" instead of silently reporting every address as held.
func stillHeldIPs(ips []string) ([]string, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	inv, err := network.BuildIPInventory()
	if err != nil {
		return nil, err
	}
	return filterStillHeld(ips, inv.Exists), nil
}

// confirmPeerReleasedIPs asks an unreachable peer to release its floating IPs and reports
// what could actually be established about it.
//
// This deliberately does NOT go through s.MakePassive. That method flattens every remote
// failure into (&Response{Success: false}, nil) — a nil error — so a caller cannot tell a
// refused connection from a wedged peer from a successful demotion. Promotion safety turns
// entirely on that distinction, so the RPC is issued directly and the gRPC status preserved.
//
// Returns:
//   - released:     the peer answered and confirmed it is now Passive with its IPs down.
//   - provablyDown: the transport itself failed in a way that means no daemon is listening.
//     Anything indeterminate (deadline, local fault, a peer that answers but
//     declines) is reported as NOT provably down, so the caller stays conservative.
func (s *Server) confirmPeerReleasedIPs(ctx context.Context, nodeID string) (released bool, provablyDown bool, err error) {
	node := s.config.Nodes[nodeID]
	if node == nil {
		return false, false, fmt.Errorf("no configuration for node %s", nodeID)
	}

	remoteClient, err := client.New()
	if err != nil {
		// A local fault tells us nothing about the peer. Never read it as "the peer is gone".
		return false, false, fmt.Errorf("failed to create client for %s: %w", nodeID, err)
	}
	defer remoteClient.Close()

	if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
		// Only a malformed target reaches here — grpc.NewClient does not dial — so this is a
		// local fault, not evidence about the peer.
		return false, false, fmt.Errorf("failed to create connection to %s: %w", nodeID, err)
	}

	// Prove the socket is reachable before asking anything of it.
	//
	// Without this the two failures that decide a promotion are the same error. A peer that
	// refuses the connection answers Unavailable; a peer whose packets are being dropped —
	// a dead switch port, a powered-off node, a firewall DROP — leaves the call waiting for a
	// transport that never arrives, which gRPC reports as DeadlineExceeded, and that is the
	// identical code a peer returns when it accepts the call and then hangs. Reading the
	// second as the first aborted every failover away from a blackholed node:
	// `Aborting promotion - cannot confirm unreachable node released its floating IPs
	// peer_still_alive=true have_quorum=true`, repeating for as long as the partition lasted.
	// The symptom is a floating IP that stays dark for as long as the node stays dead, and it
	// is not limited to two-node clusters — peerStillAlive short-circuits ahead of the quorum
	// check, so a majority cannot override it at any cluster size.
	//
	// Only `systemctl stop` hid this: a closed port refuses, which is Unavailable, so every
	// failover this project has verified live took the working branch. A node that loses power
	// or a link that starts dropping produces no refusal and took the other one.
	//
	// A bare TCP dial, deliberately, and NOT gRPC's own connectivity state. Waiting for a
	// ClientConn to reach READY reports the HTTP/2 connection, which a daemon wedged before it
	// can send its SETTINGS frame never completes — so readiness would classify a wedged-but-
	// live Active as gone and promote over a node still holding every address, which is TC-6
	// reintroduced through a new door. The kernel accepts on behalf of a wedged process, so
	// the socket answering is exactly the signal that keeps that case safe while the blackhole
	// case gets fixed. It is also the probe checkNodeConnectivity already uses to decide the
	// same question.
	dialAddr := net.JoinHostPort(utils.SanitizeIPv6(node.IP), node.Port)
	probe, dialErr := net.DialTimeout("tcp", dialAddr, transportProbeTimeout)
	if dialErr != nil {
		// Nothing is listening, or nothing can reach it. Either way no daemon is holding those
		// addresses. This is also what a network partition looks like from here, which is why
		// the caller still requires quorum before acting on it.
		return false, true, fmt.Errorf("no daemon reachable at %s: %w", dialAddr, dialErr)
	}
	_ = probe.Close()

	resp, err := remoteClient.Server().MakePassive(ctx, &rpc.MakePassiveRequest{NodeId: nodeID})
	if err != nil {
		// The socket answered a moment ago, so a deadline here genuinely means the peer took
		// the call and never finished — alive, and still owning its IPs. Unavailable means the
		// transport dropped under the call; the daemon is not serving.
		return false, status.Code(err) == codes.Unavailable, err
	}
	if !resp.Success {
		// The peer is alive enough to answer and told us it did not demote.
		return false, false, fmt.Errorf("peer %s declined to release: %s", nodeID, resp.Message)
	}
	return true, false, nil
}

// transportProbeTimeout bounds how long confirmPeerReleasedIPs waits for a TCP connection to
// the peer before concluding nothing is there.
//
// Sized for a kernel handshake on a healthy segment, not for an application response — that
// separation is the whole point of probing the socket rather than the RPC, since a loaded peer
// still completes a handshake in milliseconds while its daemon may take much longer to answer.
// Long enough to absorb a retransmit or two; short enough that a dead node does not hold a
// promotion open for the length of TCP's own SYN backoff, which is what made the blackhole
// case look like a hang rather than a failure.
//
// Six times checkNodeConnectivity's 500ms. That probe runs every health-check tick and can be
// wrong cheaply — the next tick corrects it. This one runs once, and being wrong here either
// strands a floating IP or dual-homes it, so it buys margin against a transient loss that
// probe would simply retry past.
//
// Deliberately NOT derived from the group size the way DemotionTimeoutFor is: the number of
// addresses a peer has to release affects how long it takes to ANSWER, never how long it takes
// to accept a connection.
const transportProbeTimeout = 3 * time.Second

// canPromoteWithoutConfirmedRelease decides whether a promotion may claim the floating IPs
// when the previous Active is unreachable and its release could not be confirmed.
//
//   - peerStillAlive: the peer could not be proven down — it may be wedged but running, and
//   - peerStillAlive: the peer could not be proven down — it may be wedged but running, and
//     still own every floating IP. Nothing overrides this, including forceDemote.
//   - haveQuorum: this node is on the majority side. A minority must never claim addresses it
//     cannot prove were released, or both sides of a partition serve the same IPs.
//
// forceDemote deliberately does NOT override a live peer. It cannot serve as an operator escape
// here because HealthChecker.tryForcePromote sets it on every promotion the automatic election
// drives, so honouring it disabled this check for precisely the case it exists to catch — an
// election promoting over an Active wedged by SetMode (docs/TEST-PLAN.md TC-6). The operator
// escape for a permanently wedged Active is to stop its daemon, which makes it provably down.
func canPromoteWithoutConfirmedRelease(peerStillAlive, haveQuorum, forceDemote bool) bool {
	if peerStillAlive {
		return false
	}
	// The peer is provably down: promote from the majority side, or when explicitly forced.
	return haveQuorum || forceDemote
}

// demotionTimeout sizes a MakePassive deadline to the number of floating IPs the
// target has to release and verify — see membership.DemotionTimeoutFor.
func (s *Server) demotionTimeout() time.Duration {
	s.RLock()
	total := 0
	for _, ips := range s.config.Groups {
		total += len(ips)
	}
	s.RUnlock()

	return membership.DemotionTimeoutFor(total)
}

// remoteDemotionContext bounds the MakePassive forwarded to the node that
// actually has to release and verify the addresses.
//
// Derived from the caller's context, so whichever deadline is sooner wins: a
// caller that already sized one keeps it, and a caller with none gets the group-
// sized bound rather than an unbounded wait on an unresponsive peer.
func (s *Server) remoteDemotionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.demotionTimeout())
}

// restoreMemberStates puts every member back to the status it held before a
// promotion attempt began.
//
// Each write goes through SetStatus rather than assigning the field: the health
// check loop, the IP monitor and the status RPC all read Status under the member
// lock, and this runs on the promotion goroutine alongside them.
func (s *Server) restoreMemberStates(originalStates map[string]membership.MemberStatus) {
	for id, status := range originalStates {
		if mm := s.memberList.GetMemberByID(id); mm != nil {
			mm.SetStatus(status)
		}
	}
}

// performPromotionAsync executes the promotion operation asynchronously
// This prevents frontend timeouts on long-running IP failover operations
func (s *Server) performPromotionAsync(targetNodeID string, ips []string, forceDemote bool) {
	startTime := time.Now()
	s.logger.Info("PROMOTE_ASYNC: Starting asynchronous promotion",
		"target_node", targetNodeID,
		"force_demote", forceDemote,
		"ip_count", len(ips))

	localNodeID, _ := s.config.GetLocalNodeUUID()

	// Identify current active (if any)
	prevActiveID := ""
	// Nodes whose release of the floating IPs we cannot confirm. A member marked Unknown is
	// unreachable, which is NOT the same as known-idle — it may still hold every group IP.
	// Promoting over one of these without confirming the release dual-homes the whole group
	// (see docs/TEST-PLAN.md TC-6). Only populated when no reachable Active is present.
	unconfirmedIncumbents := make([]string, 0, 1)
	reachableCount := 0
	if s.config.Pulse.Mode == "active-passive" {
		for id, m := range s.memberList.MembersSnapshot() {
			status := m.GetStatus()
			if status != membership.StatusUnknown {
				reachableCount++
			}
			if status == membership.StatusActive && prevActiveID == "" {
				prevActiveID = id
				s.logger.Info("PROMOTE_ASYNC: Found current active node", "active_node", id, "hostname", m.Hostname)
			}
		}
		if prevActiveID == "" {
			for id, m := range s.memberList.MembersSnapshot() {
				if id != targetNodeID && m.GetStatus() == membership.StatusUnknown {
					unconfirmedIncumbents = append(unconfirmedIncumbents, id)
				}
			}
		}
	}
	if prevActiveID == "" {
		s.logger.Info("PROMOTE_ASYNC: No active node found in cluster",
			"unconfirmed_incumbents", len(unconfirmedIncumbents),
			"reachable_nodes", reachableCount)
	}

	// Get the member
	member := s.memberList.GetMemberByID(targetNodeID)
	if member == nil {
		s.logger.Error("PROMOTE_ASYNC: Target node not found", "node_id", targetNodeID)
		return
	}

	// Snapshot current states for rollback
	originalStates := make(map[string]membership.MemberStatus)
	for id, m := range s.memberList.MembersSnapshot() {
		originalStates[id] = m.GetStatus()
	}

	// Determine demotion strategy
	shouldDemote := s.config.Pulse.Mode == "active-passive" && prevActiveID != "" && prevActiveID != targetNodeID
	isTargetNodePromotion := member.IsLocal() && prevActiveID != localNodeID

	s.logger.Info("PROMOTE_ASYNC: Demotion decision",
		"shouldDemote", shouldDemote,
		"isTargetNodePromotion", isTargetNodePromotion,
		"target_is_local", member.IsLocal(),
		"prev_active", prevActiveID,
		"target", targetNodeID)

	// Step 1: Demote previous active if needed
	demotionFailed := false
	if shouldDemote && !isTargetNodePromotion {
		s.logger.Info("PROMOTE_ASYNC: Demoting current active before promotion",
			"previous_active", prevActiveID,
			"new_active", targetNodeID)

		if _, err := s.MakePassive(context.Background(), &rpc.MakePassiveRequest{NodeId: prevActiveID}); err != nil {
			s.logger.Error("PROMOTE_ASYNC: Failed to demote previous active",
				"previous_active", prevActiveID,
				"error", err)

			if !forceDemote {
				// Abort and restore
				s.logger.Warn("PROMOTE_ASYNC: Aborting promotion due to demotion failure")
				s.restoreMemberStates(originalStates)
				_ = s.broadcastNextEpoch(originalStates)
				return
			}

			demotionFailed = true
			s.logger.Warn("PROMOTE_ASYNC: Continuing with promotion despite demotion failure (force_demote=true)",
				"previous_active", prevActiveID)

			if failingMember := s.memberList.GetMemberByID(prevActiveID); failingMember != nil {
				failingMember.MarkUnreachable()
			}
		} else {
			s.logger.Info("PROMOTE_ASYNC: Successfully demoted previous active", "previous_active", prevActiveID)
		}
	} else if shouldDemote && isTargetNodePromotion {
		s.logger.Info("PROMOTE_ASYNC: Skipping demotion (remote-initiated promotion)",
			"previous_active", prevActiveID,
			"new_active", targetNodeID)
	}

	// Step 1b: There is no reachable Active, but an unreachable peer may still be holding the
	// entire group. Try to confirm it has released before claiming its addresses. This runs
	// independently of shouldDemote/isTargetNodePromotion, because an election-driven
	// self-promotion takes neither of those paths yet is exactly the case that dual-homes the
	// group (docs/TEST-PLAN.md TC-6).
	if prevActiveID == "" && len(unconfirmedIncumbents) > 0 {
		for _, id := range unconfirmedIncumbents {
			// Must be bounded: without a deadline a wedged peer hangs this goroutine forever
			// instead of surfacing the DeadlineExceeded the decision below relies on.
			//
			// Sized to the group rather than fixed at 10s. The peer's MakePassive drops
			// and verifies every configured group address, so on the 201-address topology
			// a healthy but loaded incumbent overran the flat deadline — and an overrun is
			// read as "still owns its IPs", which aborts a promotion that was safe.
			mpCtx, mpCancel := context.WithTimeout(context.Background(), s.demotionTimeout())
			released, provablyDown, err := s.confirmPeerReleasedIPs(mpCtx, id)
			mpCancel()
			if released {
				s.logger.Info("PROMOTE_ASYNC: Confirmed unreachable node released its floating IPs", "node_id", id)
				continue
			}

			// Only a transport-level failure proves nothing is holding those addresses. A wedged
			// peer that accepted the connection but never answered is alive and still owns every
			// floating IP, so quorum cannot make claiming them safe.
			stillAlive := !provablyDown
			// Otherwise the peer is genuinely unreachable. Promote only from the majority side:
			// a minority must never claim addresses it cannot prove were released.
			haveQuorum := s.quorumManager != nil && s.quorumManager.HasQuorum(reachableCount)

			if !canPromoteWithoutConfirmedRelease(stillAlive, haveQuorum, forceDemote) {
				s.logger.Error("PROMOTE_ASYNC: Aborting promotion - cannot confirm unreachable node released its floating IPs",
					"unconfirmed_node", id,
					"target", targetNodeID,
					"peer_still_alive", stillAlive,
					"have_quorum", haveQuorum,
					"reachable_nodes", reachableCount,
					"error", err)
				s.restoreMemberStates(originalStates)
				_ = s.broadcastNextEpoch(originalStates)
				return
			}

			s.logger.Warn("PROMOTE_ASYNC: Proceeding without a confirmed release",
				"unconfirmed_node", id,
				"force_demote", forceDemote,
				"peer_still_alive", stillAlive,
				"have_quorum", haveQuorum,
				"error", err)
			if mm := s.memberList.GetMemberByID(id); mm != nil {
				mm.MarkUnreachable()
			}
		}
	}

	// Step 2: Promote target node
	if member.IsLocal() {
		s.logger.Info("PROMOTE_ASYNC: Promoting local node to Active", "node_id", targetNodeID)
		if err := member.MakeActive(ips); err != nil {
			s.logger.Error("PROMOTE_ASYNC: Failed to promote local node",
				"node_id", targetNodeID,
				"error", err)

			// Attempt rollback
			if prevActiveID != "" && prevActiveID != targetNodeID {
				s.logger.Info("PROMOTE_ASYNC: Attempting rollback of failed promotion")
				_, _ = s.MakePassive(context.Background(), &rpc.MakePassiveRequest{NodeId: targetNodeID})
				if mm := s.memberList.GetMemberByID(prevActiveID); mm != nil {
					_ = mm.MakeActive(nil)
				}
			}
			s.restoreMemberStates(originalStates)
			_ = s.broadcastNextEpoch(originalStates)
			return
		}
		s.logger.Info("PROMOTE_ASYNC: Successfully promoted local node to Active", "node_id", targetNodeID)
	} else {
		s.logger.Info("PROMOTE_ASYNC: Target is remote, forwarding promote request", "node_id", targetNodeID)
		node := s.config.Nodes[targetNodeID]
		if node == nil {
			s.logger.Error("PROMOTE_ASYNC: Node configuration not found", "node_id", targetNodeID)
			_ = s.broadcastNextEpoch(originalStates)
			return
		}

		remoteClient, err := client.New()
		if err != nil {
			s.logger.Error("PROMOTE_ASYNC: Failed to create client", "error", err)
			_ = s.broadcastNextEpoch(originalStates)
			return
		}
		defer remoteClient.Close()

		if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
			s.logger.Error("PROMOTE_ASYNC: Failed to connect to target node", "error", err)
			_ = s.broadcastNextEpoch(originalStates)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		rresp, rerr := remoteClient.CLI().Promote(ctx, &rpc.PromoteRequest{
			NodeId:      targetNodeID,
			Ips:         ips,
			ForceDemote: forceDemote,
		})

		if rerr != nil || (rresp != nil && !rresp.Success) {
			s.logger.Error("PROMOTE_ASYNC: Remote promotion failed",
				"error", rerr,
				"response", rresp)

			// Attempt rollback
			if prevActiveID != "" && prevActiveID != targetNodeID {
				s.logger.Info("PROMOTE_ASYNC: Attempting rollback after remote failure")
				if mm := s.memberList.GetMemberByID(prevActiveID); mm != nil {
					_ = mm.MakeActive(nil)
				}
			}
			s.restoreMemberStates(originalStates)
			_ = s.broadcastNextEpoch(originalStates)
			return
		}

		member.SetStatus(membership.StatusActive)
		s.logger.Info("PROMOTE_ASYNC: Remote promotion succeeded", "node_id", targetNodeID)
	}

	// Step 3: Orchestrate IP failover (potentially slow operation)
	if s.config.Pulse.Mode == "active-passive" {
		var allIPs []string
		for _, ipList := range s.config.Groups {
			allIPs = append(allIPs, ipList...)
		}

		if len(allIPs) > 0 {
			s.logger.Info("PROMOTE_ASYNC: Starting IP failover orchestration",
				"ip_count", len(allIPs),
				"old_node", prevActiveID,
				"new_node", targetNodeID)

			if err := s.OrchestrateIPFailover(prevActiveID, targetNodeID, allIPs); err != nil {
				s.logger.Warn("PROMOTE_ASYNC: IP failover encountered issues", "error", err)
			} else {
				s.logger.Info("PROMOTE_ASYNC: IP failover completed successfully")
			}
		}
	}

	// Step 4: Broadcast final cluster state
	states := make(map[string]membership.MemberStatus)
	for id, m := range s.memberList.MembersSnapshot() {
		if id == prevActiveID && s.config.Pulse.Mode == "active-passive" {
			if demotionFailed {
				states[id] = membership.StatusUnknown
			} else {
				states[id] = membership.StatusPassive
			}
		} else if id == targetNodeID {
			states[id] = membership.StatusActive
		} else {
			states[id] = m.GetStatus()
		}
	}

	newEpoch := s.GetClusterEpoch() + 1
	s.logger.Info("PROMOTE_ASYNC: Broadcasting final cluster state",
		"new_active", targetNodeID,
		"epoch", newEpoch)
	_ = s.BroadcastClusterState(states, newEpoch, targetNodeID, nil)

	elapsed := time.Since(startTime)
	s.logger.Info("PROMOTE_ASYNC: Promotion completed successfully",
		"target_node", targetNodeID,
		"elapsed_time", elapsed.String())
}

// MakePassive handles the passive RPC call making one node passive
func (s *Server) MakePassive(ctx context.Context, req *rpc.MakePassiveRequest) (*rpc.MakePassiveResponse, error) {
	s.logger.Infof("Received make passive request for node ID: %s", req.NodeId)

	if req.NodeId == "" {
		return &rpc.MakePassiveResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
	}

	// Get the member
	member := s.memberList.GetMemberByID(req.NodeId)
	if member == nil {
		return &rpc.MakePassiveResponse{
			Success: false,
			Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
		}, nil
	}

	// If local, make passive locally; otherwise forward to remote node and reflect state
	if member.IsLocal() {
		// Becoming Passive means holding none of the cluster's floating IPs, so the drop set is
		// every group rather than the ones this node is currently assigned. During a mode change
		// the assignment is rewritten while the addresses are still up, which left this method
		// dropping nothing and reporting success anyway — the exact shape of the TC-6 split-brain
		// (docs/TEST-PLAN.md defect #21). This mirrors what the IP monitor's non-Active branch
		// already does when it cleans up.
		var ipsToDrop []string
		for _, ipList := range s.config.Groups {
			ipsToDrop = append(ipsToDrop, ipList...)
		}
		// Release the IPs while the node still counts as Active: BringDownIPs
		// on an already-Passive local node defers to the monitor instead.
		if len(ipsToDrop) > 0 {
			if err := member.BringDownIPs(ipsToDrop); err != nil {
				s.logger.Warn("Failed to bring down IPs during demotion", "error", err)
			}
		}

		// Verify against the interfaces instead of trusting the call above. Callers use this
		// response to decide whether they may claim these addresses, so reporting success while
		// any are still up is what dual-homes the group.
		remaining, verifyErr := stillHeldIPs(ipsToDrop)
		if verifyErr != nil {
			// Unable to read the interfaces, so the release cannot be established either way.
			// Report failure rather than a release we did not observe.
			s.logger.Error("MakePassive: cannot verify floating IP release",
				"node_id", req.NodeId, "error", verifyErr)
			return &rpc.MakePassiveResponse{
				Success: false,
				Message: "unable to verify floating IP release: " + verifyErr.Error(),
			}, nil
		}
		if len(remaining) > 0 {
			// Deliberately leave the status as-is. This node is still serving these addresses;
			// marking it Passive would have the monitor strip them from under live traffic while
			// the promotion that requested the demotion has already been refused.
			s.logger.Error("MakePassive: refusing to report a release that did not happen",
				"node_id", req.NodeId,
				"remaining", len(remaining),
				"sample", remaining[:min(3, len(remaining))])
			return &rpc.MakePassiveResponse{
				Success: false,
				Message: fmt.Sprintf("%d floating IP(s) still up after demotion (e.g. %s)",
					len(remaining), strings.Join(remaining[:min(3, len(remaining))], ", ")),
			}, nil
		}

		// Passive with nothing claimed: a demoted node must not go on telling
		// peers it holds addresses it has given up.
		member.SetClaim(membership.Claim{Status: membership.StatusPassive})
	} else {
		node := s.config.Nodes[req.NodeId]
		if node == nil {
			return &rpc.MakePassiveResponse{Success: false, Message: "Node configuration not found"}, nil
		}
		remoteClient, err := client.New()
		if err != nil {
			return &rpc.MakePassiveResponse{Success: false, Message: "Failed to create client: " + err.Error()}, nil
		}
		defer remoteClient.Close()
		if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
			return &rpc.MakePassiveResponse{Success: false, Message: "Failed to connect to target node: " + err.Error()}, nil
		}
		// Derived from the caller's context so a caller with a deadline — the
		// health checker's consolidation invariant — isn't left waiting on an
		// unresponsive peer for the full timeout.
		//
		// Sized rather than flat. WithTimeout takes the *sooner* of the parent
		// deadline and the one given here, so a flat 5s clamped every demotion
		// that crosses this hop back to 5s — including enforceSingleActive's
		// makePassiveTimeout() (up to 120s) and performPromotionAsync's step-1
		// demotion, which is exactly the hop that matters, since the remote node
		// is the one doing the releasing and verifying. The constants above
		// DemotionTimeoutFor record that even a flat 10s was overrun by a loaded
		// incumbent on the 201-address topology, and that an overrun is not
		// neutral: DeadlineExceeded is deliberately read as "the peer may still
		// own its IPs", so a too-short deadline aborts promotions and
		// consolidations that were safe. confirmPeerReleasedIPs got the sized
		// deadline only because it bypasses this method entirely.
		ctx2, cancel := s.remoteDemotionContext(ctx)
		defer cancel()
		rresp, rerr := remoteClient.Server().MakePassive(ctx2, &rpc.MakePassiveRequest{NodeId: req.NodeId})
		if rerr != nil {
			return &rpc.MakePassiveResponse{Success: false, Message: "Remote make passive failed: " + rerr.Error()}, nil
		}
		if !rresp.Success {
			return &rpc.MakePassiveResponse{Success: false, Message: rresp.Message}, nil
		}
		// Reflect locally
		// Passive with nothing claimed: a demoted node must not go on telling
		// peers it holds addresses it has given up.
		member.SetClaim(membership.Claim{Status: membership.StatusPassive})
	}

	// Success
	// Update local monitor expectations based on new role
	s.refreshLocalMonitorExpectedIPs()
	return &rpc.MakePassiveResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully made node %s passive", req.NodeId),
	}, nil
}

// HealthCheck handles the health check RPC call
func (s *Server) HealthCheck(ctx context.Context, req *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	localToken := s.config.Pulse.ClusterToken

	// Validate cluster membership token when provided
	if req.ClusterToken != "" && req.ClusterToken != localToken {
		s.logger.Warnf("HealthCheck cluster token mismatch from node %s (expected %s, got %s)",
			req.NodeId, localToken, req.ClusterToken)
		return &rpc.HealthCheckResponse{
			Success:      false,
			Message:      "cluster token mismatch",
			ClusterToken: localToken,
		}, nil
	}

	// Get the member
	var member *membership.Member

	if req.NodeId != "" {
		member = s.memberList.GetMemberByID(req.NodeId)
		if member == nil {
			return &rpc.HealthCheckResponse{
				Success:      false,
				Message:      fmt.Sprintf("Node not found with ID: %s", req.NodeId),
				ClusterToken: localToken,
			}, nil
		}
		s.logger.Debugf("Found node by ID: %s (%s)", req.NodeId, member.Hostname)
	}

	if member == nil {
		return &rpc.HealthCheckResponse{
			Success:      false,
			Message:      "No node identifier provided",
			ClusterToken: localToken,
		}, nil
	}

	// Genuine contact: this peer just called us, so record it. Through the
	// accessor, because Member's mutex is private and three consumers read this
	// field under it to measure silence.
	member.SetLastResponse(time.Now())

	// Latency is deliberately not touched here. It used to be set to
	// time.Since(member.LastHCResponse) on the line after that stamp, which is
	// zero by construction — so an inbound health check overwrote the latency
	// our own outbound check had measured with a meaningless ~0s. What this
	// side of the call can see is not a round trip.
	s.logger.Debugf("Member %s health-checked us", member.Hostname)

	return &rpc.HealthCheckResponse{
		Success:      true,
		Message:      fmt.Sprintf("Node %s is healthy", member.Hostname),
		ClusterToken: localToken,
	}, nil
}

// Remove removes a node from the cluster
func (s *Server) Remove(ctx context.Context, req *rpc.RemoveRequest) (*rpc.RemoveResponse, error) {
	s.logger.Infof("Received remove request for node ID: %s", req.NodeId)

	if req.NodeId == "" {
		return &rpc.RemoveResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
	}

	// Get the member
	member := s.memberList.GetMemberByID(req.NodeId)
	if member == nil {
		return &rpc.RemoveResponse{
			Success: false,
			Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
		}, nil
	}

	// Remove the node from our member list
	if err := s.memberList.RemoveMember(member.ID); err != nil {
		s.logger.Error("Failed to remove member", "error", err)
		return &rpc.RemoveResponse{
			Success: false,
			Message: "Failed to remove member: " + err.Error(),
		}, nil
	}

	// Update our config to remove the node
	delete(s.config.Nodes, member.ID)

	// Persist the updated configuration
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config after removing node", "error", err)
		return &rpc.RemoveResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	// Success
	return &rpc.RemoveResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully removed node %s from the cluster", req.NodeId),
	}, nil
}

// CoordinateRemoval handles a coordinated node removal request using quorum consensus.
// The coordinator broadcasts the removal to all other cluster members and requires
// a majority (quorum) to acknowledge the removal. This allows the cluster to make
// progress even when some nodes are unavailable.
func (s *Server) CoordinateRemoval(ctx context.Context, req *rpc.CoordinateRemovalRequest) (*rpc.CoordinateRemovalResponse, error) {
	s.logger.Info("Received coordinated removal request", "node_id", req.NodeId)

	if req.NodeId == "" {
		return &rpc.CoordinateRemovalResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
	}

	// Validate the node exists
	member := s.memberList.GetMemberByID(req.NodeId)
	if member == nil {
		return &rpc.CoordinateRemovalResponse{
			Success: false,
			Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
		}, nil
	}

	localNodeID, _ := s.config.GetLocalNodeUUID()

	// Get all peers (excluding the node being removed and ourselves)
	s.RLock()
	var peers []struct {
		id   string
		ip   string
		port string
	}
	totalClusterSize := len(s.config.Nodes)
	for id, node := range s.config.Nodes {
		if id == req.NodeId || id == localNodeID || node == nil {
			continue
		}
		peers = append(peers, struct {
			id   string
			ip   string
			port string
		}{id: id, ip: node.IP, port: node.Port})
	}
	s.RUnlock()

	// Calculate quorum requirement: majority of the cluster AFTER the node is removed
	// If we have 4 nodes and removing 1, quorum is majority of remaining 3 = 2
	remainingNodes := totalClusterSize - 1 // Excluding the node being removed
	requiredQuorum := (remainingNodes / 2) + 1

	s.logger.Info("Quorum-based removal initiated",
		"total_cluster_size", totalClusterSize,
		"remaining_after_removal", remainingNodes,
		"required_quorum", requiredQuorum)

	// Broadcast removal to all peers
	var updatedCount int32
	var failedNodes []string

	for _, peer := range peers {
		s.logger.Info("Broadcasting removal to peer", "peer_id", peer.id)
		remoteClient, err := client.New()
		if err != nil {
			s.logger.Warn("Failed to create client for peer", "peer", peer.id, "error", err)
			failedNodes = append(failedNodes, peer.id)
			continue
		}

		if err := remoteClient.Connect(peer.ip, peer.port, false); err != nil {
			s.logger.Warn("Failed to connect to peer", "peer", peer.id, "error", err)
			remoteClient.Close()
			failedNodes = append(failedNodes, peer.id)
			continue
		}

		pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := remoteClient.Server().Remove(pctx, &rpc.RemoveRequest{NodeId: req.NodeId})
		pcancel()
		remoteClient.Close()

		if err != nil {
			s.logger.Warn("Failed to send removal to peer", "peer", peer.id, "error", err)
			failedNodes = append(failedNodes, peer.id)
			continue
		}

		if !resp.Success {
			s.logger.Warn("Peer rejected removal", "peer", peer.id, "message", resp.Message)
			failedNodes = append(failedNodes, peer.id)
			continue
		}

		s.logger.Info("Peer successfully removed node", "peer", peer.id)
		updatedCount++
	}

	// Count ourselves (coordinator) as updated
	updatedCount++

	// Check if we achieved quorum
	if updatedCount < int32(requiredQuorum) {
		s.logger.Warn("Failed to achieve quorum for removal",
			"updated_count", updatedCount,
			"required_quorum", requiredQuorum,
			"failed_nodes", len(failedNodes))

		return &rpc.CoordinateRemovalResponse{
			Success:      false,
			Message:      fmt.Sprintf("Quorum not met: only %d of %d required nodes acknowledged removal", updatedCount, requiredQuorum),
			UpdatedNodes: updatedCount,
			FailedNodes:  failedNodes,
		}, nil
	}

	// Quorum achieved - now commit the removal locally
	if err := s.memberList.RemoveMember(member.ID); err != nil {
		s.logger.Error("Failed to remove member locally after quorum", "error", err)
		return &rpc.CoordinateRemovalResponse{
			Success: false,
			Message: "Failed to remove member: " + err.Error(),
		}, nil
	}

	delete(s.config.Nodes, member.ID)

	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config after quorum removal", "error", err)
		return &rpc.CoordinateRemovalResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save local config: %v", err),
		}, nil
	}

	// Log which nodes failed (for operator awareness)
	if len(failedNodes) > 0 {
		s.logger.Warn("Removal succeeded with quorum, but some nodes were unreachable",
			"updated_count", updatedCount,
			"failed_nodes", failedNodes)

		return &rpc.CoordinateRemovalResponse{
			Success:      true,
			Message:      fmt.Sprintf("Node %s removed with quorum (%d/%d nodes), %d unreachable: %v", req.NodeId, updatedCount, remainingNodes, len(failedNodes), failedNodes),
			UpdatedNodes: updatedCount,
			FailedNodes:  failedNodes,
		}, nil
	}

	return &rpc.CoordinateRemovalResponse{
		Success:      true,
		Message:      fmt.Sprintf("Node %s successfully removed from all %d cluster members", req.NodeId, updatedCount),
		UpdatedNodes: updatedCount,
		FailedNodes:  []string{},
	}, nil
}

// Reconfigure updates the server configuration in real-time. Serialized via
// reconfigureMu so concurrent callers (e.g. ConfigSync-triggered reconfigures)
// don't both attempt to rebind the cluster listener simultaneously.
//
// Lock order: reconfigureMu is acquired before any short-lived s.RWMutex
// sections taken inside the body. Callers MUST NOT hold s.RWMutex when
// invoking Reconfigure.
func (s *Server) Reconfigure() error {
	s.reconfigureMu.Lock()
	defer s.reconfigureMu.Unlock()

	s.logger.Info("Reconfiguring PulseHA server...")

	// Reload the config from disk into a fresh instance, then swap the
	// pointer. Reloading in place would rewrite the config while the health
	// checker, the IP monitor and every in-flight RPC are reading it
	// (defect #32); a pointer swap leaves those readers on a consistent
	// snapshot. Same shape as ConfigSync's `s.config = newConfig`.
	s.logger.Debug("Reloading configuration...")
	s.RLock()
	reloadedFrom := s.config
	s.RUnlock()
	newConfig, err := reloadedFrom.Reload()
	if err != nil {
		return fmt.Errorf("failed to reload config: %v", err)
	}
	s.Lock()
	// Only install the reload if nothing replaced the config while we were
	// reading the file. ConfigSync saves and swaps under this same lock, but the
	// read above is outside it, so a sync landing in between used to be undone in
	// memory by a snapshot taken before it existed — the node then served a
	// config older than the one on its own disk, and broadcast it as its own,
	// until the next reconfigure happened to correct it. Measured 2026-08-07 on
	// two back-to-back syncs: disk 120 addresses, memory 100.
	//
	// Pointer identity rather than a counter, so this catches every writer that
	// swaps s.config, not just the ones remembering to bump something. Superseded
	// means the installed config is already at least as new as what we read, so
	// there is nothing to do but let the rest of the function work on it.
	superseded := s.config != reloadedFrom
	if superseded {
		newConfig = s.config
	} else {
		s.config = newConfig
	}
	s.Unlock()
	if superseded {
		s.logger.Debug("Reconfigure: a newer config was applied while reloading; keeping it")
	}
	// The member list holds its own pointer, and the health checker reads
	// the config through it, so both go stale without this.
	s.memberList.UpdateConfig(newConfig)

	// Get local node config (no server lock needed)
	s.logger.Debug("Getting updated local node configuration...")
	localNode, err := newConfig.GetLocalNode()
	if err != nil {
		return fmt.Errorf("failed to get local node config: %v", err)
	}
	s.logger.Infof("Updated local node configuration: IP=%s, Port=%s", localNode.IP, localNode.Port)

	// Rebind the cluster listener only when the bind address actually moved.
	//
	// Almost every Reconfigure comes from a ConfigSync that changed the config and
	// nothing else — a group gained an address, a peer changed status — and for
	// those the listener is already serving the right endpoint. Rebinding it anyway
	// is not free in a way that is easy to miss: GracefulStop plus a fresh bind
	// refuses every inbound RPC for the gap, so peers see `connection refused` for
	// seconds against a daemon that never restarted (docs/TEST-PLAN.md defect #31).
	// Under a burst of mutations each broadcast then tore the listener down on every
	// receiver, which is what refused 56 of 60 peer bring-up RPCs on whitecrane in
	// run 23 and what starved the config broadcast's own retries into defect #43.
	//
	// The address is the whole of the listener's configuration here — the gRPC
	// server is built with no credentials and no options — so an unchanged address
	// means there is genuinely nothing to re-apply.
	address := fmt.Sprintf("%s:%s", utils.FormatIPv6(localNode.IP), localNode.Port)
	if s.clusterListenerServing(address) {
		s.logger.Debug("Cluster bind address unchanged; keeping the listener serving",
			"address", address)
	} else {
		// Swap out old cluster gRPC server pointer quickly under lock, then stop outside lock
		var oldSrv *grpc.Server
		s.Lock()
		oldSrv = s.grpcServer
		s.grpcServer = nil
		s.Unlock()
		if oldSrv != nil {
			s.logger.Debug("Stopping existing gRPC server...")
			oldSrv.GracefulStop()
		}

		// Create new gRPC server instance and assign pointer under a short lock
		newSrv := grpc.NewServer()
		rpc.RegisterServerServer(newSrv, s)
		// Also register CLI service on the cluster listener for remote operations (e.g., join)
		rpc.RegisterCLIServer(newSrv, s)
		s.Lock()
		s.grpcServer = newSrv
		s.Unlock()

		s.logger.Debugf("Starting cluster listener on %s:%s...", utils.FormatIPv6(localNode.IP), localNode.Port)
		if err := s.startClusterListener(localNode); err != nil {
			return fmt.Errorf("failed to start cluster listener: %v", err)
		}
	}

	// Ensure health checker is running after reconfigure
	s.startHealthChecker()

	s.logger.Info("Server reconfiguration completed successfully")
	return nil
}

// GetMemberList returns the server's member list
func (s *Server) GetMemberList() *membership.MemberList {
	return s.memberList
}

// resolvePeerPlaceholder removed: placeholders are no longer introduced; nodes always sync by UUID.

// RefreshLocalMonitorExpectedIPs updates the IP monitor's expected IPs for the local node (public method for interface)
func (s *Server) RefreshLocalMonitorExpectedIPs() {
	s.logger.Info("REFRESH: RefreshLocalMonitorExpectedIPs called")

	// Add call stack to understand what's triggering continuous refreshes
	if s.logger != nil {
		// Get caller information to trace what's calling this function
		_, file, line, ok := runtime.Caller(1)
		if ok {
			// Get just the filename without the full path
			parts := strings.Split(file, "/")
			filename := parts[len(parts)-1]
			s.logger.Info("REFRESH: Called from", "file", filename, "line", line)
		}

		// Get a stack trace to understand the full call chain
		buf := make([]byte, 1024)
		n := runtime.Stack(buf, false)
		stackTrace := string(buf[:n])
		// Log just the first few lines to avoid overwhelming the logs
		lines := strings.Split(stackTrace, "\n")
		if len(lines) > 10 {
			lines = lines[:10]
		}
		s.logger.Debug("REFRESH: Call stack trace", "stack", strings.Join(lines, "\n"))
	}

	s.refreshLocalMonitorExpectedIPs()
	s.logger.Debug("REFRESH: RefreshLocalMonitorExpectedIPs completed")
}

// expectedIfaceIPs returns the floating IPs the given node should hold on iface.
//
// In active-passive the Active node owns every IP of every group mapped to that
// interface. In active-active the groups are shared, so a node owns only the
// subset assigned to it. Seeding the monitor with the whole group in that mode
// makes every Active node's next enforce tick re-add all of them, which undoes
// each rebalance move about as fast as the coordinator can make one — the
// cluster then never converges and addresses flap between owners
// (docs/TEST-PLAN.md defects #2/#26).
//
// An active-active node with no assignments expects nothing, which is the point:
// it should hold no addresses until the coordinator gives it some.
func (s *Server) expectedIfaceIPs(nodeID, iface string) []string {
	node := s.config.Nodes[nodeID]
	if node == nil {
		return nil
	}

	var groupIPs []string
	for _, g := range node.IPGroups[iface] {
		ips, ok := s.config.Groups[g]
		if !ok {
			s.logger.Warn("Group not found in config", "group", g, "iface", iface)
			continue
		}
		groupIPs = append(groupIPs, ips...)
	}

	if s.config.Pulse.Mode != "active-active" {
		return groupIPs
	}
	return s.filterToAssigned(nodeID, groupIPs)
}

// filterToAssigned narrows a whole-group address list to the subset currently
// assigned to nodeID, preserving order.
//
// "Assigned nothing" must not collapse into "no restriction" — a node awaiting
// its first assignment holds nothing, rather than the entire group — so callers
// decide by mode whether to filter at all, and this returns nil when the node
// has no assignments. It is shared by every site that has to answer "which of
// these addresses are mine", because the last time two sites answered it
// separately one of them missed a fix (defect #30 missing #2/#26).
func (s *Server) filterToAssigned(nodeID string, ips []string) []string {
	assigned := make(map[string]bool)
	if m := s.memberList.GetMemberByID(nodeID); m != nil {
		for _, ip := range m.GetActiveIPs() {
			assigned[ip] = true
		}
	}

	var mine []string
	for _, ip := range ips {
		if assigned[ip] {
			mine = append(mine, ip)
		}
	}
	return mine
}

// refreshLocalMonitorExpectedIPs updates the IP monitor's expected IPs for the local node
// Only enforces when the local member is Active; clears expectations when not active
func (s *Server) refreshLocalMonitorExpectedIPs() {
	s.logger.Debug("REFRESH: Starting refreshLocalMonitorExpectedIPs")

	if s.ipMonitor == nil {
		s.logger.Debug("REFRESH: IP monitor is nil, skipping")
		return
	}

	// Get current local node status
	localID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		s.logger.Error("REFRESH: Failed to get local node ID", "error", err)
		return
	}

	member := s.memberList.GetMemberByID(localID)
	node := s.config.Nodes[localID]
	if member == nil || node == nil {
		s.logger.Error("REFRESH: Member or node config not found", "member", member != nil, "node", node != nil)
		return
	}

	// One locked read for the whole refresh. These were five unlocked reads of the
	// field, which raced every writer -- and a refresh that re-read it could take
	// the not-Active cleanup branch and then log its way through as Active.
	status := member.GetStatus()

	s.logger.Info("REFRESH: Processing node", "nodeID", localID, "status", membership.StatusToString(status))

	if status != membership.StatusActive {
		// For passive/unknown/maintenance nodes, trigger enforcement to clean up any floating IPs
		s.logger.Info("REFRESH: Node is not Active, cleanup needed", "status", membership.StatusToString(status))
		if s.ipMonitor != nil {
			s.logger.Debug("REFRESH: Calling TriggerEnforce for passive node cleanup")
			s.ipMonitor.TriggerEnforce()
		} else {
			s.logger.Debug("REFRESH: IP monitor disabled, skipping cleanup TriggerEnforce")
			s.cleanupFloatingIPsDirectly(node)
		}
		return
	}

	s.logger.Info("REFRESH: Node is Active, setting up expected IPs", "status", membership.StatusToString(status))

	// One interface snapshot for the whole refresh, not one netlink dump per
	// expected address. network.CheckIfIPExists builds a complete inventory —
	// every link, both families — on each call, so scanning a 72-address share
	// cost 72 of them, and this scan's output is a whole-share bring-up
	// (docs/TEST-PLAN.md defect #64). A dump that fails leaves the lookup nil and
	// every expected address is treated as missing, which is what the discarded
	// error here used to achieve by accident.
	var heldOn func(ip string) (bool, string)
	if inventory, invErr := network.BuildIPInventory(); invErr != nil {
		s.logger.Warn("REFRESH: could not read interface addresses; treating every expected address as missing", "error", invErr)
	} else {
		heldOn = ipInventoryLookup(inventory)
	}

	for iface := range node.IPGroups {
		s.logger.Debug("REFRESH: Processing interface", "iface", iface, "groups", node.IPGroups[iface])
		ifaceIPs := s.expectedIfaceIPs(localID, iface)
		s.ipMonitor.ClearExpectedIPs(iface)
		if len(ifaceIPs) > 0 {
			s.logger.Info("REFRESH: Updating expected IPs for Active node", "iface", iface, "ips", ifaceIPs)
			s.ipMonitor.UpdateExpectedIPs(iface, ifaceIPs)
			// Proactively bring up any missing expected IPs on this interface
			missing, invalid := missingOnIface(iface, ifaceIPs, heldOn)
			if len(invalid) > 0 {
				// Skipped, but not silently: an unparseable configured address is
				// never placed, and without this it looks like a floating IP that
				// will not come up rather than a config entry that cannot be read.
				s.logger.Warn("REFRESH: skipping unparseable configured addresses",
					"iface", iface, "addresses", invalid)
			}
			if len(missing) > 0 {
				s.logger.Info("REFRESH: Bringing up missing IPs on Active node", "iface", iface, "missingIPs", missing, "status", membership.StatusToString(status))
				_, err := s.BringUpIP(context.Background(), &rpc.UpIpRequest{Iface: iface, Ips: missing})
				if err != nil {
					s.logger.Error("REFRESH: Failed to bring up missing IPs", "error", err, "iface", iface, "ips", missing)
				} else {
					s.logger.Info("REFRESH: Successfully brought up missing IPs", "iface", iface, "ips", missing)
				}
			} else {
				s.logger.Debug("REFRESH: No missing IPs for interface", "iface", iface)
			}
		} else {
			s.logger.Debug("REFRESH: No IPs configured for interface", "iface", iface)
		}
	}

	// IP monitor is disabled, so no TriggerEnforce call
	if s.ipMonitor != nil {
		s.logger.Debug("REFRESH: Calling TriggerEnforce on IP monitor")
		s.ipMonitor.TriggerEnforce()
	} else {
		s.logger.Debug("REFRESH: IP monitor disabled, skipping TriggerEnforce")
	}
	s.logger.Debug("REFRESH: Completed refreshLocalMonitorExpectedIPs")
}

// cleanupFloatingIPsDirectly removes floating IPs directly using network calls (used when IP monitor is disabled)
func (s *Server) cleanupFloatingIPsDirectly(node *config.Node) {
	s.logger.Debug("CLEANUP: Starting direct floating IP cleanup for non-Active node")

	// Build list of all floating IPs that this node could potentially manage
	var allFloatingIPs []string
	for ifaceName, groups := range node.IPGroups {
		s.logger.Debug("CLEANUP: Checking interface", "iface", ifaceName, "groups", groups)
		for _, group := range groups {
			if ips, ok := s.config.Groups[group]; ok {
				allFloatingIPs = append(allFloatingIPs, ips...)
				s.logger.Debug("CLEANUP: Found IPs in group", "group", group, "ips", ips)
			} else {
				s.logger.Debug("CLEANUP: Group not found", "group", group)
			}
		}
	}

	if len(allFloatingIPs) == 0 {
		s.logger.Info("CLEANUP: No floating IPs to check")
		return
	}

	s.logger.Info("CLEANUP: Checking for floating IPs to clean up", "count", len(allFloatingIPs), "ips", allFloatingIPs)

	// Check each floating IP and remove if found on any interface
	for _, ip := range allFloatingIPs {
		s.logger.Debug("CLEANUP: Checking IP", "ip", ip)

		// Extract IP without CIDR if needed
		ipOnly := ip
		if cidr, err := utils.GetCIDR(ip); err == nil && cidr != nil {
			ipOnly = cidr.String()
		}

		exists, iface, err := network.CheckIfIPExists(ipOnly)
		if err != nil {
			s.logger.Debug("CLEANUP: Error checking IP existence", "ip", ip, "error", err)
			continue
		}

		if exists {
			s.logger.Info("CLEANUP: Found floating IP on interface, removing", "ip", ip, "iface", iface)
			if err := network.BringIPdown(iface, ip); err != nil {
				s.logger.Warn("CLEANUP: Failed to remove floating IP", "ip", ip, "iface", iface, "error", err)
			} else {
				s.logger.Info("CLEANUP: Successfully removed floating IP", "ip", ip, "iface", iface)
			}
		} else {
			s.logger.Debug("CLEANUP: Floating IP not found on any interface", "ip", ip)
		}
	}

	s.logger.Debug("CLEANUP: Direct floating IP cleanup complete")
}

// BroadcastVoteRequest broadcasts a voting session request to all cluster nodes
func (s *Server) BroadcastVoteRequest(sessionID string, voteType, subject, description string, timeoutSeconds int64) error {
	localID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		return fmt.Errorf("failed to get local node ID: %v", err)
	}

	// Get all cluster nodes
	var broadcastErrors []string
	successCount := 0

	for nodeID, node := range s.config.Nodes {
		if nodeID == localID {
			continue // Skip local node - we already started the session locally
		}

		// Create client connection
		remoteClient, err := client.New()
		if err != nil {
			broadcastErrors = append(broadcastErrors, fmt.Sprintf("node %s: failed to create client: %v", nodeID, err))
			continue
		}

		// Connect to the remote node
		if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
			broadcastErrors = append(broadcastErrors, fmt.Sprintf("node %s: connection failed: %v", nodeID, err))
			remoteClient.Close()
			continue
		}

		// Ask the remote node to create its own local voting session with the same ID
		// This approach ensures each node has its own local session but they coordinate votes
		// Create the same local voting session on the remote node's quorum manager
		go func(nodeID string, node *config.Node, rc *client.Client) {
			defer rc.Close()

			// First, try to cast a vote on our local session using their node ID
			// This simulates them voting on our session
			if s.quorumManager != nil {
				err := s.quorumManager.CastVote(sessionID, nodeID, quorum.VoteDecisionYes)
				if err != nil {
					s.logger.Debugf("Could not register remote vote from %s: %v", nodeID, err)
				} else {
					s.logger.Debugf("Registered vote from remote node %s", nodeID)
				}
			}
		}(nodeID, node, remoteClient)

		successCount++
		s.logger.Debugf("Successfully initiated vote process for node %s", nodeID)
		// Note: remoteClient.Close() is now handled by the goroutine's defer statement
	}

	// Log results
	if len(broadcastErrors) > 0 {
		s.logger.Warnf("Vote broadcast had %d errors: %v", len(broadcastErrors), broadcastErrors)
	}

	s.logger.Infof("Vote broadcast completed: %d successes out of %d total nodes", successCount, len(s.config.Nodes)-1)

	// Return success if we got at least one response, or if it's a single-node cluster
	totalPeers := len(s.config.Nodes) - 1
	if totalPeers == 0 || successCount > 0 {
		return nil
	}

	return fmt.Errorf("failed to broadcast vote request to any nodes: %v", broadcastErrors)
}

// pendingIPWork is a node whose floating IPs still have to be released or brought
// up. Both are synchronous network operations — a gRPC call for a remote node, an
// address-by-address bring-up locally — so the work is deferred until the server
// lock has been dropped rather than blocking every other daemon operation.
type pendingIPWork struct {
	member *membership.Member
	ips    []string
}

// SetMode handles changing the cluster operation mode
func (s *Server) SetMode(ctx context.Context, req *rpc.SetModeRequest) (*rpc.SetModeResponse, error) {
	s.logger.Infof("Received request to change cluster mode to: %s", req.Mode)
	s.Lock()
	defer s.Unlock()

	// Validate request
	if req.Mode != "active-passive" && req.Mode != "active-active" {
		return &rpc.SetModeResponse{
			Success: false,
			Message: fmt.Sprintf("invalid mode: %s", req.Mode),
		}, nil
	}

	// Get current mode
	currentMode := "active-passive" // Default mode
	if s.config.Pulse.Mode == "active-active" {
		currentMode = "active-active"
	}

	// If mode is not changing, return early
	if currentMode == req.Mode {
		return &rpc.SetModeResponse{
			Success: true,
			Message: fmt.Sprintf("cluster is already in %s mode", req.Mode),
		}, nil
	}

	// IP releases owed by nodes demoted below, run after s.Unlock().
	var demotions []pendingIPWork
	// The IPs the consolidated Active must bring up, likewise run after s.Unlock().
	var activation *pendingIPWork

	// Update mode in config
	s.config.Pulse.Mode = req.Mode

	// Save config
	if err := s.config.Save(); err != nil {
		return &rpc.SetModeResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	// If switching to active-active, record where the floating IPs already are
	// and let the coordinator spread them from there.
	//
	// Clearing every assignment and redistributing the whole group was wrong on
	// both counts. It left the former sole Active still physically holding all
	// of the group while three peers were told to bring the same addresses up,
	// so the switch produced ~150 duplicated addresses immediately; and it ran
	// those bring-ups under s.Lock(), stalling this daemon long enough for peers
	// to mark it unreachable — which made each of them appoint itself
	// coordinator and redistribute too (docs/TEST-PLAN.md defects #2/#26).
	//
	// Seeding the current owner instead means the reconciler sees the addresses
	// as hosted, so it rebalances rather than re-places: every move goes through
	// OrchestrateIPFailover, which brings the address down on the source before
	// bringing it up on the destination.
	if req.Mode == "active-active" {
		if !s.seedActiveActiveAssignments() {
			s.logger.Warn("No active node found on switch to active-active; redistributing the whole group")
			var allIPs []string
			for _, ips := range s.config.Groups {
				allIPs = append(allIPs, ips...)
			}
			if err := s.memberList.RedistributeIPs(allIPs); err != nil {
				s.logger.Error("Failed to redistribute IPs", "error", err)
				// Continue anyway as the mode change is already saved
			}
		}
	}

	// If switching to active-passive, consolidate every floating IP onto a
	// single active node. In active-passive an Active node's IP monitor expects
	// *all* of the group IPs on its interfaces, so leaving more than one node
	// Active — the normal state in active-active — makes every node claim every
	// floating IP and ARP-fight over them. The whole consolidation is driven
	// from here, the node handling the request, so it happens exactly once
	// rather than once per node reacting to the mode change.
	if req.Mode == "active-passive" {
		activeNode := membership.ConsolidationTarget(s.memberList.MembersSnapshot(), s.leaderID)
		if activeNode == nil {
			s.logger.Warn("No eligible node found to become active during mode switch")
		} else {
			s.logger.Info("Consolidating floating IPs onto a single active node",
				"hostname", activeNode.Hostname, "node_id", activeNode.ID)

			// Demote every other node and release the IPs it holds.
			for _, member := range s.memberList.MembersSnapshot() {
				if member.ID == activeNode.ID {
					continue
				}

				// One acquisition: what this node held has to be read in the same
				// breath as clearing it, or a concurrent assignment lands between
				// the two and its addresses are dropped without being released.
				var heldIPs []string
				var wasActive bool
				member.UpdateClaim(func(current membership.Claim) (membership.Claim, bool) {
					heldIPs = current.ActiveIPs
					wasActive = current.Status == membership.StatusActive
					current.ActiveIPs = nil
					// Only Active nodes are demoted; a failed or maintenance node
					// keeps its status so it isn't falsely reported as healthy.
					if wasActive {
						current.Status = membership.StatusPassive
					}
					return current, true
				})

				if wasActive {
					s.logger.Info("Demoted node to passive for active-passive mode", "hostname", member.Hostname)
				}

				// Releasing a remote node's IPs is a blocking gRPC call, so it
				// waits until s.Lock() is dropped — an unreachable peer would
				// otherwise stall every other daemon operation. The local node
				// needs no call at all: refreshLocalMonitorExpectedIPs below
				// makes the monitor strip the IPs now that it isn't Active.
				if len(heldIPs) > 0 && !member.IsLocal() {
					demotions = append(demotions, pendingIPWork{member: member, ips: heldIPs})
				}
			}

			// config.Groups is the authoritative IP source: member.ActiveIPs is
			// empty on nodes promoted by election, and only groups actually
			// assigned to this node's interfaces can be brought up on it.
			activeIPs := s.groupIPsForNode(activeNode.ID)

			// Record the promotion now so the epoch bump, monitor refresh and state
			// broadcast below all see this node as the Active owner of these IPs, but
			// leave the bring-up itself until s.Lock() is dropped. Bringing up a large
			// group under the server lock stalled every other operation on this daemon
			// — including the health checks peers use to decide it is still alive
			// (docs/TEST-PLAN.md defects #4/#8) — and it claimed the addresses before
			// the demoted nodes had released them.
			// The claim only. MakeActive would bring the addresses up here too,
			// and the whole point of this site is that the bring-up waits until
			// s.Lock() is dropped -- see the comment above.
			activeNode.SetClaim(membership.Claim{
				Status:    membership.StatusActive,
				ActiveIPs: activeIPs,
			})
			if len(activeIPs) > 0 {
				activation = &pendingIPWork{member: activeNode, ips: activeIPs}
			}

			// In active-passive the active node is the leader; peers accept
			// this along with the bumped epoch below.
			s.leaderID = activeNode.ID
		}
	}

	s.logger.Infof("Successfully changed cluster mode to: %s", req.Mode)

	// Bump epoch by 2 so our health-check broadcasts (epoch+1) supersede any stale
	// broadcasts from peers that still carry the pre-switch epoch.
	s.clusterEpoch += 2

	// Update local monitor expectations based on new role
	s.refreshLocalMonitorExpectedIPs()

	// Hand the new mode to the broadcaster as well as sending it directly below.
	// The direct send is one unretried pass per peer with its result discarded, so
	// a peer that is briefly unreachable — a listener mid-rebind under defect #31
	// is enough — stayed in the old mode indefinitely, and a cluster running two
	// modes at once is the split-brain configuration quorum exists to prevent.
	// Stamping it makes the retrying broadcaster own eventual propagation
	// (defect #43's machinery), and a peer that already applied the direct send
	// answers the re-push with `config version already held` — an accept, since
	// it holds exactly this stamp. It deliberately does not answer `superseded
	// config version ignored`: that reply makes the broadcaster warn that this
	// node's change will be reverted, which would have been false on every mode
	// switch, once per peer.
	s.markConfigDirty()

	// Snapshot the decision while still holding the lock, so the goroutine below
	// broadcasts exactly what was decided here rather than re-reading state that
	// a health check may have moved on in the meantime.
	switchStates := getStatusMap()
	for id, member := range s.memberList.MembersSnapshot() {
		switchStates[id] = member.GetStatus()
	}
	switchEpoch := s.clusterEpoch
	switchLeader := s.leaderID

	// The goroutine runs after s.Lock() is released via the deferred s.Unlock():
	// both the broadcast and the IP work below make blocking gRPC calls, and
	// holding the server lock across those stalled every other operation on this
	// daemon — including the health checks peers use to decide it is still alive
	// (docs/TEST-PLAN.md defects #4/#8).
	go func() {
		// Propagate the new mode and the statuses it implies together, and before
		// any address moves. Peers that know only half of it behave as if the
		// switch never happened: still in active-active, still Active, still
		// willing to act as active-active coordinator and consolidate the group
		// somewhere else (docs/TEST-PLAN.md defect #27). Demoted peers also then
		// release on their own, from their own IP monitors, rather than waiting on
		// the serial BringDownIPs calls below.
		s.broadcastConfigAndStates(switchStates, switchEpoch, switchLeader)
		putStatusMap(switchStates)

		for _, demotion := range demotions {
			if err := demotion.member.BringDownIPs(demotion.ips); err != nil {
				s.logger.Warn("Failed to release IPs from demoted node during mode switch",
					"hostname", demotion.member.Hostname, "error", err)
			}
		}

		// Claim only after the releases above, so the group is not briefly up on both
		// the old and the new owner.
		if activation != nil {
			if err := activation.member.BringUpIPs(activation.ips); err != nil {
				s.logger.Error("Failed to bring up IPs on active node during mode switch",
					"hostname", activation.member.Hostname, "error", err)
			}
		}
	}()

	return &rpc.SetModeResponse{
		Success: true,
		Message: fmt.Sprintf("cluster mode changed to %s", req.Mode),
	}, nil
}

// seedActiveActiveAssignments records the Active node as the owner of every
// group IP it can host, and clears the rest. It reports whether an Active node
// was found; if not, nothing holds the group and the caller must place it.
//
// Callers must hold s.Lock() or s.RLock(): this reads the config maps through
// groupIPsForNode, whose contract requires it. It takes no server lock of its own
// — the lock is not reentrant and SetMode calls this with the write lock held —
// and the member writes below take only the individual member locks.
func (s *Server) seedActiveActiveAssignments() bool {
	members := s.memberList.MembersSnapshot()

	var owner *membership.Member
	for _, member := range members {
		isActive := member.GetStatus() == membership.StatusActive
		if isActive {
			owner = member
			break
		}
	}
	if owner == nil {
		return false
	}

	// config.Groups is the authoritative IP source. member.ActiveIPs is nil when
	// the Active node was promoted via election (elections set StatusActive but
	// never populate ActiveIPs), which is exactly the case that made the whole
	// group look orphaned to the reconciler.
	ownedIPs := s.groupIPsForNode(owner.ID)
	s.logger.Info("Seeding active-active assignments from the current owner",
		"hostname", owner.Hostname, "ip_count", len(ownedIPs))

	for _, member := range members {
		// The assignment map only: this seeds who holds what, and says nothing
		// about any member's status.
		if member.ID == owner.ID {
			member.SetActiveIPs(ownedIPs)
		} else {
			member.SetActiveIPs(nil)
		}
	}
	return true
}

// leastLoadedNodeForGroup picks the single node that should host a newly added
// address in active-active: of the healthy nodes with the group assigned to one
// of their interfaces, the one holding the fewest floating IPs, ties broken by
// node ID.
//
// This is deliberately the same rule as the coordinator's active-active
// distribution (see MemberList.calculateIPDistribution), so placing an address at
// add time does not fight the next rebalance. Nodes in maintenance or of unknown
// health are excluded — the address would only come straight back off.
//
// Returns "" when no node is eligible, which the caller reads as "place it later"
// rather than "place it everywhere". Callers must hold s.Lock().
func (s *Server) leastLoadedNodeForGroup(groupName string) string {
	members := s.memberList.MembersSnapshot()

	best := ""
	bestCount := -1
	for nodeID, node := range s.config.Nodes {
		if node == nil || !nodeHostsGroup(node, groupName) {
			continue
		}
		member := members[nodeID]
		if member == nil {
			continue
		}
		// One read, not two: placing an address against a status from before a
		// write and a count from after it is how a node gets given work it has
		// just stopped being eligible for.
		claim := member.Claim()
		status := claim.Status
		count := len(claim.ActiveIPs)
		if status == membership.StatusUnknown || status == membership.StatusMaintenance {
			continue
		}
		if bestCount == -1 || count < bestCount || (count == bestCount && nodeID < best) {
			best, bestCount = nodeID, count
		}
	}
	return best
}

// nodeHostsGroup reports whether the group is assigned to any of the node's
// interfaces, which is what makes the node able to bring its addresses up.
func nodeHostsGroup(node *config.Node, groupName string) bool {
	for _, groups := range node.IPGroups {
		for _, g := range groups {
			if g == groupName {
				return true
			}
		}
	}
	return false
}

// groupIPsForNode returns every configured group IP the given node can host —
// the IPs of the groups assigned to one of its interfaces. Group IPs the node
// cannot host are logged: in active-passive they have nowhere else to go.
// Callers must hold s.Lock().
func (s *Server) groupIPsForNode(nodeID string) []string {
	node := s.config.Nodes[nodeID]
	if node == nil {
		s.logger.Warn("No node configuration found when collecting group IPs", "node_id", nodeID)
		return nil
	}

	assigned := make(map[string]bool)
	var ips []string
	for _, groups := range node.IPGroups {
		for _, group := range groups {
			if assigned[group] {
				continue
			}
			assigned[group] = true
			ips = append(ips, s.config.Groups[group]...)
		}
	}

	for group, groupIPs := range s.config.Groups {
		if !assigned[group] && len(groupIPs) > 0 {
			s.logger.Warn("Group is not assigned to an interface on the active node; its IPs stay down",
				"group", group, "node_id", nodeID)
		}
	}

	return ips
}

// CreateGroup implements the CLI.CreateGroup RPC method
func (s *Server) CreateGroup(ctx context.Context, req *rpc.CreateGroupRequest) (*rpc.CreateGroupResponse, error) {
	s.logger.Infof("Received CreateGroup request for group: %s (caller: %s)", req.Name, callerAddr(ctx))
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.CreateGroupResponse{Success: false, Message: "no cluster configured"}, nil
	}

	// Check if group already exists
	if _, exists := s.config.Groups[req.Name]; exists {
		s.logger.Infof("Group %s already exists; treating as success", req.Name)
		return &rpc.CreateGroupResponse{
			Success: true,
			Message: fmt.Sprintf("group %s already exists", req.Name),
		}, nil
	}

	// Initialize Groups map if it doesn't exist
	if s.config.Groups == nil {
		s.config.Groups = make(map[string][]string)
	}

	// Create new empty group
	s.config.Groups[req.Name] = make([]string, 0)

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.CreateGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}
	// Broadcast updated config to peers
	s.markConfigDirty()

	s.logger.Infof("Successfully created group: %s", req.Name)
	return &rpc.CreateGroupResponse{
		Success: true,
		Message: fmt.Sprintf("group %s created successfully", req.Name),
	}, nil
}

// AddIPToGroup implements the CLI.AddIPToGroup RPC method
func (s *Server) AddIPToGroup(ctx context.Context, req *rpc.AddIPToGroupRequest) (*rpc.AddIPToGroupResponse, error) {
	s.logger.Infof("Received AddIPToGroup request for group: %s, IP: %s (caller: %s)", req.GroupName, req.Ip, callerAddr(ctx))
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.AddIPToGroupResponse{Success: false, Message: "no cluster configured"}, nil
	}

	// Check if group exists
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		return &rpc.AddIPToGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s does not exist", req.GroupName),
		}, nil
	}

	// Validate IP address and ensure it has a subnet mask
	ipToUse := req.Ip
	var warnings []string

	// A floating IP has exactly one owner in either mode, so the bring-up below
	// goes to exactly one node. In active-passive that is the Active node. In
	// active-active it is the least-loaded node the group is assigned to.
	//
	// Active-active used to have no gate at all: the fan-out visited every node
	// hosting the group and each one brought the same address up and appended it to
	// its own ActiveIPs, so a new address started life dual-homed and the
	// coordinator's next pass had to unwind it. Placing it once here is the same
	// rule the coordinator applies (fewest current IPs, ties by node ID), so the
	// two agree and there is nothing to unwind. If they disagree — the snapshot
	// read here is a moment old — the coordinator still moves it, which is an
	// ordinary rebalance rather than a duplicate.
	activePassive := s.config.Pulse.Mode == "active-passive"
	ownerID := ""
	if activePassive {
		for id, m := range s.memberList.MembersSnapshot() {
			if m.GetStatus() == membership.StatusActive {
				ownerID = id
				break
			}
		}
		if ownerID == "" {
			warnings = append(warnings, "No active node currently; IP will be enforced when a node becomes active")
		}
	} else {
		ownerID = s.leastLoadedNodeForGroup(req.GroupName)
		if ownerID == "" {
			warnings = append(warnings, "No node is currently able to host this group; IP will be placed when one is")
		}
	}

	// Check if it's already in CIDR notation
	if !utils.IsCIDR(req.Ip) {
		if utils.IsIPv4(req.Ip) {
			ipToUse = req.Ip + "/32" // Default to single host for IPv4
			warnings = append(warnings, fmt.Sprintf("No subnet mask provided, using %s", ipToUse))
		} else if utils.IsIPv6(req.Ip) {
			ipToUse = req.Ip + "/128" // Default to single host for IPv6
			warnings = append(warnings, fmt.Sprintf("No subnet mask provided, using %s", ipToUse))
		} else {
			return &rpc.AddIPToGroupResponse{
				Success: false,
				Message: fmt.Sprintf("invalid IP address: %s", req.Ip),
			}, nil
		}
	}

	// Check if IP already exists in configuration
	alreadyInSameGroup := false
	for g, ips := range s.config.Groups {
		for _, existingIP := range ips {
			if existingIP == ipToUse {
				if g == req.GroupName {
					alreadyInSameGroup = true
					break
				}
				return &rpc.AddIPToGroupResponse{
					Success: false,
					Message: fmt.Sprintf("IP %s already exists in group %s", ipToUse, g),
				}, nil
			}
		}
		if alreadyInSameGroup {
			break
		}
	}
	// If already configured in this group, treat as idempotent success without touching interfaces
	if alreadyInSameGroup {
		s.logger.Infof("IP %s already configured in group %s; treating as success", ipToUse, req.GroupName)
		return &rpc.AddIPToGroupResponse{
			Success:  true,
			Message:  fmt.Sprintf("IP %s already exists in group %s", ipToUse, req.GroupName),
			Warnings: warnings,
		}, nil
	}

	// Commit the configuration before touching a single interface.
	//
	// docs/TEST-PLAN.md defect #39: the bring-up below fans out to every node the
	// group is assigned to, costing ~4s per peer and ~28s when one is unreachable
	// (defect #37) — against the 30s deadline Client.Send puts on every CLI call,
	// which `group add-ip` does not override. When that deadline fired the caller
	// got rc=1 while this handler carried on to append, Save and broadcast, so a
	// failure was reported for a mutation that had in fact been applied and a
	// non-zero add could not be excluded from an expected count.
	//
	// Checking ctx instead is not the fix and is arguably worse: aborting
	// mid-fan-out leaves the address up on some nodes and absent from the config.
	// The config is the record of intent and the IP monitor's ENFORCE pass is what
	// puts the address on an interface, so committing first is what makes the
	// returned status describe the committed state.
	s.config.Groups[req.GroupName] = append(s.config.Groups[req.GroupName], ipToUse)
	if err := s.config.Save(); err != nil {
		// Roll the append back so the in-memory config still matches the disk
		// we failed to write, and nothing broadcasts a change that did not land.
		group := s.config.Groups[req.GroupName]
		s.config.Groups[req.GroupName] = group[:len(group)-1]
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.AddIPToGroupResponse{
			Success:  false,
			Message:  fmt.Sprintf("failed to save config: %v", err),
			Warnings: warnings,
		}, nil
	}
	// Broadcast updated config to peers
	s.markConfigDirty()

	// Bring the address up locally now — a netlink add with no announcement, so
	// it is cheap — and collect the peers for the asynchronous fan-out below.
	localBroughtUp := false
	var peerTargets []peerBringUpTarget
	for nodeID, node := range s.config.Nodes {
		for iface, groups := range node.IPGroups {
			for _, g := range groups {
				if g != req.GroupName {
					continue
				}
				// Only the owner brings the address up. Every other node hosting
				// the group keeps it in config and nothing else, so the address is
				// never up in two places. With no owner resolvable, nobody brings
				// it up and the IP monitor's ENFORCE pass places it once a node is
				// eligible.
				if ownerID == "" || nodeID != ownerID {
					continue
				}
				if nodeID != s.config.Pulse.LocalNode {
					// Snapshot the endpoint: the fan-out runs outside s.Lock().
					peerTargets = append(peerTargets, peerBringUpTarget{
						hostname: node.Hostname,
						ip:       node.IP,
						port:     node.Port,
						iface:    iface,
					})
					continue
				}

				// This is the local node, bring up the IP locally
				s.logger.Infof("Bringing up IP %s on interface %s", ipToUse, iface)

				// Check if interface exists
				exists, _ := network.InterfaceExist(iface)
				if !exists {
					warnings = append(warnings, fmt.Sprintf("Interface %s does not exist on local node", iface))
					continue
				}

				// Unconditionally add the IP to the expected IPs for the local monitor
				s.ipMonitor.AddExpectedIPs(iface, []string{ipToUse})

				// Check if IP is already present; treat as success if on target iface
				ipObj, _ := utils.GetCIDR(ipToUse)
				if ipObj != nil {
					exists, existingIface, err := network.CheckIfIPExists(ipObj.String())
					if err != nil {
						warnings = append(warnings, fmt.Sprintf("Failed to check if IP exists: %v", err))
						continue
					}
					if exists {
						if existingIface == iface {
							// Already configured on desired iface; mark success and update expected IPs
							localBroughtUp = true
							s.logger.Infof("IP %s already present on interface %s; treating as success", ipToUse, iface)
							continue
						}
						// Present on a different iface; try to bring it down there first
						if derr := network.BringIPdown(existingIface, ipToUse); derr != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to remove existing IP %s from interface %s: %v", ipToUse, existingIface, derr))
							continue
						}
					}
				}

				if err := network.BringIPup(iface, ipToUse); err != nil {
					warnings = append(warnings, fmt.Sprintf("Failed to bring up IP %s on interface %s: %v", ipToUse, iface, err))
					continue
				}
				localBroughtUp = true
				s.logger.Infof("Successfully brought up IP %s on interface %s", ipToUse, iface)
			}
		}
	}

	// Track the IP on the local member so status and the active-active
	// reconciler see it as hosted — otherwise it is treated as orphaned and
	// endlessly re-redistributed. Mirrors what the BringUpIP RPC handler does
	// for remote nodes, including the transition to Active in active-active.
	if localBroughtUp {
		if member := s.memberList.GetMemberByID(s.config.Pulse.LocalNode); member != nil {
			// Read before the decision runs, not inside it: the closure holds the
			// member lock, and the less it reaches for from there the better.
			activeActiveMode := s.config.Pulse.Mode == "active-active"
			member.UpdateClaim(func(current membership.Claim) (membership.Claim, bool) {
				current = current.WithAddresses(ipToUse)
				// A node given an address in active-active is serving, so it
				// stops being Passive. Only from Passive: a node in maintenance
				// or one not answering must not be promoted by an address
				// landing on it.
				if activeActiveMode && current.Status == membership.StatusPassive {
					current.Status = membership.StatusActive
				}
				return current, true
			})
		}
	}

	// Announce to the peers off the request path. Waiting on this was the whole
	// of defect #39 (see the commit-first comment above) and the whole of #37's
	// ~13s per add: the calls are independent, so they run concurrently and on a
	// context of their own — the caller's is cancelled the moment we return.
	// Queued rather than sent: a burst of adds becomes one request per peer per
	// window instead of one per address, which is #37's remainder. See
	// peerBringUpBatcher.
	if len(peerTargets) > 0 {
		batcher := s.peerBringUpQueue()
		for _, target := range peerTargets {
			batcher.Add(target, ipToUse)
		}
	}

	s.logger.Infof("Successfully added IP %s to group %s", ipToUse, req.GroupName)
	return &rpc.AddIPToGroupResponse{
		Success:  true,
		Message:  fmt.Sprintf("successfully added IP %s to group %s", ipToUse, req.GroupName),
		Warnings: warnings,
	}, nil
}

// peerBringUpQueue returns the batcher, creating it on first use.
//
// Lazily, and on its own mutex rather than s.Lock(): the only caller holds
// s.Lock() already, and every Server in the tests is built as a literal rather
// than through NewServer, so a field that has to be initialised at construction
// would be nil in exactly the paths that exercise this.
func (s *Server) peerBringUpQueue() *peerBringUpBatcher {
	s.peerBringUpMu.Lock()
	defer s.peerBringUpMu.Unlock()
	if s.peerBringUp == nil {
		s.peerBringUp = newPeerBringUpBatcher(peerBringUpWindow, s.sendPeerBringUpBatch)
	}
	return s.peerBringUp
}

// sendPeerBringUpBatch is the batcher's send hook, kept separate so the batching
// and the RPC fan-out can be tested apart from each other.
func (s *Server) sendPeerBringUpBatch(target peerBringUpTarget, ips []string) {
	s.bringUpGroupIPOnPeers([]peerBringUpTarget{target}, ips)
}

// peerBringUpTarget is one peer bring-up for a newly configured group address,
// snapshotted under s.Lock() because the fan-out that consumes it does not hold
// the lock and must not walk s.config.Nodes.
type peerBringUpTarget struct {
	hostname string
	ip       string
	port     string
	iface    string
}

// bringUpGroupIPOnPeers asks every peer holding the group to bring a newly
// configured address up, concurrently and outside the request that added it.
//
// It is best-effort by design: the address is already committed to the config
// and broadcast, so each peer's IP monitor converges on it regardless — this
// only shortens the wait for the ENFORCE pass. Failures are therefore logged
// rather than returned, and a single unreachable peer no longer decides the
// latency of an add (docs/TEST-PLAN.md defects #37 and #39).
func (s *Server) bringUpGroupIPOnPeers(targets []peerBringUpTarget, ips []string) {
	if len(ips) == 0 {
		return
	}

	// Sized to the batch, not flat: the callee announces the whole set and a
	// deadline that fits one address reports a batch that succeeded as a failure
	// (defect #57, the same mistake on the bring-up side).
	ctx, cancel := context.WithTimeout(context.Background(), bringUpTimeoutFor(len(ips)))
	defer cancel()

	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target peerBringUpTarget) {
			defer wg.Done()

			s.logger.Infof("Sending request to bring up %d IP(s) on node %s", len(ips), target.hostname)
			remoteClient, err := client.New()
			if err != nil {
				s.logger.Warn("Failed to create client to bring up new group IPs",
					"count", len(ips), "node", target.hostname, "error", err)
				return
			}
			defer remoteClient.Close()

			if err := remoteClient.Connect(target.ip, target.port, false); err != nil {
				s.logger.Warn("Failed to connect to peer to bring up new group IPs",
					"count", len(ips), "node", target.hostname, "error", err)
				return
			}

			resp, err := remoteClient.Server().BringUpIP(ctx, &rpc.UpIpRequest{
				Iface: target.iface,
				Ips:   ips,
			})
			switch {
			case err != nil:
				s.logger.Warn("Failed to bring up new group IPs on peer; its IP monitor will converge",
					"count", len(ips), "ips", ips, "node", target.hostname, "error", err)
			case !resp.Success:
				s.logger.Warn("Peer refused to bring up new group IPs; its IP monitor will converge",
					"count", len(ips), "ips", ips, "node", target.hostname, "message", resp.Message)
			default:
				s.logger.Infof("Successfully brought up %d IP(s) on node %s", len(ips), target.hostname)
			}
		}(target)
	}
	wg.Wait()
}

// RemoveIPFromGroup implements the CLI.RemoveIPFromGroup RPC method
func (s *Server) RemoveIPFromGroup(ctx context.Context, req *rpc.RemoveIPFromGroupRequest) (*rpc.RemoveIPFromGroupResponse, error) {
	s.logger.Infof("Received RemoveIPFromGroup request for group: %s, IP: %s (caller: %s)", req.GroupName, req.Ip, callerAddr(ctx))
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.RemoveIPFromGroupResponse{Success: false, Message: "no cluster configured"}, nil
	}

	// Check if group exists
	group, exists := s.config.Groups[req.GroupName]
	if !exists {
		return &rpc.RemoveIPFromGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s does not exist", req.GroupName),
		}, nil
	}

	// Validate IP address and ensure it has a subnet mask
	ipToUse := req.Ip
	var warnings []string

	// Check if it's already in CIDR notation
	if !utils.IsCIDR(req.Ip) {
		if utils.IsIPv4(req.Ip) {
			ipToUse = req.Ip + "/32" // Default to single host for IPv4
			warnings = append(warnings, fmt.Sprintf("No subnet mask provided, using %s", ipToUse))
		} else if utils.IsIPv6(req.Ip) {
			ipToUse = req.Ip + "/128" // Default to single host for IPv6
			warnings = append(warnings, fmt.Sprintf("No subnet mask provided, using %s", ipToUse))
		} else {
			return &rpc.RemoveIPFromGroupResponse{
				Success: false,
				Message: fmt.Sprintf("invalid IP address: %s", req.Ip),
			}, nil
		}
	}

	// Find and remove IP from group
	found := false
	var newIPs []string
	var foundExactIP string
	for _, existingIP := range group {
		if existingIP == ipToUse {
			found = true
			foundExactIP = existingIP
			continue
		}
		newIPs = append(newIPs, existingIP)
	}

	if !found {
		s.logger.Infof("IP %s not present in group %s; treating as success", ipToUse, req.GroupName)
		return &rpc.RemoveIPFromGroupResponse{
			Success: true,
			Message: fmt.Sprintf("IP %s not found in group %s", ipToUse, req.GroupName),
		}, nil
	}

	// Find nodes that have this group assigned and bring down the IP
	ipBroughtDown := false
	for nodeID, node := range s.config.Nodes {
		for iface, groups := range node.IPGroups {
			for _, g := range groups {
				if g == req.GroupName {
					// Check if this is the local node
					if nodeID == s.config.Pulse.LocalNode {
						// This is the local node, bring down the IP locally
						s.logger.Infof("Bringing down IP %s on interface %s", foundExactIP, iface)

						// Check if interface exists
						exists, _ := network.InterfaceExist(iface)
						if !exists {
							warnings = append(warnings, fmt.Sprintf("Interface %s does not exist on local node", iface))
							continue
						}

						if err := network.BringIPdown(iface, foundExactIP); err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to bring down IP %s on interface %s: %v", foundExactIP, iface, err))
							// Continue anyway, as we want to remove the IP from config
						} else {
							ipBroughtDown = true
							s.logger.Infof("Successfully brought down IP %s on interface %s", foundExactIP, iface)
							if s.ipMonitor != nil {
								s.ipMonitor.RemoveExpectedIPs(iface, []string{foundExactIP})
							}
						}
					} else {
						// This is a remote node, send RPC to bring down the IP
						s.logger.Infof("Sending request to bring down IP %s on node %s", foundExactIP, node.Hostname)
						remoteClient, err := client.New()
						if err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to create client for node %s: %v", node.Hostname, err))
							continue
						}
						defer remoteClient.Close()

						// Connect to remote node
						if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to connect to node %s: %v", node.Hostname, err))
							continue
						}

						// Send request to bring down IP
						resp, err := remoteClient.Server().BringDownIP(ctx, &rpc.DownIpRequest{
							Iface: iface,
							Ips:   []string{foundExactIP},
						})

						if err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to bring down IP %s on node %s: %v", foundExactIP, node.Hostname, err))
							// Continue anyway, as we want to remove the IP from config
						} else if !resp.Success {
							warnings = append(warnings, fmt.Sprintf("Failed to bring down IP %s on node %s: %s", foundExactIP, node.Hostname, resp.Message))
							// Continue anyway, as we want to remove the IP from config
						} else {
							ipBroughtDown = true
							s.logger.Infof("Successfully brought down IP %s on node %s", foundExactIP, node.Hostname)
						}
					}
				}
			}
		}
	}

	// Update group in config - ensure we always use an empty slice instead of null
	if newIPs == nil {
		newIPs = make([]string, 0)
	}
	s.config.Groups[req.GroupName] = newIPs

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.RemoveIPFromGroupResponse{
			Success:  false,
			Message:  fmt.Sprintf("failed to save config: %v", err),
			Warnings: warnings,
		}, nil
	}
	// Broadcast updated config to peers
	s.markConfigDirty()

	// If we couldn't bring down the IP on any node but it was in the config, add a warning
	if !ipBroughtDown && len(warnings) > 0 {
		warnings = append(warnings, "IP was removed from configuration but could not be brought down on any node. You may need to manually remove the IP from interfaces if it's still active.")
	}

	s.logger.Infof("Successfully removed IP %s from group %s", ipToUse, req.GroupName)
	return &rpc.RemoveIPFromGroupResponse{
		Success:  true,
		Message:  fmt.Sprintf("successfully removed IP %s from group %s", ipToUse, req.GroupName),
		Warnings: warnings,
	}, nil
}

// AssignGroupToNode implements the CLI.AssignGroupToNode RPC method
func (s *Server) AssignGroupToNode(ctx context.Context, req *rpc.AssignGroupRequest) (*rpc.AssignGroupResponse, error) {
	s.logger.Infof("Received AssignGroupToNode request for group: %s", req.GroupName)
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.AssignGroupResponse{Success: false, Message: "no cluster configured"}, nil
	}

	// Validate group
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		return &rpc.AssignGroupResponse{Success: false, Message: fmt.Sprintf("group %s does not exist", req.GroupName)}, nil
	}

	// Find node by node ID
	var nodeFound bool
	var node *config.Node
	if n, ok := s.config.Nodes[req.NodeId]; ok {
		nodeFound = true
		node = n
	}

	if !nodeFound || node == nil {
		return &rpc.AssignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("node_id %s not found", req.NodeId),
		}, nil
	}

	// Initialize IPGroups map if needed
	if node.IPGroups == nil {
		node.IPGroups = make(map[string][]string)
	}

	// Check if group is already assigned to this interface (idempotent success)
	for _, g := range node.IPGroups[req.Interface] {
		if g == req.GroupName {
			s.logger.Infof("Group %s already assigned to %s on node %s; treating as success", req.GroupName, req.Interface, req.NodeId)
			return &rpc.AssignGroupResponse{
				Success: true,
				Message: fmt.Sprintf("group %s already assigned to interface %s on node %s", req.GroupName, req.Interface, req.NodeId),
			}, nil
		}
	}

	// Add group to interface
	node.IPGroups[req.Interface] = append(node.IPGroups[req.Interface], req.GroupName)

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.AssignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}
	// Broadcast updated config to peers
	s.markConfigDirty()

	// No refresh call here: the config broadcast above lands as a ConfigSync on every
	// peer including this node's own reconcile path, and the health checker drives VIP
	// reconciliation off that.

	// If assigning on the local node, refresh expected IPs for this interface
	if s.ipMonitor != nil {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil && req.NodeId == localID {
			node := s.config.Nodes[localID]
			if node != nil {
				iface := req.Interface
				ifaceIPs := s.expectedIfaceIPs(localID, iface)
				s.ipMonitor.ClearExpectedIPs(iface)
				if len(ifaceIPs) > 0 {
					s.ipMonitor.UpdateExpectedIPs(iface, ifaceIPs)
				}
			}
		}
	}

	s.logger.Infof("Successfully assigned group %s to interface %s on node %s", req.GroupName, req.Interface, req.NodeId)
	return &rpc.AssignGroupResponse{
		Success: true,
		Message: fmt.Sprintf("successfully assigned group %s to interface %s on node %s", req.GroupName, req.Interface, req.NodeId),
	}, nil
}

// Temporary structs for new RPC methods (until protobuf is regenerated)
type UnassignGroupRequest struct {
	GroupName string
	NodeID    string
	Interface string
}

type UnassignGroupResponse struct {
	Success bool
	Message string
}

type DeleteGroupRequest struct {
	GroupName string
	Force     bool
}

type DeleteGroupResponse struct {
	Success  bool
	Message  string
	Warnings []string
}

// UnassignGroupFromNode implements the CLI.UnassignGroupFromNode RPC method
func (s *Server) UnassignGroupFromNode(ctx context.Context, req *rpc.UnassignGroupRequest) (*rpc.UnassignGroupResponse, error) {
	s.logger.Infof("Received UnassignGroupFromNode request for group: %s", req.GroupName)
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.UnassignGroupResponse{Success: false, Message: "no cluster configured"}, nil
	}

	// Validate group
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		return &rpc.UnassignGroupResponse{Success: false, Message: fmt.Sprintf("group %s does not exist", req.GroupName)}, nil
	}

	// Enforce node_id-only lookup
	if req.NodeId == "" {
		return &rpc.UnassignGroupResponse{Success: false, Message: "missing node_id"}, nil
	}
	node, exists := s.config.Nodes[req.NodeId]
	if !exists || node == nil {
		return &rpc.UnassignGroupResponse{Success: false, Message: fmt.Sprintf("node_id %s not found", req.NodeId)}, nil
	}

	// Check if group is assigned to this interface
	if node.IPGroups == nil {
		// Nothing assigned; idempotent success
		s.logger.Infof("Group %s not assigned on %s for node %s; treating as success", req.GroupName, req.Interface, req.NodeId)
		return &rpc.UnassignGroupResponse{Success: true, Message: fmt.Sprintf("group %s is not assigned to interface %s on node %s", req.GroupName, req.Interface, req.NodeId)}, nil
	}

	// Find and remove the group from interface
	groups := node.IPGroups[req.Interface]
	groupIndex := -1
	for i, g := range groups {
		if g == req.GroupName {
			groupIndex = i
			break
		}
	}

	if groupIndex == -1 {
		// Already unassigned; idempotent success
		s.logger.Infof("Group %s already unassigned from %s on node %s; treating as success", req.GroupName, req.Interface, req.NodeId)
		return &rpc.UnassignGroupResponse{Success: true, Message: fmt.Sprintf("group %s is not assigned to interface %s on node %s", req.GroupName, req.Interface, req.NodeId)}, nil
	}

	// Remove group from slice
	node.IPGroups[req.Interface] = append(groups[:groupIndex], groups[groupIndex+1:]...)

	// If interface has no more groups, remove the entry
	if len(node.IPGroups[req.Interface]) == 0 {
		delete(node.IPGroups, req.Interface)
	}

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.UnassignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}
	// Broadcast updated config to peers
	s.markConfigDirty()

	// No refresh call here: the config broadcast above lands as a ConfigSync on every
	// peer including this node's own reconcile path, and the health checker drives VIP
	// reconciliation off that.

	// If unassigning on the local node, refresh expected IPs for this interface
	if s.ipMonitor != nil {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil && req.NodeId == localID {
			node := s.config.Nodes[localID]
			if node != nil {
				iface := req.Interface
				ifaceIPs := s.expectedIfaceIPs(localID, iface)
				s.ipMonitor.ClearExpectedIPs(iface)
				if len(ifaceIPs) > 0 {
					s.ipMonitor.UpdateExpectedIPs(iface, ifaceIPs)
				}
			}
		}
	}

	s.logger.Infof("Successfully unassigned group %s from interface %s on node %s", req.GroupName, req.Interface, req.NodeId)
	return &rpc.UnassignGroupResponse{
		Success: true,
		Message: fmt.Sprintf("successfully unassigned group %s from interface %s on node %s", req.GroupName, req.Interface, req.NodeId),
	}, nil
}

// DeleteGroup implements the CLI.DeleteGroup RPC method.
//
// A group that is assigned somewhere is deleted in two config writes rather than
// one: its assignments are dropped and committed, the addresses are then released
// on the nodes holding them, and only a release confirmed everywhere is followed
// by the delete itself. See beginGroupDeletion for why the ordering is the fix
// for docs/TEST-PLAN.md defect #59.
func (s *Server) DeleteGroup(ctx context.Context, req *rpc.DeleteGroupRequest) (*rpc.DeleteGroupResponse, error) {
	s.logger.Infof("Received DeleteGroup request for group: %s (caller: %s)", req.GroupName, callerAddr(ctx))

	done, targets, warnings := s.beginGroupDeletion(req)
	if done != nil {
		return done, nil
	}

	// The release runs outside s.Lock(): a fan-out to every node holding the
	// group is exactly the work that stops a node answering its own health
	// checks while it holds the lock, which is how a busy node gets elected
	// around (defects #4/#7/#8).
	if len(targets) > 0 {
		releaseWarnings, unconfirmed := s.releaseDeletedGroupIPs(ctx, targets)
		warnings = append(warnings, releaseWarnings...)

		if len(unconfirmed) > 0 {
			// Deleting over this is defect #59: the addresses stay up and the
			// group that referenced them is gone, so no enforce pass can ever
			// compute them as surplus again. Left configured-but-unassigned they
			// are still recoverable — every node's release pass takes its share
			// down when it can, and a retried delete finishes the job.
			s.logger.Error("Not deleting group: its addresses could not be confirmed released",
				"group", req.GroupName, "nodes", unconfirmed)
			return &rpc.DeleteGroupResponse{
				Success: false,
				Message: fmt.Sprintf("group %s was unassigned but NOT deleted: could not confirm "+
					"its floating IPs were released on %s. The addresses stay accounted for while "+
					"the group is still configured; retry the delete once those nodes are reachable",
					req.GroupName, strings.Join(unconfirmed, ", ")),
				Warnings: warnings,
			}, nil
		}
	}

	if err := s.commitGroupDeletion(req.GroupName); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.DeleteGroupResponse{
			Success:  false,
			Message:  fmt.Sprintf("failed to save config: %v", err),
			Warnings: warnings,
		}, nil
	}

	s.logger.Infof("Successfully deleted group %s", req.GroupName)
	return &rpc.DeleteGroupResponse{
		Success:  true,
		Message:  fmt.Sprintf("successfully deleted group %s", req.GroupName),
		Warnings: warnings,
	}, nil
}

// beginGroupDeletion runs the locked first half of a group deletion: it
// validates the request and, for an assigned group deleted with --force, drops
// every assignment and commits that as a write of its own, returning the release
// each node still owes.
//
// Dropping the assignments separately is what makes the release that follows
// possible at all. Removing them and the group in one write — the original
// behaviour — left the addresses referenced by nothing the moment it landed:
// surplusFloatingIPs scans only *configured* groups, deliberately, so a node
// still holding its share had it fall outside every set any enforce pass could
// compute, and it stayed up indefinitely (docs/TEST-PLAN.md defect #59).
// Configured-but-unassigned is the one state whose release pass is verified live
// (#58), so this ordering also means a node that misses the explicit release
// still converges on its own instead of stranding.
//
// The release cannot run from here — it is a fan-out to every node holding the
// group and this holds s.Lock() — so the plan is snapshotted for the caller.
// A non-nil response is the final answer and the caller must return it.
func (s *Server) beginGroupDeletion(req *rpc.DeleteGroupRequest) (*rpc.DeleteGroupResponse, []groupReleaseTarget, []string) {
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.DeleteGroupResponse{Success: false, Message: "no cluster configured"}, nil, nil
	}

	// Validate group exists (idempotent success if missing)
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		s.logger.Infof("Group %s does not exist; treating delete as success", req.GroupName)
		return &rpc.DeleteGroupResponse{Success: true, Message: fmt.Sprintf("group %s does not exist", req.GroupName)}, nil, nil
	}

	// Check if group is assigned to any nodes (unless force is true)
	var assignedNodes []string
	for _, node := range s.config.Nodes {
		for iface, groups := range node.IPGroups {
			for _, group := range groups {
				if group == req.GroupName {
					assignedNodes = append(assignedNodes, fmt.Sprintf("%s:%s", node.Hostname, iface))
				}
			}
		}
	}

	if len(assignedNodes) > 0 && !req.Force {
		return &rpc.DeleteGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s is assigned to nodes: %s. Use --force to delete anyway", req.GroupName, assignedNodes),
		}, nil, nil
	}

	// An unassigned group holds nothing anywhere, so there is nothing to release
	// and no reason to spend a second config write on it.
	if len(assignedNodes) == 0 {
		return nil, nil, nil
	}

	// Plan the release before the assignments go, since it is the assignments
	// that say which node holds what.
	targets := s.planGroupRelease(req.GroupName)

	var warnings []string
	for _, node := range s.config.Nodes {
		for iface := range node.IPGroups {
			groups := node.IPGroups[iface]
			for i := len(groups) - 1; i >= 0; i-- {
				if groups[i] == req.GroupName {
					// Remove group from slice
					node.IPGroups[iface] = append(groups[:i], groups[i+1:]...)
					warnings = append(warnings, fmt.Sprintf("removed assignment from %s:%s", node.Hostname, iface))
				}
			}
			// If interface has no more groups, remove the entry
			if len(node.IPGroups[iface]) == 0 {
				delete(node.IPGroups, iface)
			}
		}
	}

	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.DeleteGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil, nil
	}
	// Broadcast updated config to peers
	s.markConfigDirty()

	return nil, targets, warnings
}

// commitGroupDeletion removes the group from the config once its addresses are
// accounted for, and is the second of the two writes an assigned deletion makes.
func (s *Server) commitGroupDeletion(groupName string) error {
	s.Lock()
	defer s.Unlock()

	delete(s.config.Groups, groupName)
	if err := s.config.Save(); err != nil {
		return err
	}
	// Broadcast updated config to peers
	s.markConfigDirty()
	return nil
}

// groupReleaseTarget is one node's share of a group being deleted, snapshotted
// under s.Lock() because the release that consumes it runs without the lock and
// must not walk s.config.Nodes.
type groupReleaseTarget struct {
	nodeID   string
	hostname string
	ip       string
	port     string
	iface    string
	// ips is the share this node is recorded as holding, which is what gets
	// released.
	ips []string
	// candidates is every address of the group that could be up on this
	// interface. It is wider than ips on purpose: the record of who holds what
	// was append-only until defect #58, so the local node — the one node whose
	// interfaces can actually be read — verifies against this rather than trusting
	// the record it is about to delete.
	candidates []string
	local      bool
}

// planGroupRelease returns, per node and interface holding the group, the
// addresses that have to come down before the group can leave the config.
//
// expectedIfaceIPs answers "which of these addresses are mine" for a node, so in
// active-active each node is sent only its assigned share: asking a node to bring
// down a group it holds none of is defect #34's noise, 201 error lines for a
// no-op. In active-passive it yields the whole group, which is correct there —
// the Active holds all of it and a node that just changed role may still hold any
// of it.
//
// An address another still-configured group provides on the same interface is
// excluded. Nothing in the CLI can create that overlap (AddIPToGroup rejects an
// address already held by another group), but config.json is written by the
// appliance too (defect #3), and tearing down an address a live group still
// serves would be an outage.
//
// The caller must hold s.Lock() or s.RLock().
func (s *Server) planGroupRelease(groupName string) []groupReleaseTarget {
	groupIPs := s.config.Groups[groupName]
	if len(groupIPs) == 0 {
		return nil
	}

	var targets []groupReleaseTarget
	for nodeID, node := range s.config.Nodes {
		if node == nil {
			continue
		}
		for iface, groups := range node.IPGroups {
			if !slices.Contains(groups, groupName) {
				continue
			}

			mine := make(map[string]bool)
			for _, ip := range s.expectedIfaceIPs(nodeID, iface) {
				mine[ip] = true
			}
			retained := make(map[string]bool)
			for _, g := range groups {
				if g == groupName {
					continue
				}
				for _, ip := range s.config.Groups[g] {
					retained[ip] = true
				}
			}

			var ips, candidates []string
			for _, ip := range groupIPs {
				if retained[ip] {
					continue
				}
				candidates = append(candidates, ip)
				if mine[ip] {
					ips = append(ips, ip)
				}
			}
			// A node recorded as holding nothing is still visited when it is the
			// local one, because there the record can be checked against the
			// kernel instead of believed.
			if len(ips) == 0 && !(nodeID == s.config.Pulse.LocalNode && len(candidates) > 0) {
				continue
			}

			targets = append(targets, groupReleaseTarget{
				nodeID:     nodeID,
				hostname:   node.Hostname,
				ip:         node.IP,
				port:       node.Port,
				iface:      iface,
				ips:        ips,
				candidates: candidates,
				local:      nodeID == s.config.Pulse.LocalNode,
			})
		}
	}

	// Map iteration order is random; a deterministic plan keeps the logs and the
	// tests readable.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].nodeID != targets[j].nodeID {
			return targets[i].nodeID < targets[j].nodeID
		}
		return targets[i].iface < targets[j].iface
	})
	return targets
}

// releaseDeletedGroupIPs brings a deleted group's addresses down on every node
// holding them, concurrently and outside s.Lock(). It returns a warning per node
// that reported trouble, and the hostnames whose release could not be confirmed
// at all — the delete must not proceed over one of those.
func (s *Server) releaseDeletedGroupIPs(ctx context.Context, targets []groupReleaseTarget) (warnings []string, unconfirmed []string) {
	// Sized to the work, not fixed. A flat deadline on a batched bring-down is
	// defect #57 on the release side: it reports as failed a release that in fact
	// succeeded, and a false failure here costs the operator the delete. The
	// nodes run concurrently, so the largest single batch is what has to fit, and
	// the caller's deadline still caps the whole thing.
	largest := 0
	for _, target := range targets {
		if len(target.ips) > largest {
			largest = len(target.ips)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, membership.DemotionTimeoutFor(largest))
	defer cancel()

	var (
		mu pulselock.Mutex
		wg sync.WaitGroup
	)
	for _, target := range targets {
		wg.Add(1)
		go func(target groupReleaseTarget) {
			defer wg.Done()

			warning, confirmed := s.releaseGroupIPsOnTarget(ctx, target)

			mu.Lock()
			defer mu.Unlock()
			if warning != "" {
				warnings = append(warnings, warning)
			}
			if !confirmed {
				unconfirmed = append(unconfirmed, target.hostname)
			}
		}(target)
	}
	wg.Wait()

	sort.Strings(warnings)
	sort.Strings(unconfirmed)
	return warnings, unconfirmed
}

// releaseGroupIPsOnTarget brings one node's share of a deleted group down and
// reports whether the release can be treated as done.
func (s *Server) releaseGroupIPsOnTarget(ctx context.Context, target groupReleaseTarget) (warning string, confirmed bool) {
	s.logger.Info("Releasing floating IPs of a group being deleted",
		"node", target.hostname, "iface", target.iface, "count", len(target.ips))

	if target.local {
		return s.releaseGroupIPsLocally(ctx, target)
	}

	remoteClient, err := client.New()
	if err != nil {
		return fmt.Sprintf("failed to create client for node %s: %v", target.hostname, err), false
	}
	defer remoteClient.Close()

	if err := remoteClient.Connect(target.ip, target.port, false); err != nil {
		return fmt.Sprintf("failed to connect to node %s to release its floating IPs: %v", target.hostname, err), false
	}

	resp, err := remoteClient.Server().BringDownIP(ctx, &rpc.DownIpRequest{
		Iface: target.iface,
		Ips:   target.ips,
	})
	switch {
	case err != nil:
		return fmt.Sprintf("failed to release %d floating IP(s) on node %s: %v", len(target.ips), target.hostname, err), false
	case !resp.Success:
		return fmt.Sprintf("node %s refused to release %d floating IP(s): %s", target.hostname, len(target.ips), resp.Message), false
	}

	// A peer's per-address netlink failures are not visible here, and cannot be:
	// no RPC exposes a peer's interface state (the same wall defect #54 hit). They
	// are also the benign case — a failed bring-down is overwhelmingly "cannot
	// assign requested address", i.e. the address was already gone (#34). What
	// must not be waved through is the transport failing, which is what the two
	// cases above cover.
	return "", true
}

// releaseGroupIPsLocally brings the local node's share down through the same
// handler a peer would run, so the IP monitor's expectations and the member's
// assignment list stay honest (defect #58), and then checks the kernel rather
// than trusting the return — the lesson of #21.
func (s *Server) releaseGroupIPsLocally(ctx context.Context, target groupReleaseTarget) (warning string, confirmed bool) {
	// An address cannot be up on an interface the node does not have, so there is
	// nothing to release and nothing to strand.
	if exists, _ := network.InterfaceExist(target.iface); !exists {
		return fmt.Sprintf("interface %s does not exist on local node; nothing to release there", target.iface), true
	}

	if len(target.ips) > 0 {
		if _, err := s.BringDownIP(ctx, &rpc.DownIpRequest{Iface: target.iface, Ips: target.ips}); err != nil {
			return fmt.Sprintf("failed to release %d floating IP(s) locally: %v", len(target.ips), err), false
		}
	}

	// Keep the local assignment list honest whatever the mode: BringDownIP only
	// maintains it in active-active, and a deleted group's addresses left on the
	// list are reported as held forever, since nothing can recompute them
	// downward once the group is gone (defect #58).
	if member := s.memberList.GetMemberByID(target.nodeID); member != nil {
		member.RemoveActiveIPs(target.ips)
	}

	inventory, err := network.BuildIPInventory()
	if err != nil {
		// A host whose addresses cannot be read is a problem of its own, but
		// refusing the delete over it would leave no way to finish one. Say so
		// and take the release at its word.
		return fmt.Sprintf("released %d floating IP(s) locally but could not read interface state to confirm: %v",
			len(target.ips), err), true
	}

	// Checked over every address the group could have here, not only the ones the
	// record said to release. That record was append-only until defect #58 and is
	// about to be deleted along with the group, so the kernel is the authority for
	// the one node where it can be read — and an address it reports up is exactly
	// the strand this whole ordering exists to prevent.
	var stillHeld []string
	for _, ip := range target.candidates {
		addr, cerr := utils.GetCIDR(ip)
		if cerr != nil || addr == nil {
			continue
		}
		if held, _, eerr := inventory.Exists(addr.String()); eerr == nil && held {
			stillHeld = append(stillHeld, ip)
		}
	}
	if len(stillHeld) > 0 {
		return fmt.Sprintf("%d floating IP(s) of this group are still up locally on %s: %s",
			len(stillHeld), target.iface, strings.Join(stillHeld, ", ")), false
	}
	return "", true
}

// ListGroups implements the CLI.ListGroups RPC method
func (s *Server) ListGroups(ctx context.Context, req *rpc.ListGroupsRequest) (*rpc.ListGroupsResponse, error) {
	s.logger.Info("Received ListGroups request")
	s.RLock()
	defer s.RUnlock()

	if len(s.config.Groups) == 0 {
		return &rpc.ListGroupsResponse{
			Success: true,
			Message: "no IP groups configured",
			Groups:  []*rpc.GroupInfo{},
		}, nil
	}

	// If JSON output is requested, marshal the groups
	if req.JsonOutput {
		jsonData, err := json.MarshalIndent(s.config.Groups, "", "  ")
		if err != nil {
			s.logger.Error("Failed to marshal groups", "error", err)
			return &rpc.ListGroupsResponse{
				Success: false,
				Message: fmt.Sprintf("failed to marshal groups: %v", err),
			}, nil
		}
		return &rpc.ListGroupsResponse{
			Success:  true,
			Message:  "groups retrieved successfully",
			JsonData: string(jsonData),
		}, nil
	}

	// Otherwise, build structured response
	var groups []*rpc.GroupInfo
	for groupName, ips := range s.config.Groups {
		group := &rpc.GroupInfo{
			Name: groupName,
			Ips:  ips,
		}

		// Find assignments for this group
		for id, node := range s.config.Nodes {
			for iface, assignedGroups := range node.IPGroups {
				for _, g := range assignedGroups {
					if g == groupName {
						group.Assignments = append(group.Assignments, &rpc.GroupAssignment{
							Interface: iface,
							NodeId:    id,
						})
					}
				}
			}
		}

		groups = append(groups, group)
	}

	s.logger.Infof("Successfully retrieved %d groups", len(groups))
	return &rpc.ListGroupsResponse{
		Success: true,
		Message: "groups retrieved successfully",
		Groups:  groups,
	}, nil
}

// CreateCluster implements the CLI.CreateCluster RPC method
func (s *Server) CreateCluster(ctx context.Context, req *rpc.CreateClusterRequest) (*rpc.CreateClusterResponse, error) {
	s.logger.Infof("Received CreateCluster request with bindIP: %s, bindPort: %s, mode: %s", req.BindIp, req.BindPort, req.Mode)

	// Serialize with InitiateJoin to prevent a TOCTOU race where both pass
	// ClusterCheck() concurrently (both see 0 nodes) and both activate as
	// "first node", producing dual-active in active-passive mode.
	s.clusterInitMu.Lock()
	defer s.clusterInitMu.Unlock()

	// Check if cluster is already configured
	if s.config.ClusterCheck() {
		s.logger.Warn("CreateCluster rejected: cluster is already configured",
			"bind_ip", req.BindIp, "mode", req.Mode)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: "cluster is already configured",
		}, nil
	}

	// Set up initial node
	bindPort := req.BindPort
	if bindPort == "" {
		bindPort = "8080"
	}

	// Get hostname for certificates
	hostname, err := os.Hostname()
	if err != nil {
		s.logger.Error("Failed to get hostname", "error", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("failed to get hostname: %v", err),
		}, nil
	}

	// Generate certificates for mTLS
	if os.Getenv("PULSEHA_TEST") != "true" {
		if err := security.GenerateCertificates(hostname); err != nil {
			s.logger.Warn("Failed to generate certificates", "error", err)
			// Continue without TLS for now
		}
	} else {
		s.logger.Debug("PULSEHA_TEST=true: skipping certificate generation in CreateCluster")
	}

	// Generate a unique node ID
	nodeID := req.NodeId
	if nodeID == "" {
		nodeID = s.config.GenerateNodeID()
		s.logger.Infof("Generated node ID: %s", nodeID)
	} else {
		s.logger.Infof("Using provided node ID: %s", nodeID)
	}

	// Generate a cluster token for other nodes to join
	clusterToken := uuid.New().String()
	s.config.Pulse.ClusterToken = clusterToken
	s.logger.Infof("Generated cluster token: %s", clusterToken)

	// Add the node to config using generated ID
	s.config.Nodes[nodeID] = &config.Node{
		Hostname: hostname,
		IP:       req.BindIp,
		Port:     bindPort,
		IPGroups: make(map[string][]string),
	}

	// Set local node to the generated ID
	s.config.Pulse.LocalNode = nodeID

	// Set the cluster mode
	s.config.Pulse.Mode = req.Mode

	// Create default IP groups for each network interface
	interfaces, err := net.Interfaces()
	if err != nil {
		s.logger.Warn("Failed to get network interfaces", "error", err)
	} else {
		for _, iface := range interfaces {
			// Skip loopback, down interfaces, and interfaces without addresses
			if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
				continue
			}

			addrs, err := iface.Addrs()
			if err != nil {
				s.logger.Warn("Failed to get addresses for interface %s", "interface", iface.Name, "error", err)
				continue
			}

			if len(addrs) == 0 {
				continue
			}

			// Create a group for this interface
			groupName := fmt.Sprintf("default-%s", iface.Name)

			// Initialize the group if it doesn't exist
			if _, exists := s.config.Groups[groupName]; !exists {
				s.config.Groups[groupName] = []string{}
				s.logger.Infof("Created default IP group for interface %s", iface.Name)

				// Assign this group to the node's interface
				if s.config.Nodes[nodeID].IPGroups == nil {
					s.config.Nodes[nodeID].IPGroups = make(map[string][]string)
				}
				s.config.Nodes[nodeID].IPGroups[iface.Name] = append(s.config.Nodes[nodeID].IPGroups[iface.Name], groupName)
				s.logger.Infof("Assigned default IP group %s to interface %s on node %s", groupName, iface.Name, hostname)

				// Ensure monitor has a clean slate for this interface
				if s.ipMonitor != nil {
					s.ipMonitor.ClearExpectedIPs(iface.Name)
				}
			}
		}
	}

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	// Sync the updated config (with LocalNode set) into the member list before adding
	// members. Without this, the health checker sees LocalNode="" and treats the node
	// as remote, causing it to be marked Unknown after gRPC checks fail.
	s.memberList.UpdateConfig(s.config)

	// Add the first member to the member list
	if err := s.memberList.AddMember(nodeID, hostname, req.BindIp, bindPort); err != nil {
		s.logger.Error("Failed to add first node to member list", "error", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("failed to add first node to member list: %v", err),
		}, nil
	}

	// Make it active.
	//
	// Through the claim rather than a bare field write: CreateCluster clears the
	// expectation set a few lines below, which triggers an enforce pass in its own
	// goroutine, and that pass reads this member's claim. A direct assignment here
	// raced it -- `make testrace` caught this the moment ./tests/unit/... entered a
	// CI target, and only on Linux, since the reader lives in ip_monitor_linux.go.
	member := s.memberList.GetMemberByID(nodeID)
	if member != nil {
		member.UpdateClaim(func(current membership.Claim) (membership.Claim, bool) {
			current.Status = membership.StatusActive
			return current, true
		})
		s.logger.Info("First node activated successfully")
	}

	// Reconfigure the server to apply changes
	if err := s.Reconfigure(); err != nil {
		s.logger.Error("Failed to reconfigure server", "error", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("cluster created but failed to reconfigure server: %v", err),
		}, nil
	}

	// After successfully creating the cluster, start the health checker
	s.startHealthChecker()

	// Best-effort: wait briefly for the cluster listener to be ready to accept connections
	// This improves UX by ensuring the service is reachable immediately after successful creation
	finalPort := bindPort
	if bindPort == "0" {
		// Resolve actual bound port from config after Reconfigure
		if localNode, e := s.config.GetLocalNode(); e == nil {
			finalPort = localNode.Port
		}
	}
	// JoinHostPort rather than the FormatIPv6 + "%s:%s" idiom used for the
	// listener addresses elsewhere in this file: the two are equivalent for every
	// input (FormatIPv6 brackets a v6 literal and JoinHostPort brackets any host
	// containing a colon), but `go vet` cannot see through the helper and reports
	// every "%s:%s" reaching net.Dial as broken for IPv6. Silencing it here keeps
	// a real finding from being buried in known noise. Only the dial sites are
	// converted — the listener addresses are string-compared against each other
	// by clusterListenerServing, so they change together or not at all.
	address := net.JoinHostPort(utils.SanitizeIPv6(req.BindIp), finalPort)
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) {
		conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.logger.Info("Cluster created successfully")
	return &rpc.CreateClusterResponse{
		Success: true,
		Message: "cluster created successfully",
		Token:   clusterToken,
		NodeId:  nodeID,
	}, nil
}

// Token implements the CLI.Token RPC method
func (s *Server) Token(ctx context.Context, req *rpc.TokenRequest) (*rpc.TokenResponse, error) {
	s.logger.Infof("Received Token request with regenerate: %t", req.Regenerate)
	s.Lock()
	defer s.Unlock()

	// Check if cluster is configured
	if !s.config.ClusterCheck() {
		return &rpc.TokenResponse{
			Success: false,
			Message: "no cluster configured",
		}, nil
	}

	currentToken := s.config.Pulse.ClusterToken

	// If regenerate is false, just return the current token
	if !req.Regenerate {
		if currentToken == "" {
			return &rpc.TokenResponse{
				Success: false,
				Message: "no cluster token available",
			}, nil
		}
		return &rpc.TokenResponse{
			Success: true,
			Message: "current cluster token",
			Token:   currentToken,
		}, nil
	}

	// Generate new token
	newToken := uuid.New().String()
	if newToken == "" {
		return &rpc.TokenResponse{
			Success: false,
			Message: "failed to generate new token",
		}, nil
	}

	// Update the config
	s.config.Pulse.ClusterToken = newToken

	// TODO: Implement cluster-wide config synchronization
	// For now, the token will be updated on this node only
	// Future enhancement: sync with all cluster members

	// Save the configuration
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save configuration with new token", "error", err)
		return &rpc.TokenResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save new token: %v", err),
		}, nil
	}

	// Best-effort: broadcast updated token via ConfigSync to all peers
	configBytes, err := json.Marshal(s.config)
	if err == nil {
		localID, _ := s.config.GetLocalNodeUUID()
		for id, node := range s.config.Nodes {
			if id == localID {
				continue
			}
			remoteClient, err := s.getPeerClient(id, node)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: configBytes})
			cancel()
		}
	}

	s.logger.Infof("Successfully generated new cluster token")
	return &rpc.TokenResponse{
		Success: true,
		Message: "new cluster token generated",
		Token:   newToken,
	}, nil
}

// Quorum-related RPC method implementations that delegate to the quorum handler

// StartVotingSession delegates to the quorum handler
func (s *Server) StartVotingSession(ctx context.Context, req *rpc.StartVotingSessionRequest) (*rpc.StartVotingSessionResponse, error) {
	if s.quorumHandler == nil {
		return &rpc.StartVotingSessionResponse{
			Success: false,
			Message: "Quorum voting is not available",
		}, fmt.Errorf("quorum handler is not initialized")
	}
	return s.quorumHandler.StartVotingSession(ctx, req)
}

// CastVote delegates to the quorum handler
func (s *Server) CastVote(ctx context.Context, req *rpc.CastVoteRequest) (*rpc.CastVoteResponse, error) {
	if s.quorumHandler == nil {
		return &rpc.CastVoteResponse{
			Success: false,
			Message: "Quorum voting is not available",
		}, fmt.Errorf("quorum handler is not initialized")
	}
	return s.quorumHandler.CastVote(ctx, req)
}

// GetVotingResult delegates to the quorum handler
func (s *Server) GetVotingResult(ctx context.Context, req *rpc.GetVotingResultRequest) (*rpc.GetVotingResultResponse, error) {
	if s.quorumHandler == nil {
		return &rpc.GetVotingResultResponse{
			Success: false,
			Message: "Quorum voting is not available",
		}, fmt.Errorf("quorum handler is not initialized")
	}
	return s.quorumHandler.GetVotingResult(ctx, req)
}

// GetVotingSessions delegates to the quorum handler
func (s *Server) GetVotingSessions(ctx context.Context, req *rpc.GetVotingSessionsRequest) (*rpc.GetVotingSessionsResponse, error) {
	if s.quorumHandler == nil {
		return &rpc.GetVotingSessionsResponse{
			Success: false,
			Message: "Quorum voting is not available",
		}, fmt.Errorf("quorum handler is not initialized")
	}
	return s.quorumHandler.GetVotingSessions(ctx, req)
}

// GetVotingSessionDetails delegates to the quorum handler
func (s *Server) GetVotingSessionDetails(ctx context.Context, req *rpc.GetVotingSessionDetailsRequest) (*rpc.GetVotingSessionDetailsResponse, error) {
	if s.quorumHandler == nil {
		return &rpc.GetVotingSessionDetailsResponse{
			Success: false,
			Message: "Quorum voting is not available",
		}, fmt.Errorf("quorum handler is not initialized")
	}
	return s.quorumHandler.GetVotingSessionDetails(ctx, req)
}

// GetQuorumManager returns the quorum manager instance
func (s *Server) GetQuorumManager() *quorum.QuorumManager {
	return s.quorumManager
}

// ConfigSync handles configuration synchronization between nodes
func (s *Server) ConfigSync(ctx context.Context, req *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	s.logger.Debug("CONFIG_SYNC: Received configuration sync request", "configSize", len(req.Config))

	if req.Config == nil {
		s.logger.Warn("CONFIG_SYNC: No configuration data provided")
		return &rpc.ConfigSyncResponse{
			Success: false,
			Message: permanentRejectionPrefix + "no configuration data provided",
		}, nil
	}

	// Detect whether the incoming payload contains a full config (has "pulseha" root) or is an envelope
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(req.Config, &raw)
	isFullConfig := false
	if raw != nil {
		if _, ok := raw["pulseha"]; ok {
			isFullConfig = true
			s.logger.Debug("CONFIG_SYNC: Detected full config format (has pulseha root)")
		} else {
			s.logger.Debug("CONFIG_SYNC: Detected envelope format (no pulseha root)")
		}
		// Log what keys are present
		var keys []string
		for k := range raw {
			keys = append(keys, k)
		}
		s.logger.Debug("CONFIG_SYNC: Config contains keys", "keys", keys)
	}

	// Optionally read member states and convergence metadata if present (EnhancedConfig)
	var (
		incomingMemberStates map[string]membership.MemberStatus
		incomingEpoch        int64
		incomingLeaderID     string
		senderID             string
		senderActiveIPs      []string
		incomingStamp        configStamp
		incomingConfigHash   string
		repairFor            string
	)
	// Defer cleanup of any allocated maps
	defer func() {
		if incomingMemberStates != nil {
			putStatusMap(incomingMemberStates)
		}
	}()
	{
		type enhanced struct {
			MemberStates    map[string]int    `json:"member_states"`
			Epoch           *int64            `json:"epoch"`
			LeaderID        string            `json:"leader_id"`
			Leases          map[string]string `json:"leases"`
			SenderID        string            `json:"sender_id"`
			SenderActiveIPs []string          `json:"sender_active_ips"`
			ConfigVersion   int64             `json:"config_version"`
			ConfigOrigin    string            `json:"config_origin"`
			ConfigHash      string            `json:"config_hash"`
			RepairFor       string            `json:"repair_for"`
		}
		var e enhanced
		if err := json.Unmarshal(req.Config, &e); err == nil {
			if len(e.MemberStates) > 0 {
				incomingMemberStates = getStatusMap()
				for id, st := range e.MemberStates {
					incomingMemberStates[id] = membership.MemberStatus(st)
				}
			}
			if e.Epoch != nil {
				incomingEpoch = *e.Epoch
			}
			incomingLeaderID = e.LeaderID
			senderID = e.SenderID
			senderActiveIPs = e.SenderActiveIPs
			incomingConfigHash = e.ConfigHash
			repairFor = e.RepairFor
			// A binary that versions configs but does not name the origin gets the
			// sender treated as the origin — right for a direct mutation broadcast,
			// and the pre-origin behaviour otherwise.
			origin := e.ConfigOrigin
			if origin == "" {
				origin = e.SenderID
			}
			incomingStamp = configStamp{version: e.ConfigVersion, origin: origin}
		}
	}

	// Envelope syncs are the only continuous signal there is: they flow every few
	// seconds where a full config only follows a mutation, which is why a node that
	// missed one join broadcast stayed diverged until something unrelated changed
	// (docs/TEST-PLAN.md #103). A full config is not checked here -- it is about to
	// be arbitrated on its own merits, and comparing a hash it may be seconds from
	// adopting would pull a repair for a difference that is already being fixed.
	if !isFullConfig {
		s.noteConfigDivergence(senderID, incomingConfigHash)
	}

	// The epoch this node held *before* this sync is what decides whether the
	// payload carries a decision or merely re-asserts an already-agreed view.
	// Both branches below adopt a higher incoming epoch as soon as they see one,
	// so reading s.clusterEpoch after them compared the payload against the very
	// epoch it had just installed — never greater, so nothing was ever decisive
	// and every peer's view of the local node's own status was discarded,
	// including a real demotion (docs/TEST-PLAN.md defect #28).
	//
	// Read under the lock: this runs before the branch below takes it, so a bare
	// read races the config broadcaster and any concurrent sync.
	preSyncEpoch := s.GetClusterEpoch()

	if isFullConfig {
		// Drop a snapshot the sender has already superseded. ConfigSync applies a
		// carried group wholesale, and since defect #43 applies its absence too, so
		// an older snapshot delivered late used to overwrite a newer one with no way
		// back (docs/TEST-PLAN.md defect #5) — and now could delete from it. This
		// guard is what makes absence safe to honour: it is only ever read as a
		// removal on a payload strictly newer than what this node holds. Not an
		// error: the sender has nothing to fix, the message is simply obsolete.
		// A repair this node asked for is exempt from the generation guard, and has
		// to be: divergence at an *equal* generation is precisely what the guard
		// refuses and precisely what #103 is. The exemption is safe because it is
		// addressed -- honoured only when it names this node, so it cannot be
		// replayed elsewhere -- and because reaching it means this node had already
		// established, from the coordinator's own hash, that it disagreed.
		requestedRepair := false
		if repairFor != "" {
			if localID, err := s.config.GetLocalNodeUUID(); err == nil && localID != "" {
				requestedRepair = repairFor == localID
			}
		}
		if requestedRepair {
			s.logger.Info("CONFIG_SYNC: applying a repair this node requested",
				"sender", senderID, "version", incomingStamp.version)
		}

		if !requestedRepair && !s.shouldApplyIncomingConfig(incomingStamp) {
			held := s.loadConfigStamp()
			s.logger.Debug("CONFIG_SYNC: ignoring config already superseded or held",
				"sender", senderID, "version", incomingStamp.version,
				"origin", incomingStamp.origin,
				"held", held.version, "heldOrigin", held.origin)
			return &rpc.ConfigSyncResponse{
				Success: true,
				Message: configNotAppliedMessage(incomingStamp, held),
			}, nil
		}

		// One snapshot of the live pointer for every preserve-read below, so a
		// concurrent Reconfigure swapping it cannot be observed mid-function.
		cur := s.currentConfig()

		// Create a new config instance to hold incoming cluster-wide configuration
		newConfig := &config.Config{}

		// Unmarshal the received configuration
		if err := json.Unmarshal(req.Config, newConfig); err != nil {
			s.logger.Error("Failed to unmarshal configuration", "error", err)
			return &rpc.ConfigSyncResponse{
				Success: false,
				Message: permanentRejectionPrefix +
					fmt.Sprintf("failed to unmarshal configuration: %v", err),
			}, nil
		}

		// Preserve the local node identity from our existing configuration to avoid adopting remote LocalNode
		prevLocalID := cur.Pulse.LocalNode
		if prevLocalID != "" && newConfig.Pulse.LocalNode != prevLocalID {
			s.logger.Debugf("ConfigSync: preserving local node identity: %s (incoming had %s)", prevLocalID, newConfig.Pulse.LocalNode)
			newConfig.Pulse.LocalNode = prevLocalID
			// Ensure the node entry exists in incoming config
			if newConfig.Nodes == nil {
				newConfig.Nodes = map[string]*config.Node{}
			}
			if _, ok := newConfig.Nodes[prevLocalID]; !ok {
				if existing := cur.Nodes[prevLocalID]; existing != nil {
					// Shallow copy to avoid aliasing
					copied := *existing
					newConfig.Nodes[prevLocalID] = &copied
				}
			}
		}

		// Preserve local-specific settings before applying cluster config
		// These should not be overwritten by a remote ConfigSync
		localIDPreserve := cur.Pulse.LocalNode
		loggingLevelPreserve := cur.Pulse.LoggingLevel
		logToFilePreserve := cur.Pulse.LogToFile
		logFileLocationPreserve := cur.Pulse.LogFileLocation
		logToSyslogPreserve := cur.Pulse.LogToSyslog
		syslogNetworkPreserve := cur.Pulse.SyslogNetwork
		syslogAddressPreserve := cur.Pulse.SyslogAddress
		syslogFacilityPreserve := cur.Pulse.SyslogFacility
		syslogTagPreserve := cur.Pulse.SyslogTag
		// Whether this sync is the one that flips us into active-active decides
		// whether we have to seed the assignment map below.
		prevMode := cur.Pulse.Mode

		s.Lock()

		// Re-check now the write lock is held. The guard above ran before it, and a
		// local mutation takes s.Lock(), bumps the version and edits the config in
		// that window — unmarshalling a 200-address config is long enough for one of
		// the back-to-back add-ip calls that produce defect #38 to land inside it.
		// Applying anyway would erase an add that had already reported success and
		// leave the version claiming otherwise, which is the very shape being fixed.
		//
		// A requested repair is exempt here for the same reason as at the guard
		// above -- its whole purpose is to apply at a generation this node already
		// holds. The window this re-check protects is a local mutation landing
		// mid-apply, and that case is still handled: a local mutation bumps the
		// version, so the repair is then genuinely older and the next envelope's
		// fingerprint comparison decides whether another is needed.
		if !requestedRepair && !configIsNewer(incomingStamp, s.loadConfigStamp()) {
			held := s.loadConfigStamp()
			s.Unlock()
			s.logger.Debug("CONFIG_SYNC: this config was superseded or already held mid-apply",
				"sender", senderID, "version", incomingStamp.version,
				"origin", incomingStamp.origin,
				"held", held.version, "heldOrigin", held.origin)
			return &rpc.ConfigSyncResponse{
				Success: true,
				Message: configNotAppliedMessage(incomingStamp, held),
			}, nil
		}

		// Apply preserved local-specific settings onto the incoming config
		newConfig.Pulse.LocalNode = localIDPreserve
		newConfig.Pulse.LoggingLevel = loggingLevelPreserve
		newConfig.Pulse.LogToFile = logToFilePreserve
		newConfig.Pulse.LogFileLocation = logFileLocationPreserve
		newConfig.Pulse.LogToSyslog = logToSyslogPreserve
		newConfig.Pulse.SyslogNetwork = syslogNetworkPreserve
		newConfig.Pulse.SyslogAddress = syslogAddressPreserve
		newConfig.Pulse.SyslogFacility = syslogFacilityPreserve
		newConfig.Pulse.SyslogTag = syslogTagPreserve

		// Groups: a payload that carries the field is authoritative about it,
		// including about what is no longer in it (docs/TEST-PLAN.md defect #43).
		//
		// This used to merge — "if the incoming list is missing or empty, prefer
		// mine" — which is unanswerable for a removal, because absence and
		// emptiness are exactly what a removal looks like on the wire. All three
		// removing mutations were therefore undone by every receiver:
		// commitGroupDeletion deletes the key, UnassignGroupFromNode deletes an
		// interface entry that has no groups left, and RemoveIPFromGroup leaves the
		// group present with an empty list. Worse, the receiver still answered
		// Success, so the sender's broadcaster recorded full propagation and
		// cleared #43's retry state — the repair could never fire, because nothing
		// reported a failure. On whitecrane a `group delete --force` on the
		// coordinator propagated write 1 (the release) to all four and write 2 (the
		// delete) to none, leaving three nodes listing a group the coordinator had
		// dropped, which is the state that puts a group's addresses outside every
		// computable set (defect #59).
		//
		// Keyed on nil, not on emptiness. nil means the field was absent from the
		// JSON or explicitly null, which is a sender that has no opinion about
		// groups — the case the merge was written for, and the only one that still
		// preserves local. `floating_ip_groups`/`group_assignments` carry no
		// omitempty and config.New/Load always initialise the maps, so any live
		// daemon sending a full config emits at least `{}`: "I have no groups" is
		// distinguishable from "I do not speak groups".
		//
		// Safe because this point is past two stamp checks, the second under
		// s.Lock(): the payload is strictly newer than what this node holds, and
		// the wholesale semantics are what a newer config already had for a group's
		// address list. Absence is now ordered on the same clock as content.
		if newConfig.Groups == nil && len(s.config.Groups) > 0 {
			// Deep copy local groups
			newConfig.Groups = make(map[string][]string, len(s.config.Groups))
			for g, ips := range s.config.Groups {
				copySlice := make([]string, len(ips))
				copy(copySlice, ips)
				newConfig.Groups[g] = copySlice
			}
		}
		// Preserve per-node interface group assignments when the incoming node
		// entry carries none at all — same nil-versus-empty rule as the groups
		// above, for the same reason.
		localNodeID, _ := s.config.GetLocalNodeUUID()
		for nodeID, existing := range s.config.Nodes {
			if existing == nil {
				continue
			}
			nIncoming, ok := newConfig.Nodes[nodeID]
			if !ok || nIncoming == nil {
				// Keep existing node entirely if absent
				copied := *existing
				if copied.IPGroups != nil {
					copiedGroups := make(map[string][]string, len(copied.IPGroups))
					for iface, groups := range copied.IPGroups {
						gg := make([]string, len(groups))
						copy(gg, groups)
						copiedGroups[iface] = gg
					}
					copied.IPGroups = copiedGroups
				}
				newConfig.Nodes[nodeID] = &copied
				continue
			}
			if nIncoming.IPGroups == nil && len(existing.IPGroups) > 0 {
				nIncoming.IPGroups = make(map[string][]string, len(existing.IPGroups))
				for iface, groups := range existing.IPGroups {
					gg := make([]string, len(groups))
					copy(gg, groups)
					nIncoming.IPGroups[iface] = gg
				}
			}
			// Always use the local daemon's maintenance state — peers cannot override it in either direction
			if nodeID == localNodeID {
				nIncoming.Maintenance = existing.Maintenance
			}
		}

		// Persist and update our configuration
		s.logger.Debug("CONFIG_SYNC: Saving synchronized configuration")
		if err := newConfig.Save(); err != nil {
			s.logger.Error("CONFIG_SYNC: Failed to save synchronized configuration", "error", err)
			s.Unlock()
			// Deliberately *not* marked permanent: the payload was understood and only
			// storing it failed, which is ENOSPC, EIO or a read-only mount and clears
			// on its own. The sender has to keep this peer in its retry set, or the
			// broadcast reports full propagation to a node holding none of the config.
			return &rpc.ConfigSyncResponse{
				Success: false,
				Message: fmt.Sprintf("failed to save synchronized configuration: %v", err),
			}, nil
		}
		s.logger.Debug("CONFIG_SYNC: Configuration saved successfully")

		// Log what nodes are in the new config
		s.logger.Debug("CONFIG_SYNC: New config contains nodes", "nodeCount", len(newConfig.Nodes))
		for id, node := range newConfig.Nodes {
			s.logger.Debug("CONFIG_SYNC: Node in new config",
				"nodeID", id,
				"hostname", node.Hostname,
				"ip", node.IP,
				"port", node.Port)
		}

		s.config = newConfig

		// Adopted in the same critical section as the pointer swap, and only once
		// the config is actually in place: a save failure above cannot make this
		// node claim a version it never applied, and a local mutation cannot land
		// between the two. Adopting after the unlock left exactly that gap — the
		// mutation minted its version against the stamp this sync was about to
		// overwrite, so the next reconcile silently reverted it.
		s.adoptConfigStamp(incomingStamp)

		// Update convergence metadata if newer. Also under the lock: the config
		// broadcaster reads s.clusterEpoch under RLock, so writing it after the
		// unlock is a data race the detector flags.
		//
		// adoptConvergenceMetadataLocked, not adoptConvergenceMetadata: the
		// latter takes the lock this branch already holds. Until that split
		// existed this was a hand-copied compare-and-write, which is how one
		// rule came to have two implementations — see the Locked variant.
		//
		// atLeast is false, which is what the copy did: it adopted on
		// `incomingEpoch > s.clusterEpoch` and nothing else.
		oldEpoch := s.clusterEpoch
		if s.adoptConvergenceMetadataLocked(incomingEpoch, incomingLeaderID, false) {
			s.logger.Debug("CONFIG_SYNC: Updating cluster epoch",
				"oldEpoch", oldEpoch,
				"newEpoch", incomingEpoch,
				"leaderID", incomingLeaderID)
		}

		s.Unlock()

		// One snapshot for everything below the unlock. These were bare s.config
		// reads, and the async reconfigure this function spawns swaps that very
		// pointer, so every one of them raced it.
		installed := s.currentConfig()

		// Immediately refresh member list from new configuration so peers become visible
		s.logger.Debug("CONFIG_SYNC: Updating member list with new config")
		s.memberList.UpdateConfig(installed)

		s.logger.Debug("CONFIG_SYNC: Loading initial members")
		if err := s.loadInitialMembers(installed); err != nil {
			s.logger.Error("CONFIG_SYNC: Failed to load members after sync", "error", err)
		}

		// Seed the assignment map on the node that learns the mode changed, not
		// only on the node that handled the request. Whoever ends up
		// active-active coordinator makes the redistribute-or-rebalance decision
		// from its own member list, and on whitecrane that was a different node:
		// it saw no assignments anywhere, called all 201 addresses orphaned and
		// placed them on top of the ones the previous Active still held
		// (docs/TEST-PLAN.md defects #2/#26). Every node derives the same answer
		// from the config it just applied, so seeding here is consistent rather
		// than a second opinion.
		//
		// Under s.RLock(): this runs after the pointer swap released the write
		// lock, and seedActiveActiveAssignments reads the config maps through
		// groupIPsForNode. The read lock is enough — it writes only member state —
		// and it must be the read lock, because the write lock is what the swap
		// above already released.
		s.RLock()
		switchedToActiveActive := prevMode != "active-active" && s.config.Pulse.Mode == "active-active"
		if switchedToActiveActive {
			s.logger.Info("CONFIG_SYNC: cluster switched to active-active, seeding assignments")
		}
		seeded := switchedToActiveActive && s.seedActiveActiveAssignments()
		s.RUnlock()

		if switchedToActiveActive && !seeded {
			s.logger.Warn("CONFIG_SYNC: no active node found while seeding active-active assignments")
		}
	} else {
		// Envelope-only update: do NOT overwrite config; just apply incoming states
		// and metadata.
		//
		// Adopted under the lock, like the full-config branch above. This branch
		// never takes s.Lock() at all, so the compare-and-write here was the same
		// unsynchronised access on a different path: the config broadcaster reads
		// both fields under RLock, so -race flags it.
		s.adoptConvergenceMetadata(incomingEpoch, incomingLeaderID, false)
		// Keep member list as-is for envelope updates to avoid clobbering runtime states
		// s.memberList.UpdateConfig(s.config)
		// Skip loadInitialMembers here; members are stable
	}

	// Apply incoming member states if provided
	if len(incomingMemberStates) > 0 {
		// Decide whether to apply incoming states based on epoch and leader to avoid cross-over
		applyStates := false
		// Both branches above have released the lock by now, so read the pair
		// under it — and as a pair, so the leader belongs to the epoch.
		currentEpoch, currentLeader := s.convergenceMetadata()
		s.logger.Debug("CONFIG_SYNC: Evaluating incoming member states", "incoming_epoch", incomingEpoch, "current_epoch", currentEpoch,
			"incoming_leader", incomingLeaderID, "current_leader", currentLeader, "states", incomingMemberStates)

		if incomingEpoch > currentEpoch {
			applyStates = true
			s.logger.Debug("CONFIG_SYNC: Will apply states (incoming epoch is higher)")
		} else if incomingEpoch == currentEpoch {
			// Only accept if leader matches (or no leader set yet)
			if currentLeader == "" || incomingLeaderID == currentLeader {
				applyStates = true
				s.logger.Debug("CONFIG_SYNC: Will apply states (same epoch, matching leader)")
			} else {
				s.logger.Debug("CONFIG_SYNC: Rejecting states (same epoch, different leader)")
			}
		} else {
			s.logger.Debug("CONFIG_SYNC: Rejecting states (incoming epoch is lower)")
		}

		if applyStates {
			// Snapshotted for the same reason: every config read below raced the
			// async reconfigure's pointer swap.
			statesCfg := s.currentConfig()
			// Update epoch/leader if needed. An equal epoch is adopted here as well
			// as a higher one, which is why this takes the atLeast comparison.
			if s.adoptConvergenceMetadata(incomingEpoch, incomingLeaderID, true) {
				s.logger.Debug("CONFIG_SYNC: Updated cluster epoch and leader", "epoch", incomingEpoch, "leader", incomingLeaderID)
			}

			// Capture LOCAL node's status before applying incoming states
			localID, err := statesCfg.GetLocalNodeUUID()
			var oldLocalStatus membership.MemberStatus
			var hadLocalMember bool
			if err == nil {
				if localMember := s.memberList.GetMemberByID(localID); localMember != nil {
					oldLocalStatus = localMember.GetStatus()
					hadLocalMember = true
				}
			}

			s.logger.Debug("CONFIG_SYNC: Applying member states to local member list", "count", len(incomingMemberStates))
			syncLocalID, _ := statesCfg.GetLocalNodeUUID()

			// Whether this sync carries a decision or merely re-asserts an agreed view.
			// Only a strictly higher epoch is a decision; an equal epoch is a peer's
			// heartbeat, which has no authority over what this node knows about itself.
			// Compared against the epoch held on entry, not currentEpoch: by this
			// point a higher incoming epoch has already been adopted above.
			decisive := incomingEpoch > preSyncEpoch

			for id, st := range incomingMemberStates {
				m := s.memberList.GetMemberByID(id)
				if m == nil {
					s.logger.Warn("CONFIG_SYNC: Member not found in member list", "node_id", id)
					continue
				}

				// Read before the decision runs rather than inside it, so the
				// locked region reaches for as little as possible. Unsynchronised
				// either way -- config contents are guarded by nothing, which is
				// its own ticket -- so hoisting it changes no behaviour.
				nodeCfg := statesCfg.Nodes[id]

				// UpdateClaim rather than the member lock taken by hand. The
				// decision has to see the current status to judge the incoming
				// one, and commit against the status it judged: reading and
				// writing in two acquisitions would let a promotion land in
				// between and be silently overwritten by a peer's older view.
				//
				// The guard clauses return false, which leaves the member
				// untouched -- what "ignored" has to mean here.
				//
				// The Debug lines stay inside the decision. They take no member
				// lock, the locked region is the same as it was, and this repo's
				// live verification is grep-based (docs/TEST-PLAN.md passim), so
				// moving the log surface would cost more than the shorter
				// critical section is worth.
				m.UpdateClaim(func(current membership.Claim) (membership.Claim, bool) {
					// Peers must not override the local node's maintenance state;
					// only the local daemon controls its own maintenance flag.
					if id == syncLocalID {
						// The same principle applied to status generally. This node knows
						// its own status better than a peer whose view may predate the
						// change — most importantly when a coordinator has just assigned
						// it IPs in active-active and it has gone Active in response. An
						// equal-epoch peer that still remembers it as Passive would
						// otherwise demote it and have its new IPs stripped, only for the
						// coordinator to assign them again (docs/TEST-PLAN.md defect #2).
						// A real demotion — election, mode switch, explicit promote —
						// always arrives at a higher epoch and still applies.
						if !decisive && st != current.Status {
							s.logger.Debug("CONFIG_SYNC: Ignoring peer's equal-epoch view of local status",
								"node_id", id, "local", membership.StatusToString(current.Status),
								"peer_claims", membership.StatusToString(st), "epoch", incomingEpoch)
							return current, false
						}
						if nodeCfg != nil {
							if nodeCfg.Maintenance {
								// Config says we're in maintenance — enforce it.
								if current.Status != membership.StatusMaintenance {
									current.Status = membership.StatusMaintenance
									s.logger.Debug("CONFIG_SYNC: Restored local maintenance status overridden by peer", "node_id", id)
									// The status only: enforcing maintenance says
									// nothing about which addresses the record
									// still lists, and clearing them here was not
									// what the hand-written version did.
									return current, true
								}
								return current, false
							}
							// Config says we're NOT in maintenance — reject stale StatusMaintenance from peers.
							if st == membership.StatusMaintenance {
								s.logger.Debug("CONFIG_SYNC: Rejected stale maintenance status from peer; local config shows not in maintenance", "node_id", id)
								return current, false
							}
						}
					}
					oldStatus := current.Status
					current.Status = st
					// A Passive or Maintenance node cannot hold floating IPs, so
					// drop any assignment still recorded against it. Without
					// this, status keeps listing IPs the node already released
					// (after a demote, or a switch to active-passive) because
					// the node's own clear is never reported to its peers.
					// StatusUnknown is deliberately left alone: failover reads a
					// failed active's last-known IPs to hand to its replacement.
					if st == membership.StatusPassive || st == membership.StatusMaintenance {
						current.ActiveIPs = nil
					}
					s.logger.Debug("CONFIG_SYNC: Updated member status", "node_id", id, "old_status", membership.StatusToString(oldStatus), "new_status", membership.StatusToString(st))
					return current, true
				})
			}

			// Check if LOCAL node transitioned to Active - if so, bring up VIPs
			// This handles the case where a node learns it's Active from ConfigSync (e.g., after restart during election)
			// Only triggers on state CHANGE, not continuous heartbeats, so no GARP storm risk
			if hadLocalMember && oldLocalStatus != membership.StatusActive {
				if newLocalMember := s.memberList.GetMemberByID(localID); newLocalMember != nil && newLocalMember.GetStatus() == membership.StatusActive {
					s.logger.Debug("ConfigSync: LOCAL node transitioned to Active, triggering VIP setup", "oldStatus", membership.StatusToString(oldLocalStatus), "newStatus", "Active")
					go s.refreshLocalMonitorExpectedIPs()
				}
			}

			// The mirror case: a node that learns it has been demoted must
			// release the floating IPs it still holds. Without this the release
			// waits on the monitor's periodic reconcile, and until then this
			// node and the surviving Active ARP-fight over every floating IP.
			// See isDemotion for why a transition to Unknown is not one.
			if hadLocalMember {
				// One locked read: the unlocked field read raced every writer of
				// it, and reading twice could test one status and log another.
				newLocalStatus := membership.StatusUnknown
				newLocalMember := s.memberList.GetMemberByID(localID)
				if newLocalMember != nil {
					newLocalStatus = newLocalMember.GetStatus()
				}
				if newLocalMember != nil && isDemotion(oldLocalStatus, newLocalStatus) {
					s.logger.Info("ConfigSync: LOCAL node demoted from Active, releasing floating IPs",
						"newStatus", membership.StatusToString(newLocalStatus))
					go s.refreshLocalMonitorExpectedIPs()
				}
			}

			// Do not coerce non-leader nodes to Passive here; let health checks set Unknown/Passive
		} else {
			s.logger.Debug("ConfigSync: ignoring incoming member_states due to stale epoch or lower-priority leader",
				"incoming_epoch", incomingEpoch, "current_epoch", currentEpoch,
				"incoming_leader", incomingLeaderID, "current_leader", currentLeader)
		}
	}

	// Apply the sender's self-reported hosted IPs (active-active only). A
	// node's report about itself is authoritative, so no epoch gating; this
	// keeps every peer's view of the IP assignment map current enough to
	// redistribute correctly if the sender later fails.
	senderCfg := s.currentConfig()
	if senderCfg.Pulse.Mode == "active-active" && senderID != "" && senderActiveIPs != nil {
		if localID, err := senderCfg.GetLocalNodeUUID(); err == nil && senderID != localID {
			if senderMember := s.memberList.GetMemberByID(senderID); senderMember != nil {
				// SetActiveIPs copies, so the explicit append is no longer
				// needed to detach the payload's slice.
				senderMember.SetActiveIPs(senderActiveIPs)
			}
		}
	}

	// Only reconfigure when full config changed; skip for envelope-only state updates
	if isFullConfig {
		// Counted before the spawn, not inside it: a Wait racing the goroutine's
		// own Add would return before the reconfigure had started.
		s.asyncReconfigures.Add(1)
		go func() {
			defer s.asyncReconfigures.Done()
			if err := s.Reconfigure(); err != nil {
				s.logger.Error("Async reconfigure failed after ConfigSync", "error", err)
			} else {
				s.logger.Debug("Async reconfigure completed after ConfigSync")
			}
			if s.onAsyncReconfigure != nil {
				s.onAsyncReconfigure()
			}
		}()
	}

	// REMOVED: Redundant refresh call - health checker already handles VIP reconciliation
	// The continuous ConfigSync operations were creating a feedback loop with excessive refresh calls
	// go s.refreshLocalMonitorExpectedIPs()

	return &rpc.ConfigSyncResponse{
		Success: true,
		Message: "configuration synchronized successfully",
	}, nil
}

// AddNode adds a new node to the cluster
func (s *Server) AddNode(nodeID string) error {
	s.logger.Debugf("Adding node %s to cluster", nodeID)

	// Get node config
	node, ok := s.config.Nodes[nodeID]
	if !ok {
		s.logger.Errorf("FATAL: No configuration found for node %s", nodeID)
		return fmt.Errorf("no configuration found for node %s", nodeID)
	}

	if err := s.memberList.AddMember(nodeID, node.Hostname, node.IP, node.Port); err != nil {
		s.logger.Errorf("FATAL: Failed to add member %s: %v", nodeID, err)
		return fmt.Errorf("failed to add member %s: %v", nodeID, err)
	}

	return nil
}

// configKeyScope says where a `pulsectl config set` key takes effect.
type configKeyScope int

const (
	// scopeCluster keys describe how the cluster behaves as a whole, so every
	// node has to hold the same value. A cluster running two different values
	// for one of these is exactly the divergence quorum exists to prevent.
	scopeCluster configKeyScope = iota
	// scopeNode keys describe how this one daemon logs. ConfigSync deliberately
	// preserves them when it applies an incoming full config, so that a peer
	// left at debug for an investigation is not reset by the next broadcast —
	// which also means a broadcast cannot carry them, and they are node-local by
	// design rather than by omission.
	scopeNode
)

// settableConfigKeys is every key `config set` accepts and the scope it takes
// effect at.
//
// Keys absent from the table are refused rather than written. The tag-based
// setter underneath will happily overwrite `local_node` or `cluster_token`, and
// neither is a value an operator sets by hand: the first is this node's identity
// in the member list, the second the shared cluster secret.
var settableConfigKeys = map[string]configKeyScope{
	"mode":              scopeCluster,
	"hcs_interval":      scopeCluster,
	"fos_interval":      scopeCluster,
	"fo_limit":          scopeCluster,
	"auto_failback":     scopeCluster,
	"logging_level":     scopeNode,
	"log_to_file":       scopeNode,
	"log_file_location": scopeNode,
	"log_to_syslog":     scopeNode,
	"syslog_network":    scopeNode,
	"syslog_address":    scopeNode,
	"syslog_facility":   scopeNode,
	"syslog_tag":        scopeNode,
}

// What UpdateConfig reports back about the reach of a change it applied. The CLI
// prints these verbatim, because "Successfully updated mode to active-passive"
// meant one node on whitecrane and the whole cluster in the help text.
const (
	configScopeClusterMessage = "applied to every node in the cluster"
	configScopeNodeMessage    = "applied to this node only — this key is node-local and has to be set on each node"
)

// UpdateConfig implements CLI.UpdateConfig
func (s *Server) UpdateConfig(ctx context.Context, req *rpc.UpdateConfigRequest) (*rpc.UpdateConfigResponse, error) {
	if req == nil || req.Key == "" {
		return &rpc.UpdateConfigResponse{Success: false, Message: "invalid request"}, nil
	}

	scope, settable := settableConfigKeys[req.Key]
	if !settable {
		return &rpc.UpdateConfigResponse{
			Success: false,
			Message: fmt.Sprintf("%q is not a settable configuration key", req.Key),
		}, nil
	}

	// The mode is not a value to write and push: changing it consolidates or
	// spreads the floating IPs and re-broadcasts the member statuses that go with
	// the new mode, and SetMode owns all of that. Writing `mode` into the config
	// here instead left the node that ran the command in active-passive while its
	// peers stayed in active-active — it logged "4 nodes are Active in
	// active-passive mode; waiting for the coordinator to consolidate" 529 times
	// against a coordinator that was not in active-passive and never would be
	// (docs/TEST-PLAN.md defect #42).
	//
	// Delegated before the lock below is taken: SetMode takes s.Lock() itself and
	// the lock is not reentrant.
	if req.Key == "mode" {
		resp, err := s.SetMode(ctx, &rpc.SetModeRequest{Mode: req.Value})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return &rpc.UpdateConfigResponse{Success: false, Message: "mode change returned no result"}, nil
		}
		return &rpc.UpdateConfigResponse{Success: resp.Success, Message: resp.Message}, nil
	}

	s.Lock()
	defer s.Unlock()

	if err := s.config.UpdateValue(req.Key, req.Value); err != nil {
		s.logger.Errorf("Failed to update config %s: %v", req.Key, err)
		return &rpc.UpdateConfigResponse{Success: false, Message: err.Error()}, nil
	}

	// Apply runtime changes for known keys
	if req.Key == "logging_level" {
		if level, err := log.ParseLevel(req.Value); err == nil {
			s.logger.SetLevel(level)
		}
	}

	if scope == scopeNode {
		return &rpc.UpdateConfigResponse{Success: true, Message: configScopeNodeMessage}, nil
	}

	// Stamp and broadcast, the same way a group mutation does. Without this the
	// value only ever reached the node the CLI happened to run on, while the
	// operator was told it had been applied to the cluster — and the broadcaster
	// is also what retries a push a peer could not take (defect #43), so a peer
	// that is briefly unreachable still converges.
	s.markConfigDirty()

	return &rpc.UpdateConfigResponse{Success: true, Message: configScopeClusterMessage}, nil
}

// ReadConfig implements CLI.ReadConfig — returns the daemon's live config as JSON.
func (s *Server) ReadConfig(ctx context.Context, req *rpc.ReadConfigRequest) (*rpc.ReadConfigResponse, error) {
	s.RLock()
	defer s.RUnlock()

	data, err := json.Marshal(s.config)
	if err != nil {
		return &rpc.ReadConfigResponse{Success: false, Message: err.Error()}, nil
	}
	return &rpc.ReadConfigResponse{Success: true, Config: data}, nil
}

// SetMaintenance implements CLI.SetMaintenance and Server.SetMaintenance.
//
// When called via the CLI service (pulsectl), it may target any node:
//   - Local node: handled directly.
//   - Remote node: forwarded to the target via the inter-node Server.SetMaintenance RPC so
//     the target manages its own state and persists the flag in its own config.
//
// When called via the Server service (inter-node forward), req.NodeId is always the local
// node so it is handled as a local operation.
func (s *Server) SetMaintenance(ctx context.Context, req *rpc.SetMaintenanceRequest) (*rpc.SetMaintenanceResponse, error) {
	localID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: "failed to resolve local node: " + err.Error()}, nil
	}

	// Resolve target; empty means local
	targetID := req.NodeId
	if targetID == "" {
		targetID = localID
	}

	if targetID != localID {
		// Remote target: forward the request directly to that node so it manages its own state
		return s.setMaintenanceRemote(ctx, targetID, req.Enable)
	}

	// Local target
	return s.setMaintenanceLocal(ctx, localID, req.Enable)
}

// setMaintenanceRemote forwards a maintenance request to a remote node via the Server service.
func (s *Server) setMaintenanceRemote(ctx context.Context, targetID string, enable bool) (*rpc.SetMaintenanceResponse, error) {
	// Quorum guard: refuse if this would leave no available nodes
	if enable {
		availableAfter := 0
		for id, m := range s.memberList.MembersSnapshot() {
			if id == targetID {
				continue
			}
			st := m.GetStatus()
			if st != membership.StatusMaintenance && st != membership.StatusUnknown {
				availableAfter++
			}
		}
		if availableAfter == 0 {
			return &rpc.SetMaintenanceResponse{Success: false, Message: "cannot enter maintenance: at least one other node must remain available"}, nil
		}
	}

	node := s.config.Nodes[targetID]
	if node == nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: fmt.Sprintf("node %s not found in config", targetID)}, nil
	}

	remoteClient, err := client.New()
	if err != nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: "failed to create client: " + err.Error()}, nil
	}
	defer remoteClient.Close()

	if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: "failed to connect to target node: " + err.Error()}, nil
	}

	fwdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Forward with NodeId = targetID so the remote resolves it as its own local node
	resp, err := remoteClient.Server().SetMaintenance(fwdCtx, &rpc.SetMaintenanceRequest{
		Enable: enable,
		NodeId: targetID,
	})
	if err != nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: "remote SetMaintenance failed: " + err.Error()}, nil
	}

	// Reflect the state change in our local member list so status is correct here too
	if member := s.memberList.GetMemberByID(targetID); member != nil {
		if enable {
			// Maintenance with nothing claimed: the addresses were released
			// before the transition, so the record must stop asserting them.
			member.SetClaim(membership.Claim{Status: membership.StatusMaintenance})
		} else {
			// SetStatus, not SetClaim, and the asymmetry is deliberate: leaving
			// maintenance restores eligibility and nothing else. A node in
			// maintenance holds no addresses, so there is no assignment set to
			// write, and clearing one here would discard anything a coordinator
			// had already given it on the way back in.
			member.SetStatus(membership.StatusPassive)
		}
	}

	states := getStatusMap()
	for id, m := range s.memberList.MembersSnapshot() {
		states[id] = m.GetStatus()
	}
	_ = s.broadcastNextEpoch(states)
	putStatusMap(states)

	return resp, nil
}

// setMaintenanceLocal enters or exits maintenance on the local node.
func (s *Server) setMaintenanceLocal(ctx context.Context, localID string, enable bool) (*rpc.SetMaintenanceResponse, error) {
	member := s.memberList.GetMemberByID(localID)
	if member == nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: "local node not found in member list"}, nil
	}

	if enable {
		currentStatus := member.GetStatus()

		if currentStatus == membership.StatusMaintenance {
			return &rpc.SetMaintenanceResponse{Success: true, Message: "node is already in maintenance mode"}, nil
		}

		// Refuse if this would leave no available nodes
		availableAfter := 0
		for id, m := range s.memberList.MembersSnapshot() {
			if id == localID {
				continue
			}
			st := m.GetStatus()
			if st != membership.StatusMaintenance && st != membership.StatusUnknown {
				availableAfter++
			}
		}
		if availableAfter == 0 {
			return &rpc.SetMaintenanceResponse{Success: false, Message: "cannot enter maintenance: at least one other node must remain available"}, nil
		}

		// Demote first if active so the cluster elects a new active node.
		// Abort maintenance if demotion fails — otherwise the node would be marked
		// maintenance while still holding active IPs, leaving the cluster in a split state.
		if currentStatus == membership.StatusActive {
			s.logger.Info("Maintenance: local node is active — triggering failover before entering maintenance")
			resp, err := s.MakePassive(ctx, &rpc.MakePassiveRequest{NodeId: localID})
			if err != nil {
				s.logger.Error("Maintenance: MakePassive RPC error; aborting maintenance entry", "error", err)
				return &rpc.SetMaintenanceResponse{Success: false, Message: "failed to demote node before maintenance: " + err.Error()}, nil
			}
			if resp == nil || !resp.Success {
				msg := "demotion reported failure"
				if resp != nil && resp.Message != "" {
					msg = resp.Message
				}
				s.logger.Error("Maintenance: MakePassive returned failure; aborting maintenance entry", "message", msg)
				return &rpc.SetMaintenanceResponse{Success: false, Message: "failed to demote node before maintenance: " + msg}, nil
			}
		}

		if err := member.EnterMaintenance(); err != nil {
			return &rpc.SetMaintenanceResponse{Success: false, Message: err.Error()}, nil
		}

		s.Lock()
		if node := s.config.Nodes[localID]; node != nil {
			node.Maintenance = true
		}
		s.Unlock()
		if err := s.config.Save(); err != nil {
			s.logger.Warn("Maintenance: failed to persist maintenance flag", "error", err)
		}

		states := getStatusMap()
		for id, m := range s.memberList.MembersSnapshot() {
			states[id] = m.GetStatus()
		}
		_ = s.broadcastNextEpoch(states)
		putStatusMap(states)

		s.logger.Info("Node entered maintenance mode")
		return &rpc.SetMaintenanceResponse{Success: true, Message: fmt.Sprintf("node %s is now in maintenance mode", localID)}, nil
	}

	// Exit maintenance
	if err := member.ExitMaintenance(); err != nil {
		return &rpc.SetMaintenanceResponse{Success: false, Message: err.Error()}, nil
	}

	s.Lock()
	if node := s.config.Nodes[localID]; node != nil {
		node.Maintenance = false
	}
	s.Unlock()
	if err := s.config.Save(); err != nil {
		s.logger.Warn("Maintenance: failed to clear maintenance flag", "error", err)
	}

	states := getStatusMap()
	for id, m := range s.memberList.MembersSnapshot() {
		states[id] = m.GetStatus()
	}
	_ = s.broadcastNextEpoch(states)
	putStatusMap(states)

	s.logger.Info("Node exited maintenance mode")
	return &rpc.SetMaintenanceResponse{Success: true, Message: fmt.Sprintf("node %s returned to passive — eligible for promotion", localID)}, nil
}

// SetCapacity implements CLI.SetCapacity. Capacity is a config-only setting:
// it is persisted and broadcast here, and every node (including this one)
// applies it to its member list when the synced config lands. Existing IP
// assignments above the new capacity are not evicted; the active-active
// reconcile loop simply stops placing new IPs on the node.
func (s *Server) SetCapacity(ctx context.Context, req *rpc.SetCapacityRequest) (*rpc.SetCapacityResponse, error) {
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.SetCapacityResponse{Success: false, Message: "no cluster configured"}, nil
	}

	if req.Capacity < 0 {
		return &rpc.SetCapacityResponse{Success: false, Message: "capacity must be >= 0 (0 = unlimited)"}, nil
	}

	// Resolve target; empty means local, otherwise accept UUID or hostname
	targetID := req.NodeId
	if targetID == "" {
		localID, err := s.config.GetLocalNodeUUID()
		if err != nil {
			return &rpc.SetCapacityResponse{Success: false, Message: "failed to resolve local node: " + err.Error()}, nil
		}
		targetID = localID
	}
	if _, ok := s.config.Nodes[targetID]; !ok {
		if member := s.memberList.GetMemberByIdentifier(targetID); member != nil {
			targetID = member.ID
		}
	}

	node, ok := s.config.Nodes[targetID]
	if !ok {
		return &rpc.SetCapacityResponse{Success: false, Message: fmt.Sprintf("node %s not found in config", req.NodeId)}, nil
	}

	node.Capacity = int(req.Capacity)

	if err := s.config.Save(); err != nil {
		return &rpc.SetCapacityResponse{Success: false, Message: fmt.Sprintf("failed to save config: %v", err)}, nil
	}

	// Apply to the in-memory member immediately so local distribution
	// decisions don't wait for the next config sync
	if member := s.memberList.GetMemberByID(targetID); member != nil {
		member.SetCapacity(int(req.Capacity))
	}

	// Broadcast updated config to peers
	s.markConfigDirty()

	limit := "unlimited"
	if req.Capacity > 0 {
		limit = fmt.Sprintf("%d floating IP(s)", req.Capacity)
	}
	s.logger.Info("Node capacity updated", "node", node.Hostname, "capacity", req.Capacity)
	return &rpc.SetCapacityResponse{Success: true, Message: fmt.Sprintf("capacity for node %s set to %s", node.Hostname, limit)}, nil
}

// ResyncNetwork implements CLI.ResyncNetwork RPC
func (s *Server) ResyncNetwork(ctx context.Context, req *rpc.ResyncNetworkRequest) (*rpc.ResyncNetworkResponse, error) {
	// Avoid holding the server lock while calling Reconfigure to prevent deadlocks,
	// since Reconfigure acquires the same lock internally.

	// Optionally create default groups for new interfaces
	if req.GetCreateDefaultGroups() {
		interfaces, err := net.Interfaces()
		if err != nil {
			return &rpc.ResyncNetworkResponse{Success: false, Message: err.Error()}, nil
		}
		for _, iface := range interfaces {
			if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
				continue
			}
			groupName := fmt.Sprintf("default-%s", iface.Name)
			if _, exists := s.config.Groups[groupName]; !exists {
				s.config.Groups[groupName] = []string{}
				// assign group entry on local node so UI can see mapping
				localID, err := s.config.GetLocalNodeUUID()
				if err == nil {
					node := s.config.Nodes[localID]
					if node != nil {
						if node.IPGroups == nil {
							node.IPGroups = make(map[string][]string)
						}
						node.IPGroups[iface.Name] = append(node.IPGroups[iface.Name], groupName)
					}
				}
			}
		}
		_ = s.config.Save()

		// Refresh monitor expected IPs for local node after default group creation
		if s.ipMonitor != nil {
			if localID, err := s.config.GetLocalNodeUUID(); err == nil {
				node := s.config.Nodes[localID]
				if node != nil {
					for iface := range node.IPGroups {
						// Recompute expected IPs (likely empty at creation time)
						ifaceIPs := s.expectedIfaceIPs(localID, iface)
						s.ipMonitor.ClearExpectedIPs(iface)
						if len(ifaceIPs) > 0 {
							s.ipMonitor.UpdateExpectedIPs(iface, ifaceIPs)
						}
					}
				}
			}
		}
	}

	// Force immediate activation if cluster configuration exists
	// Create a fresh config instance to ensure we read the current on-disk config

	// Preserve local node identity before reloading config (critical for correct member identification)
	prevLocalNodeID := ""
	prevNodeCount := 0
	if s.config != nil {
		prevLocalNodeID, _ = s.config.GetLocalNodeUUID()
		prevNodeCount = len(s.config.Nodes)
	}

	s.logger.Info("RESYNC: Reloading config from disk",
		"prev_local_node_id", prevLocalNodeID,
		"prev_node_count", prevNodeCount)

	cfg, err := config.New()
	if err != nil {
		s.logger.Errorf("RESYNC: Failed to reload config during resync: %v", err)
		return &rpc.ResyncNetworkResponse{Success: false, Message: fmt.Sprintf("failed to reload config: %v", err)}, nil
	}

	diskLocalNodeID, _ := cfg.GetLocalNodeUUID()
	diskNodeCount := len(cfg.Nodes)

	s.logger.Info("RESYNC: Loaded config from disk",
		"disk_local_node_id", diskLocalNodeID,
		"disk_node_count", diskNodeCount)

	// Preserve local node identity if it was set and differs from disk
	// This prevents config corruption during resync (similar to ConfigSync logic)
	if prevLocalNodeID != "" && diskLocalNodeID != prevLocalNodeID {
		s.logger.Warn("RESYNC: Local node identity mismatch between runtime and disk config",
			"runtime_local_id", prevLocalNodeID,
			"disk_local_id", diskLocalNodeID)

		// Verify the runtime local node ID exists in the disk config's nodes
		if _, exists := cfg.Nodes[prevLocalNodeID]; exists {
			s.logger.Info("RESYNC: Preserving runtime local node identity",
				"preserved_id", prevLocalNodeID,
				"disk_id", diskLocalNodeID)
			cfg.Pulse.LocalNode = prevLocalNodeID

			// Save the corrected identity back to disk to prevent future mismatches
			if err := cfg.Save(); err != nil {
				s.logger.Error("RESYNC: Failed to save corrected local node identity to disk", "error", err)
			} else {
				s.logger.Info("RESYNC: Saved corrected local node identity to disk")
			}
		} else {
			s.logger.Error("RESYNC: Runtime local node ID not found in disk config nodes, using disk config",
				"runtime_id", prevLocalNodeID,
				"disk_nodes", cfg.Nodes)
		}
	} else if prevLocalNodeID != "" && diskLocalNodeID == prevLocalNodeID {
		s.logger.Debug("RESYNC: Local node identity consistent between runtime and disk")
	}

	s.config = cfg

	newLocalNodeID, _ := s.config.GetLocalNodeUUID()
	s.logger.Info("RESYNC: Config update complete",
		"final_local_node_id", newLocalNodeID,
		"final_node_count", len(s.config.Nodes))

	// Always sync the member list's config pointer after replacing s.config.
	// If we skip this when ClusterCheck is false (e.g. RESYNC before cluster creation),
	// the member list holds a stale pointer and CreateCluster's LocalNode update becomes
	// invisible to the health checker, causing the node to be marked Unknown.
	s.memberList.UpdateConfig(s.config)

	if s.config.ClusterCheck() {
		// Reload members and reconfigure listener for the new config
		s.logger.Info("RESYNC: Updating member list with new config",
			"current_member_count", len(s.memberList.MembersSnapshot()))

		s.logger.Info("RESYNC: Loading initial members from new config",
			"config_node_count", len(s.config.Nodes))

		if err := s.loadInitialMembers(s.currentConfig()); err != nil {
			s.logger.Errorf("RESYNC: Failed to load members during resync: %v", err)
			return &rpc.ResyncNetworkResponse{Success: false, Message: fmt.Sprintf("failed to load members: %v", err)}, nil
		}

		s.logger.Info("RESYNC: Member loading complete",
			"final_member_count", len(s.memberList.MembersSnapshot()))

		// Ensure cluster listener is bound and health checker running
		if err := s.Reconfigure(); err != nil {
			s.logger.Errorf("Failed to reconfigure server during resync: %v", err)
			return &rpc.ResyncNetworkResponse{Success: false, Message: fmt.Sprintf("failed to reconfigure server: %v", err)}, nil
		}

		s.startHealthChecker()

		// Wait briefly for the cluster listener to become ready after resync
		if localNode, err := s.config.GetLocalNode(); err == nil {
			// JoinHostPort for the same reason as the dial probe above: equivalent
			// to FormatIPv6 + "%s:%s", but visible to `go vet`.
			address := net.JoinHostPort(utils.SanitizeIPv6(localNode.IP), localNode.Port)
			readyDeadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(readyDeadline) {
				conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
				if err == nil {
					_ = conn.Close()
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}

		// Membership reconciliation with quorum (clusters >=3)
		clusterSize := len(s.config.Nodes)
		if clusterSize >= 3 {
			// Build presence counts for each known node based on peer snapshots
			presenceCount := make(map[string]int)
			for id := range s.config.Nodes {
				presenceCount[id] = 0
			}

			// Query each peer for its status snapshot
			for id, node := range s.config.Nodes {
				// Skip local node
				localID, _ := s.config.GetLocalNodeUUID()
				if id == localID {
					continue
				}
				remoteClient, err := client.New()
				if err != nil {
					s.logger.Warn("Resync: failed to create client for peer", "peer", id, "error", err)
					continue
				}
				defer remoteClient.Close()
				if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
					s.logger.Warn("Resync: failed to connect to peer", "peer", id, "error", err)
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				resp, err := remoteClient.CLI().Status(ctx, &rpc.StatusRequest{})
				cancel()
				if err != nil || resp == nil {
					s.logger.Warn("Resync: failed to get status from peer", "peer", id, "error", err)
					continue
				}

				// Mark nodes present in this peer's view by hostname
				for knownID, knownNode := range s.config.Nodes {
					for _, m := range resp.Members {
						if m.Hostname == knownNode.Hostname {
							presenceCount[knownID]++
							break
						}
					}
				}
			}

			// Determine majority threshold
			majority := (clusterSize / 2) + 1

			// For any node missing from majority and currently Unknown locally, propose removal vote
			for id := range s.config.Nodes {
				// Skip local node
				localID, _ := s.config.GetLocalNodeUUID()
				if id == localID {
					continue
				}
				member := s.memberList.GetMemberByID(id)
				if member == nil {
					continue
				}
				if presenceCount[id] < majority && member.GetStatus() == membership.StatusUnknown {
					// Start a quorum vote to remove this member
					if s.quorumManager == nil || len(s.config.Nodes) < 3 {
						s.logger.Infof("Resync: member %s missing from majority but quorum unavailable; skipping automatic removal", id)
						continue
					}
					subject := id
					description := fmt.Sprintf("Remove node %s due to absence from majority and unknown status", id)
					sessionID, err := s.quorumManager.StartVotingSession(quorum.VoteTypeConfigChange, subject, description, 30*time.Second)
					if err != nil {
						s.logger.Warn("Resync: failed to start removal vote", "id", id, "error", err)
						continue
					}

					// Poll for result (short window)
					passed := false
					for i := 0; i < 30; i++ {
						time.Sleep(1 * time.Second)
						session, err := s.quorumManager.GetVotingSession(sessionID)
						if err != nil {
							s.logger.Warn("Resync: failed to get voting session", "sessionID", sessionID, "error", err)
							break
						}
						if session != nil && session.Result != nil {
							passed = session.Result.Passed && session.Result.QuorumMet
							break
						}
					}

					if passed {
						// Apply removal locally
						_ = s.memberList.RemoveMember(id)
						delete(s.config.Nodes, id)
						_ = s.config.Save()

						// Broadcast updated config to peers (best-effort)
						configBytes, err := json.Marshal(s.config)
						if err == nil {
							for peerID, node := range s.config.Nodes {
								if peerID == localID {
									continue
								}
								remoteClient, err := s.getPeerClient(peerID, node)
								if err != nil {
									continue
								}
								ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
								_, _ = remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: configBytes})
								cancel()
							}
						}
					}
				}
			}

			return &rpc.ResyncNetworkResponse{Success: true, Message: "network resynced and cluster activated"}, nil
		}

		// For clusters <3 nodes, only log; manual cleanup required
		s.logger.Info("Resync: cluster size <3; membership reconciliation requires manual removal. No automatic changes applied.")
		return &rpc.ResyncNetworkResponse{Success: true, Message: "network resynced and activated (manual membership management)"}, nil
	}

	return &rpc.ResyncNetworkResponse{Success: true, Message: "network resynced"}, nil
}

// BringUpIP implements the Server.BringUpIP RPC for remote IP assignment
func (s *Server) BringUpIP(ctx context.Context, req *rpc.UpIpRequest) (*rpc.UpIpResponse, error) {
	s.logger.Infof("RPC BringUpIP on iface %s for %d IP(s)", req.Iface, len(req.Ips))

	// Ensure interface exists
	exists, _ := network.InterfaceExist(req.Iface)
	if !exists {
		return &rpc.UpIpResponse{Success: false, Message: "interface does not exist"}, nil
	}

	// Normalized once, up front, so the expectation set below can be registered
	// for the whole request. An address that cannot be parsed rejects the request
	// without touching the interface — see normalizeUpRequest.
	normalized, invalid := normalizeUpRequest(req.Ips)
	if len(invalid) > 0 {
		s.logger.Error("BringUpIP: rejecting request carrying unparseable addresses",
			"iface", req.Iface, "invalid", invalid, "of", len(req.Ips))
		return &rpc.UpIpResponse{Success: false, Message: "invalid IP"}, nil
	}

	// One call for the whole request, not one per address. AddExpectedIPs takes the
	// monitor lock and calls TriggerEnforce — which starts an enforceExpectations
	// *goroutine*, coalesced since #63 to one pass in flight and one queued. That
	// bound is a backstop for this batching, not a replacement: it caps the passes,
	// while batching is what stops the lock and log churn of a trigger per address.
	// Per address, a 62-address request started 62 concurrent enforce
	// passes, each with its own netlink dump and its own placement loop, racing
	// this handler's own loop as it brought the rest up. That is the herd #34
	// removed from the release path by batching RemoveExpectedIPs, left in place on
	// this one, and it is a large part of how run 32's node-4 came to run 34 enforce
	// placement batches inside one second (docs/TEST-PLAN.md defects #64, #63).
	//
	// Still before the placement below, where the per-address call was: the
	// expectation has to exist before the address does, or the netlink watcher's
	// restore path and the enforce pass disagree about whether it belongs here.
	s.ipMonitor.AddExpectedIPs(req.Iface, normalized)

	// One snapshot for the whole request, so an address this node already holds on
	// the requested interface costs no syscall to recognise. See placeRequestedIPs
	// for why that matters and why a failed dump means attempt everything.
	var heldOn func(ip string) (bool, string)
	if len(normalized) > 0 {
		inventory, invErr := network.BuildIPInventory()
		if invErr != nil {
			s.logger.Warn("BringUpIP: could not read interface addresses; attempting every requested address",
				"iface", req.Iface, "error", invErr)
		} else {
			heldOn = ipInventoryLookup(inventory)
		}
	}

	// Live, per failing address, and only for a failing address: its whole job is
	// to be newer than the syscall that just failed (#45).
	liveHeldOnIface := func(ip string) bool {
		ipOnly, _ := utils.GetCIDR(ip)
		if ipOnly == nil {
			return false
		}
		ex, eIface, err := network.CheckIfIPExists(ipOnly.String())
		return err == nil && ex && eIface == req.Iface
	}

	attempts := placeRequestedIPs(req.Iface, normalized, heldOn, liveHeldOnIface,
		network.BringIPdown, network.BringIPup)
	summary := summarizeUpAttempts(attempts)

	// Per address only for the outcome worth reading, as on the release path (#61).
	for _, attempt := range attempts {
		if attempt.Outcome == upFailed {
			s.logger.Error("BringUpIP failed", "iface", req.Iface, "ip", attempt.IP, "error", attempt.Err)
		}
	}
	if summary.AlreadyHeld > 0 || summary.Moved > 0 || summary.Satisfied > 0 {
		// The positive control for #64: on a redundant re-place this is the line
		// that says the request cost nothing. Debug rather than Info because a
		// converged cluster emits it on every re-place.
		s.logger.Debug("BringUpIP: addresses that needed no placement",
			"iface", req.Iface, "alreadyHeld", summary.AlreadyHeld, "movedFromOtherIface", summary.Moved,
			"heldDespiteFailedAdd", summary.Satisfied, "placed", summary.Placed, "of", len(normalized))
	}

	// Announcements are collected and sent as one batch once every address is up.
	// Announcing inside the placement loop cost about four seconds per address,
	// which for a large group held this RPC — and the caller waiting on it — open
	// for minutes (docs/TEST-PLAN.md defects #4/#8).
	//
	// Batching moved the announcement after the loop, but the loop still abandons
	// the request on the first address it cannot bring up — so every exit has to
	// announce what is already up first. Skipping it leaves those addresses live on
	// the interface and unannounced: peers keep the old MAC in ARP and the traffic
	// goes nowhere, a silent partial outage mid-failover, where the per-IP version
	// had at least announced each success as it happened.
	//
	// The set announced is every address this request got as far as attempting,
	// not the ones the loop concluded were up, and it includes the ones no syscall
	// was made for — see attemptedIPs. Offering that set is safe because the batch
	// re-reads each address against the kernel immediately before its own arping,
	// announcing what the interface holds and returning the rest as skipped: the
	// kernel decides, at announce time.
	if attempted := attemptedIPs(attempts); len(attempted) > 0 {
		skipped, err := network.SendGARPBatch(req.Iface, attempted)
		if err != nil {
			s.logger.Warn("BringUpIP: failed to announce some IPs", "iface", req.Iface, "error", err)
		}
		if len(skipped) > 0 {
			// Reported here rather than in packages/network, whose package-level
			// logger nothing ever calls SetLevel on — a Debug line there cannot
			// reach the journal at any logging_level, so the skip would be
			// unverifiable live (#61's lesson, #33's positive control).
			s.logger.Debug("BringUpIP: skipped announcing addresses this node no longer holds",
				"iface", req.Iface, "count", len(skipped), "of", len(attempted))
		}
	}

	// Abandoned on a genuine failure, as before, and after the announcement above:
	// the addresses placed before it are on the interface either way. The failure
	// is always the last attempt, since the loop stops at it.
	if summary.Failed > 0 {
		message := "failed to bring up IP"
		if last := attempts[len(attempts)-1]; last.Err != nil {
			message = last.Err.Error()
		}
		return &rpc.UpIpResponse{Success: false, Message: message}, nil
	}

	// In active-active mode: if the local node is Passive/Unknown, this BringUpIP
	// call means the coordinator has assigned these IPs to us. Transition to
	// Active so the ENFORCE loop doesn't immediately strip the IPs back off.
	if s.config.Pulse.Mode == "active-active" {
		localID, err := s.config.GetLocalNodeUUID()
		if err == nil {
			localMember := s.memberList.GetMemberByID(localID)
			if localMember != nil {
				var promoted bool
				localMember.UpdateClaim(func(current membership.Claim) (membership.Claim, bool) {
					// Only from Passive or Unknown. A node the operator has put
					// in maintenance must not be promoted by an incoming
					// bring-up, and one already Active needs no transition.
					if current.Status == membership.StatusPassive || current.Status == membership.StatusUnknown {
						current.Status = membership.StatusActive
						promoted = true
					}
					// The normalized forms, not the raw request: this list is what
					// IPMonitor.deriveExpectedIPs matches against the configured group
					// to decide what this node is still expected to hold, so an entry
					// spelled differently there is an entry that expectation misses.
					return current.WithAddresses(normalized...), true
				})
				// Logged after the decision returns, so nothing but the claim
				// itself happens with the member lock held.
				if promoted {
					s.logger.Info("BringUpIP: Transitioned local node to Active for active-active mode")
				}

				// Deliberately no refreshLocalMonitorExpectedIPs() here. That call
				// was defect #64's whole-share re-place, and it was this handler
				// calling it on *every* request that made the flood: the function
				// rescans the node's entire expected share against the kernel and
				// issues a fresh s.BringUpIP for everything it finds missing, which
				// re-enters this handler. So a one-address bring-up arriving mid-
				// convergence turned into a 62-address bring-up — run 32's node-4
				// took 17 requests for the 62 addresses that were already its share
				// plus 14 single-address ones, and the load rejected five correctly
				// batched requests from #37's new batcher with DeadlineExceeded.
				// Both the 62s and the 1s were this site, at different points in
				// convergence; there is no per-address caller of BringUpIP to find.
				//
				// Nothing is lost by dropping it. AddExpectedIPs above already woke
				// the enforce pass exactly once, and that pass does the same job
				// strictly better: in active-active it re-derives the expectation
				// set from this node's own assignments rather than trusting whoever
				// wrote the cache last, takes one interface snapshot for the whole
				// pass, and places what is missing with placeMissingFloatingIPs.
				// Role transitions still refresh — SetMode, Promote, MakePassive,
				// ConfigSync and the election paths all call it — and the 30s
				// periodic reconcile backs all of them up.
			}
		}
	}

	return &rpc.UpIpResponse{Success: true, Message: "IPs brought up"}, nil
}

// BringDownIP implements the Server.BringDownIP RPC for remote IP removal
func (s *Server) BringDownIP(ctx context.Context, req *rpc.DownIpRequest) (*rpc.DownIpResponse, error) {
	s.logger.Infof("RPC BringDownIP on iface %s for %d IP(s)", req.Iface, len(req.Ips))

	normalized, invalid := normalizeDownRequest(req.Ips)
	for _, ip := range invalid {
		s.logger.Warn("BringDownIP skipping invalid IP", "ip", ip)
	}

	// One call for the whole request, not one per address. RemoveExpectedIPs
	// takes the monitor lock, logs the remaining expectation set, and calls
	// TriggerEnforce — which starts an enforceExpectations *goroutine*, coalesced
	// since #63 to one pass in flight and one queued. That bound is a backstop for
	// this batching, not a replacement: it caps the passes, while batching is what
	// stops the lock and log churn of a trigger per address. Per
	// address that was one enforce pass per requested address: 201 of them for a
	// group-delete, each with its own netlink dump and its own release loop,
	// running concurrently with this loop as it deleted the rest. Every one of
	// those passes saw a different half-released set, which is a large part of
	// how defect #34 got to ~18 duplicate release attempts per address. Batched,
	// the request removes the expectations once and wakes the enforce pass once.
	//
	// Still before the bring-downs, as it was: the expectation has to be gone
	// first, or the restore paths put the address straight back (#60).
	s.ipMonitor.RemoveExpectedIPs(req.Iface, normalized)

	// One snapshot for the whole request. See releaseRequestedIPs for why the
	// filter belongs here rather than in the caller; the nil on failure is
	// deliberate — unable to see what this node holds, attempt everything.
	var heldHere func(ip string) bool
	// No addresses to look up means no reason to dump the interface table.
	if len(normalized) > 0 {
		if inventory, invErr := network.BuildIPInventory(); invErr != nil {
			s.logger.Warn("BringDownIP: could not read interface addresses; attempting every requested release",
				"iface", req.Iface, "error", invErr)
		} else {
			heldHere = func(ip string) bool {
				ipOnly, _ := utils.GetCIDR(ip)
				if ipOnly == nil {
					return true
				}
				exists, foundIface, err := inventory.Exists(ipOnly.String())
				if err != nil {
					return true
				}
				return exists && foundIface == req.Iface
			}
		}
	}

	attempts := releaseRequestedIPs(req.Iface, normalized, heldHere, network.BringIPdownClassified)
	summary := summarizeDownAttempts(attempts)

	// Per address only for the outcome worth reading. The other three are
	// counted, and reported through this logger rather than packages/network's,
	// whose level nothing sets — a classification that cannot reach the journal
	// cannot be verified live (#61's lesson).
	for _, attempt := range attempts {
		if attempt.Outcome == downFailed {
			s.logger.Error("BringDownIP failed", "iface", req.Iface, "ip", attempt.IP, "error", attempt.Err)
		}
	}
	if summary.Skipped > 0 || summary.Vanished > 0 {
		s.logger.Debug("BringDownIP: addresses this node was not holding",
			"iface", req.Iface, "notHeld", summary.Skipped, "vanishedBeforeRelease", summary.Vanished,
			"released", summary.Released, "of", len(normalized))
	}

	// In active-active mode keep the local member's ActiveIPs bookkeeping in
	// sync (mirror of BringUpIP) so a later monitor refresh doesn't resurrect
	// IPs that were deliberately moved to another node.
	if s.config.Pulse.Mode == "active-active" {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil {
			if localMember := s.memberList.GetMemberByID(localID); localMember != nil {
				// RemoveActiveIPs rather than a filter written out here. The two
				// were the same loop apart from the LoadFactor recompute, and
				// with that field gone there is nothing left to keep separate.
				localMember.RemoveActiveIPs(req.Ips)
			}
		}
	}

	if summary.Failed > 0 {
		return &rpc.DownIpResponse{Success: true, Message: "Best-effort: some IPs may not have been present"}, nil
	}
	return &rpc.DownIpResponse{Success: true, Message: "IPs brought down"}, nil
}

// InitiateJoin performs a server-driven join against a target member
func (s *Server) InitiateJoin(ctx context.Context, req *rpc.InitiateJoinRequest) (*rpc.InitiateJoinResponse, error) {
	if req == nil || req.TargetHost == "" {
		return &rpc.InitiateJoinResponse{Success: false, Message: "target_host is required"}, nil
	}

	// Serialize with CreateCluster to prevent a TOCTOU race where both pass
	// ClusterCheck() concurrently (both see 0 nodes) and both activate as
	// "first node", producing dual-active in active-passive mode.
	s.clusterInitMu.Lock()
	defer s.clusterInitMu.Unlock()

	// Prevent joining if this node is already part of a cluster
	if s.config != nil && s.config.ClusterCheck() {
		s.logger.Warn("InitiateJoin rejected: node is already part of a cluster",
			"target_host", req.TargetHost)
		return &rpc.InitiateJoinResponse{Success: false, Message: "node is already part of a cluster; leave first"}, nil
	}

	targetPort := req.TargetPort
	if targetPort == "" {
		targetPort = "8080"
	}

	remoteClient, err := client.New()
	if err != nil {
		return &rpc.InitiateJoinResponse{Success: false, Message: "failed to create client: " + err.Error()}, nil
	}
	defer remoteClient.Close()
	if err := remoteClient.Connect(req.TargetHost, targetPort, false); err != nil {
		return &rpc.InitiateJoinResponse{Success: false, Message: "failed to connect to target: " + err.Error()}, nil
	}

	hostname, _ := os.Hostname()
	nodeID := req.NodeId
	if nodeID == "" {
		nodeID = s.config.GenerateNodeID()
	}
	bindPort := req.BindPort
	if bindPort == "" {
		bindPort = "8080"
	}

	// Preflight: if a bind IP is provided, verify we can bind to bind_ip:bind_port locally
	if req.BindIp != "" {
		if err := s.preflightBind(req.BindIp, bindPort); err != nil {
			return &rpc.InitiateJoinResponse{Success: false, Message: "bind preflight failed: " + err.Error()}, nil
		}
	}

	joinReq := &rpc.JoinRequest{
		Hostname: hostname,
		Token:    req.Token,
		NodeId:   nodeID,
		BindIp:   req.BindIp,
		BindPort: bindPort,
	}
	s.logger.Info("INITIATE_JOIN: Sending join request to target",
		"targetHost", req.TargetHost,
		"targetPort", targetPort,
		"nodeID", nodeID,
		"bindIP", req.BindIp,
		"bindPort", bindPort)

	// Bound the outbound Join RPC so a hung target cannot hold clusterInitMu
	// indefinitely and block concurrent CreateCluster / HandleNodeJoin callers.
	joinCtx, joinCancel := context.WithTimeout(ctx, 30*time.Second)
	jResp, jErr := remoteClient.CLI().Join(joinCtx, joinReq)
	joinCancel()
	if jErr != nil {
		s.logger.Error("INITIATE_JOIN: Join request failed", "error", jErr)
		return &rpc.InitiateJoinResponse{Success: false, Message: "join request failed: " + jErr.Error()}, nil
	}
	if !jResp.Success {
		s.logger.Warn("INITIATE_JOIN: Join request returned failure", "message", jResp.Message)
		return &rpc.InitiateJoinResponse{Success: false, Message: jResp.Message}, nil
	}

	s.logger.Info("INITIATE_JOIN: Join request successful",
		"nodeID", jResp.NodeId,
		"message", jResp.Message,
		"configReceived", jResp.ClusterConfig != nil,
		"configSize", len(jResp.ClusterConfig))

	// If target returned full cluster config, sync it locally
	if jResp.ClusterConfig != nil {
		s.logger.Info("INITIATE_JOIN: Received cluster config from target, syncing locally")

		// Log what's in the config
		var preview map[string]interface{}
		if err := json.Unmarshal(jResp.ClusterConfig, &preview); err == nil {
			if nodes, ok := preview["nodes"].(map[string]interface{}); ok {
				s.logger.Info("INITIATE_JOIN: Received config contains nodes", "nodeCount", len(nodes))
				for id := range nodes {
					s.logger.Info("INITIATE_JOIN: Config includes node", "nodeID", id)
				}
			}
		}

		// Ensure our local server knows its own node ID before applying the synced config
		s.config.Pulse.LocalNode = jResp.NodeId
		s.logger.Info("INITIATE_JOIN: Set local node ID", "localNodeID", jResp.NodeId)

		syncResp, syncErr := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: jResp.ClusterConfig})
		if syncErr != nil {
			s.logger.Error("INITIATE_JOIN: Config sync failed", "error", syncErr)
			return &rpc.InitiateJoinResponse{Success: false, Message: "config sync failed: " + syncErr.Error()}, nil
		}
		s.logger.Info("INITIATE_JOIN: Config sync completed",
			"success", syncResp.Success,
			"message", syncResp.Message)
	} else {
		s.logger.Warn("INITIATE_JOIN: No cluster config received from target, using minimal local update")
		// Minimal local update: seed nodes so Reconfigure can bind and health checks can start
		hostname, _ := os.Hostname()
		s.config.Pulse.LocalNode = jResp.NodeId
		if s.config.Nodes == nil {
			s.config.Nodes = make(map[string]*config.Node)
		}
		// Seed local node from provided bind params
		if _, ok := s.config.Nodes[jResp.NodeId]; !ok {
			s.config.Nodes[jResp.NodeId] = &config.Node{Hostname: hostname, IP: req.BindIp, Port: req.BindPort, IPGroups: map[string][]string{}}
		}
		// Do not create placeholder nodes; rely on leader to push full config keyed by UUID
		_ = s.config.Save()
		if err := s.Reconfigure(); err != nil {
			return &rpc.InitiateJoinResponse{Success: false, Message: "reconfigure failed: " + err.Error()}, nil
		}
		// Ensure local member list is populated immediately for CLI status
		s.memberList.UpdateConfig(s.config)
		if err := s.loadInitialMembers(s.currentConfig()); err != nil {
			s.logger.Warn("InitiateJoin: failed to load members after minimal config", "error", err)
		}

		// No placeholder resolution needed; IDs are authoritative and pushed by leader
	}

	// Ensure health checker is running post-join and reconcile VIPs according to local role
	s.startHealthChecker()
	// REMOVED: Redundant refresh call - health checker already handles VIP reconciliation after join
	// The startHealthChecker above will trigger health check logic that handles VIP assignments
	// go s.refreshLocalMonitorExpectedIPs()

	// Broadcast current states to peers to converge views (best-effort)
	states := make(map[string]membership.MemberStatus)
	for id, m := range s.memberList.MembersSnapshot() {
		states[id] = m.GetStatus()
	}
	_ = s.broadcastNextEpoch(states)

	return &rpc.InitiateJoinResponse{Success: true, Message: "join initiated"}, nil
}

// preflightBind verifies that we can bind a TCP listener on the given ip:port
// It opens a short-lived listener and closes it immediately.
func (s *Server) preflightBind(ip, port string) error {
	addr := net.JoinHostPort(ip, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_ = ln.Close()
	return nil
}

// OrchestrateIPFailover moves a set of floating IPs from an old active node to a new active node.
// It brings the IPs down on the old node first (best-effort) and then brings them up on the new node,
// using the server's IP helper RPCs (or local equivalents) grouped per interface according to config.
func (s *Server) OrchestrateIPFailover(oldNodeID, newNodeID string, ips []string) error {
	s.logger.Info("IP_FAILOVER: Starting orchestration",
		"old_node", oldNodeID,
		"new_node", newNodeID,
		"ip_count", len(ips))

	// Group IPs per interface for old and new nodes based on current configuration
	oldIfaceToIPs, err := s.groupIPsByInterfaceForNode(oldNodeID, ips)
	if err != nil {
		// Old node grouping failure should not block bringing IPs up elsewhere; log and continue
		s.logger.Warn("IP_FAILOVER: Failed to map IPs to interfaces on old node", "node", oldNodeID, "error", err)
		oldIfaceToIPs = map[string][]string{}
	}

	newIfaceToIPs, err := s.groupIPsByInterfaceForNode(newNodeID, ips)
	if err != nil {
		return fmt.Errorf("failed to map IPs to interfaces on new node: %w", err)
	}

	s.logger.Info("IP_FAILOVER: Grouped IPs by interface",
		"old_node_interfaces", len(oldIfaceToIPs),
		"new_node_interfaces", len(newIfaceToIPs))

	// OPTIMIZATION: Parallelize bring-down operations across interfaces
	// Best-effort: bring down IPs on old node per interface
	if oldNodeID != "" && len(oldIfaceToIPs) > 0 {
		s.logger.Info("IP_FAILOVER: Bringing down IPs on old node (parallel)",
			"old_node", oldNodeID,
			"interface_count", len(oldIfaceToIPs))

		var wg sync.WaitGroup
		for iface, ipList := range oldIfaceToIPs {
			wg.Add(1)
			go func(iface string, ipList []string) {
				defer wg.Done()
				if oldNodeID == s.config.Pulse.LocalNode {
					// Local: call helper directly
					if _, derr := s.BringDownIP(context.Background(), &rpc.DownIpRequest{Iface: iface, Ips: ipList}); derr != nil {
						s.logger.Warn("IP_FAILOVER: Failed to bring IPs down locally on old node", "iface", iface, "error", derr)
					} else {
						s.logger.Debug("IP_FAILOVER: Successfully brought down IPs locally", "iface", iface, "count", len(ipList))
					}
				} else {
					if derr := s.bringIPsOnNodeDown(oldNodeID, iface, ipList); derr != nil {
						s.logger.Warn("IP_FAILOVER: Failed to bring IPs down on old node", "node", oldNodeID, "iface", iface, "error", derr)
					} else {
						s.logger.Debug("IP_FAILOVER: Successfully brought down IPs remotely", "node", oldNodeID, "iface", iface, "count", len(ipList))
					}
				}
			}(iface, ipList)
		}
		wg.Wait()
		s.logger.Info("IP_FAILOVER: Completed bringing down IPs on old node")
	}

	// OPTIMIZATION: Parallelize bring-up operations across interfaces
	// Bring up IPs on new node per interface
	if len(newIfaceToIPs) > 0 {
		s.logger.Info("IP_FAILOVER: Bringing up IPs on new node (parallel)",
			"new_node", newNodeID,
			"interface_count", len(newIfaceToIPs))

		type bringUpResult struct {
			iface string
			err   error
		}
		resultChan := make(chan bringUpResult, len(newIfaceToIPs))

		var wg sync.WaitGroup
		for iface, ipList := range newIfaceToIPs {
			wg.Add(1)
			go func(iface string, ipList []string) {
				defer wg.Done()
				var uerr error
				if newNodeID == s.config.Pulse.LocalNode {
					// Local: call helper directly
					_, uerr = s.BringUpIP(context.Background(), &rpc.UpIpRequest{Iface: iface, Ips: ipList})
					if uerr != nil {
						s.logger.Error("IP_FAILOVER: Failed to bring IPs up locally", "iface", iface, "error", uerr)
					} else {
						s.logger.Debug("IP_FAILOVER: Successfully brought up IPs locally", "iface", iface, "count", len(ipList))
					}
				} else {
					uerr = s.bringIPsOnNodeUp(newNodeID, iface, ipList)
					if uerr != nil {
						s.logger.Error("IP_FAILOVER: Failed to bring IPs up remotely", "node", newNodeID, "iface", iface, "error", uerr)
					} else {
						s.logger.Debug("IP_FAILOVER: Successfully brought up IPs remotely", "node", newNodeID, "iface", iface, "count", len(ipList))
					}
				}
				resultChan <- bringUpResult{iface: iface, err: uerr}
			}(iface, ipList)
		}
		wg.Wait()
		close(resultChan)

		// Check for any critical bring-up failures
		var failedInterfaces []string
		for result := range resultChan {
			if result.err != nil {
				failedInterfaces = append(failedInterfaces, result.iface)
			}
		}

		if len(failedInterfaces) > 0 {
			s.logger.Error("IP_FAILOVER: Some interfaces failed to bring up IPs",
				"failed_interfaces", failedInterfaces,
				"total_interfaces", len(newIfaceToIPs))
			return fmt.Errorf("failed to bring up IPs on %d/%d interfaces: %v",
				len(failedInterfaces), len(newIfaceToIPs), failedInterfaces)
		}

		s.logger.Info("IP_FAILOVER: Successfully brought up all IPs on new node")
	}

	// For local node, refresh expected IPs for the interfaces involved
	if s.ipMonitor != nil && newNodeID == s.config.Pulse.LocalNode {
		s.logger.Debug("IP_FAILOVER: Refreshing IP monitor expected IPs", "interface_count", len(newIfaceToIPs))
		for iface := range newIfaceToIPs {
			// Recompute from authoritative config, honouring this node's assignments.
			// The BringUpIP above has already recorded the moved addresses per-IP, so
			// in active-active this must not widen the set back out to the whole group.
			ifaceIPs := s.expectedIfaceIPs(newNodeID, iface)
			s.ipMonitor.ClearExpectedIPs(iface)
			if len(ifaceIPs) > 0 {
				s.ipMonitor.UpdateExpectedIPs(iface, ifaceIPs)
			}
		}
	}

	s.logger.Info("IP_FAILOVER: Orchestration completed successfully")
	return nil
}

// groupIPsByInterfaceForNode maps IPs to interfaces for a specific node based on group assignments
func (s *Server) groupIPsByInterfaceForNode(nodeID string, ips []string) (map[string][]string, error) {
	ifaceToIPs := make(map[string][]string)

	nodeCfg := s.config.Nodes[nodeID]
	if nodeCfg == nil {
		return nil, fmt.Errorf("node configuration not found for %s", nodeID)
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
		for g, ipList := range s.config.Groups {
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
			return nil, fmt.Errorf("group %s not assigned to any interface on node %s", groupName, nodeID)
		}
		ifaceToIPs[iface] = append(ifaceToIPs[iface], ip)
	}
	return ifaceToIPs, nil
}

// Remote IP-batch deadline sizing.
//
// A flat 5s on both helpers below was defect #57. `BringUpIP` and `BringDownIP`
// each carry a whole batch, so their cost scales with the batch; run 25 moved
// 23–24 addresses at a time onto a node already bringing up ~71, the bring-up
// RPC overran, and the coordinator logged `IP_FAILOVER: Failed to bring IPs up
// remotely … DeadlineExceeded` → `ACTIVE_CHECK: rebalance move failed` for seven
// moves whose addresses had all in fact arrived. Same family as #39/#13/#21/#31:
// the returned status was not evidence of what happened — and here it is read,
// not just logged, since the rebalance loop breaks on it.
//
// Bring-down is #52's demotion shape exactly — release and verify, per address —
// so it reuses that sizing, as `releaseDeletedGroupIPs` already does.
//
// Bring-up needs more than the address work, which is why 5s was too short for a
// batch that was up in well under a second: the RPC ends in one gratuitous-ARP
// batch, and those arping waves dominate the call. Capped for the same reason
// DemotionTimeoutFor is — this deadline is a bound on how long a coordinator
// blocks in a single move.
const bringUpMaxTimeout = 120 * time.Second

func bringUpTimeoutFor(ipCount int) time.Duration {
	timeout := membership.DemotionTimeoutFor(ipCount) + network.AnnounceBatchTimeout(ipCount)
	if timeout > bringUpMaxTimeout {
		return bringUpMaxTimeout
	}
	return timeout
}

// announceDebounceInterval is the shortest gap between two re-announcements.
//
// The trigger has to accept `Unknown -> Passive`, because that is the transition a healed
// partition produces — and it is also the transition a merely *slow* peer produces, over and
// over. A node doing bulk IP work is slow enough to answer that its peers mark it Unknown and
// then Passive again a tick later (docs/TEST-PLAN.md defects #2/#26), and the two are
// indistinguishable from here. Without a bound, each flap re-places and re-announces the whole
// group: on the 201-address topology that is #4's per-address arping cost paid on a peer's
// health-check jitter.
//
// One announcement is all a stale ARP cache needs, so suppressing the repeats inside the window
// costs nothing the first one did not already buy.
const announceDebounceInterval = 30 * time.Second

// allowAnnounce reports whether enough time has passed since the last re-announcement, and
// records this one when it says yes.
//
// Applied to every caller rather than only to the demotion detector. Consolidation is rare
// enough that it will not be suppressed in practice, and if it is, the only way that happens
// is that something announced these same addresses from this same node moments earlier — so
// the segment already holds what the suppressed announcement would have told it.
func (s *Server) allowAnnounce() bool {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()

	now := time.Now()
	if !s.lastAnnounce.IsZero() &&
		now.Sub(s.lastAnnounce) < announceDebounceInterval {
		return false
	}
	s.lastAnnounce = now
	return true
}

// announcePlan maps each of nodeID's interfaces to the floating IPs that node is configured to
// hold on it, skipping interfaces with nothing to announce.
//
// Derived from config through expectedIfaceIPs, never from the member record. In
// active-passive a peer does not self-report what it hosts, so a remote node's ActiveIPs is
// empty and a plan built from it would announce nothing at all — silently, and precisely for
// the remote target that consolidation most often picks (ADR-0001).
//
// Reads s.config unlocked, and so does expectedIfaceIPs below it, which means a ConfigSync
// swapping the pointer mid-plan can have the node entry and the group list come from different
// configs — #71's shape, on Server.config rather than MemberList.config. Left that way rather
// than half-locked, for two reasons. Taking s.RLock here would put it above the member-list
// lock that expectedIfaceIPs' active-active branch takes, inverting an order nothing else in
// this file inverts. And a stale plan is harmless *here* specifically: SendGARPBatch re-reads
// every address against the kernel immediately before its own arping and returns the rest as
// skipped (#33), so an address this plan names but the node does not hold is not announced.
// The kernel decides, at announce time. That is a property of the announce path, not a general
// licence — a caller using this plan to *place* addresses would not be covered by it.
func (s *Server) announcePlan(nodeID string) (map[string][]string, error) {
	node := s.config.Nodes[nodeID]
	if node == nil {
		return nil, fmt.Errorf("no configuration for node %s", nodeID)
	}

	plan := make(map[string][]string, len(node.IPGroups))
	for iface := range node.IPGroups {
		if ips := s.expectedIfaceIPs(nodeID, iface); len(ips) > 0 {
			plan[iface] = ips
		}
	}
	return plan, nil
}

// AnnounceNodeIPs re-places on nodeID every floating IP that node is configured to hold, for
// the sole purpose of making it announce them.
//
// A gratuitous ARP is only ever sent by a bring-up — bringUpIPsLocally, the enforce loop's
// missing set, and BringUpIP — and there is no periodic re-announce. So a node that has held
// an address continuously never announces it again, however wrong the segment's idea of who
// owns it has become. Consolidation after a two-node split-brain is exactly that case: both
// nodes held the whole group, the loser's bring-up announced last, and when the loser is
// demoted every ARP cache still points at it while the surviving node stays silent. The
// address is then present on the survivor and reachable by nobody until the caches age out.
// See docs/adr/0002-two-node-availability-over-safety.md.
//
// Expressed as a re-place rather than a bare announce because BringUpIP already announces
// every address it got as far as attempting, including the ones it made no syscall for, and
// treats an address already on the interface as satisfied (#45/#64). A redundant re-place
// therefore costs no placement and yields the announcement; summary.AlreadyHeld is the log
// line that says so.
//
// Called without the server lock held, as refreshLocalMonitorExpectedIPs does, so that
// expectedIfaceIPs' active-active branch can take the member-list lock without inverting the
// order.
//
// Best effort by construction. Every address is up either way; failing to announce leaves the
// pre-existing dark window rather than creating one, so failures are reported and the
// remaining interfaces are still attempted.
func (s *Server) AnnounceNodeIPs(nodeID string) error {
	// Debounced here rather than at each caller, because the trigger that drives most of
	// these has to accept a peer arriving from Unknown — which is what a healed partition
	// produces and also what a merely slow peer produces (defects #2/#26). Unbounded, a
	// flapping peer re-places and re-announces the whole group on every flap: on the
	// 201-address topology that is #4's per-address arping cost paid on health-check jitter.
	if !s.allowAnnounce() {
		s.logger.Debug("ANNOUNCE: suppressed, one was made within the debounce window",
			"node_id", nodeID, "interval", announceDebounceInterval)
		return nil
	}

	plan, err := s.announcePlan(nodeID)
	if err != nil {
		return err
	}

	localID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		return fmt.Errorf("failed to resolve local node: %w", err)
	}

	var failed []string
	announced := 0
	for iface, ips := range plan {
		if nodeID == localID {
			ctx, cancel := context.WithTimeout(context.Background(), bringUpTimeoutFor(len(ips)))
			_, err = s.BringUpIP(ctx, &rpc.UpIpRequest{Iface: iface, Ips: ips})
			cancel()
		} else {
			err = s.bringIPsOnNodeUp(nodeID, iface, ips)
		}

		if err != nil {
			s.logger.Error("ANNOUNCE: failed to re-announce floating IPs",
				"node_id", nodeID, "iface", iface, "count", len(ips), "error", err)
			failed = append(failed, iface)
			continue
		}
		s.logger.Info("ANNOUNCE: re-announced floating IPs",
			"node_id", nodeID, "iface", iface, "count", len(ips))
		announced += len(ips)
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to announce on %d interface(s) of node %s: %v", len(failed), nodeID, failed)
	}
	s.logger.Debug("ANNOUNCE: completed", "node_id", nodeID, "addresses", announced)
	return nil
}

// bringIPsOnNodeUp contacts a specific node and asks it to bring IPs up on the given interface
func (s *Server) bringIPsOnNodeUp(nodeID, iface string, ips []string) error {
	node := s.config.Nodes[nodeID]
	if node == nil {
		return fmt.Errorf("node configuration not found")
	}
	remoteClient, err := client.New()
	if err != nil {
		return err
	}
	defer remoteClient.Close()
	if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), bringUpTimeoutFor(len(ips)))
	defer cancel()
	_, err = remoteClient.Server().BringUpIP(ctx, &rpc.UpIpRequest{Iface: iface, Ips: ips})
	return err
}

// bringIPsOnNodeDown contacts a specific node and asks it to bring IPs down on the given interface
func (s *Server) bringIPsOnNodeDown(nodeID, iface string, ips []string) error {
	node := s.config.Nodes[nodeID]
	if node == nil {
		return fmt.Errorf("node configuration not found")
	}
	remoteClient, err := client.New()
	if err != nil {
		return err
	}
	defer remoteClient.Close()
	if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), membership.DemotionTimeoutFor(len(ips)))
	defer cancel()
	_, err = remoteClient.Server().BringDownIP(ctx, &rpc.DownIpRequest{Iface: iface, Ips: ips})
	return err
}

// GetClusterEpoch returns the current cluster epoch (term)
func (s *Server) GetClusterEpoch() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.clusterEpoch
}

// GetLeaderID returns the current leader ID
func (s *Server) GetLeaderID() string {
	s.RLock()
	defer s.RUnlock()
	return s.leaderID
}

// convergenceMetadata returns the cluster epoch and its leader as one consistent
// pair.
//
// Reading the two fields separately can straddle an adopt and pair a new epoch
// with the previous leader, which reads as a decision from the wrong node.
func (s *Server) convergenceMetadata() (epoch int64, leaderID string) {
	s.RLock()
	defer s.RUnlock()
	return s.clusterEpoch, s.leaderID
}

// broadcastNextEpoch publishes memberStates at one past the current epoch,
// attributed to the node the cluster currently believes is Active.
//
// Exists because the idiom it replaces was wrong in a way that read as correct.
// Twelve sites passed the epoch through the locking accessor and the elected
// node bare, in one expression: GetClusterEpoch() takes the read lock and
// releases it, then s.leaderID was read with nothing held. So the two were
// never read together, and an adopt landing between them pairs a new epoch with
// the previous one's leader.
//
// That is not only a race-detector complaint, and the reason is worth stating
// precisely. BroadcastClusterState gates the epoch on `epoch > s.clusterEpoch`
// but assigns s.leaderID *unconditionally*, outside that check -- reasonable in
// itself, since this is the outgoing path where the node asserts its own view
// rather than the incoming one where a peer's claim has to be epoch-gated. The
// consequence is that a stale leaderID is installed as the elected node and
// broadcast to every peer as authoritative, with no epoch comparison standing
// in its way. The caller passing a self-consistent pair is the only protection
// there is.
//
// convergenceMetadata returns the two under one acquisition, which is what was
// needed: the pair has to be self-consistent, not fresh. Staleness is already
// handled, because BroadcastClusterState re-checks the epoch under its own lock
// before adopting anything.
//
// A method rather than two lines at each site, so the invariant lives in one
// place -- and so that the four calls sharing one scope in
// performPromotionAsync do not need four differently-named epoch variables.
func (s *Server) broadcastNextEpoch(memberStates map[string]membership.MemberStatus) error {
	epoch, electedNode := s.convergenceMetadata()
	return s.BroadcastClusterState(memberStates, epoch+1, electedNode, nil)
}

// adoptConvergenceMetadata records an incoming epoch and its leader, and reports
// whether they were adopted.
//
// Takes the server lock. Callers already holding it want
// adoptConvergenceMetadataLocked — the lock is not reentrant, so calling this
// one from inside a locked region deadlocks.
func (s *Server) adoptConvergenceMetadata(epoch int64, leaderID string, atLeast bool) bool {
	s.Lock()
	defer s.Unlock()
	return s.adoptConvergenceMetadataLocked(epoch, leaderID, atLeast)
}

// adoptConvergenceMetadataLocked is adoptConvergenceMetadata for callers that
// already hold the server lock.
//
// atLeast selects the comparison: false adopts a strictly newer epoch, true also
// adopts an equal one (re-asserting the same epoch under its leader). The compare
// and the write are one critical section, because two syncs arriving together
// would otherwise both compare against the same stale read and the later writer
// would win regardless of epoch.
//
// This split exists because the constraint above used to be paid for by
// duplication. ConfigSync's full-config branch runs inside s.Lock(), could not
// call the locking version, and so carried a hand-copied compare-and-write of
// its own — two implementations of one rule, kept in step by whoever remembered
// both. Non-reentrancy causing copy-paste rather than a deadlock is the quieter
// half of the same problem (docs/adr/0003, END-2339).
func (s *Server) adoptConvergenceMetadataLocked(epoch int64, leaderID string, atLeast bool) bool {
	if epoch < s.clusterEpoch || (epoch == s.clusterEpoch && !atLeast) {
		return false
	}
	s.clusterEpoch = epoch
	s.leaderID = leaderID
	return true
}

// selfReportedAddresses is what this node tells its peers it holds, in
// active-active, so that whichever of them becomes redistribution coordinator
// knows what to account for if this node fails.
//
// Always non-nil, and that is load-bearing rather than defensive. ConfigSync
// gates the incoming report on `senderActiveIPs != nil`, so an empty slice
// marshals to `[]` and says "I hold nothing, clear my list", while nil marshals
// to `null` and says "no report, keep what you have". A node that had released
// every address would otherwise go quiet about it and its peers would keep
// counting those addresses as hosted, which is defect #58's shape.
//
// Member.GetActiveIPs returns nil for an empty set, so the distinction has to be
// restored here. It is a function rather than an inline append because nothing
// in the suite caught the difference when it was inline (END-2339).
func selfReportedAddresses(m *membership.Member) []string {
	held := m.GetActiveIPs()
	if held == nil {
		return []string{}
	}
	return held
}

// GetLeaderLeaseUntil returns the current leader lease expiry
func (s *Server) GetLeaderLeaseUntil() time.Time {
	s.RLock()
	defer s.RUnlock()
	return s.leaderLeaseUntil
}

// getPeerClient gets or creates a gRPC connection to a peer node (thread-safe with connection pooling)
func (s *Server) getPeerClient(peerID string, node *config.Node) (*client.Client, error) {
	// Try to get existing connection with read lock
	s.clientMutex.RLock()
	c, exists := s.peerClients[peerID]
	s.clientMutex.RUnlock()

	if exists && c != nil && c.Connection != nil {
		return c, nil // Reuse existing connection
	}

	// Need to create new connection - acquire write lock
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	// Double-check after acquiring write lock (another goroutine may have created it)
	if c, exists := s.peerClients[peerID]; exists && c != nil && c.Connection != nil {
		return c, nil
	}

	// Create new client and connection
	remoteClient, err := client.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
		remoteClient.Close()
		return nil, fmt.Errorf("failed to connect to %s:%s: %w", utils.FormatIPv6(node.IP), node.Port, err)
	}

	s.peerClients[peerID] = remoteClient
	s.logger.Debug("Created new peer connection", "peerID", peerID, "address", node.IP+":"+node.Port)
	return remoteClient, nil
}

// closePeerClients closes all peer client connections (should be called during shutdown)
func (s *Server) closePeerClients() {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	for peerID, c := range s.peerClients {
		if c != nil {
			c.Close()
			s.logger.Debug("Closed peer connection", "peerID", peerID)
		}
	}
	s.peerClients = make(map[string]*client.Client)
}

// BroadcastClusterState broadcasts member states and convergence metadata to peers via ConfigSync
func (s *Server) BroadcastClusterState(memberStates map[string]membership.MemberStatus, epoch int64, leaderID string, leases map[string]string) error {
	s.Lock()
	if epoch > s.clusterEpoch {
		s.clusterEpoch = epoch
	}
	s.leaderID = leaderID
	// If we are leader, extend our lease
	if leaderID != "" && leaderID == s.config.Pulse.LocalNode {
		s.leaderLeaseUntil = time.Now().Add(5 * time.Second)
	}
	s.Unlock()

	// Build an envelope-only payload (no full config) to avoid triggering reconfigure
	payload := make(map[string]interface{})
	// Attach convergence metadata
	ms := make(map[string]int)
	for id, st := range memberStates {
		ms[id] = int(st)
	}
	payload["member_states"] = ms
	payload["epoch"] = epoch
	payload["leader_id"] = leaderID
	if leases != nil {
		payload["leases"] = leases
	}

	// The envelope is what makes divergence detectable in seconds rather than
	// never: it flows continuously, where a full config only follows a mutation.
	// A hash that cannot be computed is left off entirely rather than sent empty,
	// so the receiver treats it as unknown instead of comparing against "".
	if hash, err := sharedConfigHash(s.currentConfig()); err != nil {
		s.logger.Warn("CLUSTER_STATE: could not fingerprint the config; peers "+
			"cannot detect divergence from this node until it succeeds", "error", err)
	} else if hash != "" {
		payload["config_hash"] = hash
	}

	localID, _ := s.config.GetLocalNodeUUID()

	if localID != "" {
		if localMember := s.memberList.GetMemberByID(localID); localMember != nil {
			// The two arms below are mutually exclusive on mode, so they are two
			// operations rather than one locked region: nothing needs the clear
			// and the self-report to be atomic with each other, because no mode
			// does both.
			if s.config.Pulse.Mode != "active-active" {
				// A non-active node cannot host floating IPs — the IP monitor
				// enforces that on the interface — so clear any stale claim before
				// reporting state. Otherwise peers keep counting these IPs as
				// hosted and the reconciler never re-places them, and status shows
				// IPs on a node that doesn't actually hold them. Active-passive
				// demotions (mode switch, failover, maintenance) all leave the same
				// stale claims behind.
				//
				// The status and the addresses are read and written in one
				// acquisition, because the decision is about their relationship:
				// a status read before a promotion paired with a clear applied
				// after it would strip a node that had just legitimately gone
				// Active.
				localMember.UpdateClaim(func(current membership.Claim) (membership.Claim, bool) {
					if current.Status == membership.StatusActive || len(current.ActiveIPs) == 0 {
						return current, false
					}
					current.ActiveIPs = nil
					return current, true
				})
			} else {
				// Active-active is excluded from the clear above because there is
				// no demotion to clean up after: every eligible node is Active, so
				// a non-Active status there is a transient — a missed health check,
				// a peer's stale broadcast. Discarding the assignment map over one
				// of those cost the node every address the coordinator had given
				// it, and the addresses were off the cluster until it noticed and
				// re-placed them (docs/TEST-PLAN.md defects #2/#26). The map is
				// what the coordinator decided, not a claim about this node's
				// health.
				//
				// Instead, self-report what this node holds so peers keep an
				// accurate view of the assignment map. Without this, a peer that
				// takes over as redistribution coordinator wouldn't know which
				// IPs this node held when it failed.
				payload["sender_id"] = localID
				payload["sender_active_ips"] = selfReportedAddresses(localMember)
			}
		}
	}

	enhancedBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// Apply locally via the same path to ensure consistency
	_, _ = s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: enhancedBytes})

	// Broadcast to peers best-effort using connection pool
	for peerID, node := range s.config.Nodes {
		if peerID == localID {
			continue
		}
		remoteClient, err := s.getPeerClient(peerID, node)
		if err != nil {
			// Connection failed, skip this peer
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: enhancedBytes})
		cancel()
	}
	return nil
}

// buildFullConfigPayload renders the cluster config and the member states that
// belong with it into a single ConfigSync payload, plus the sender identity and
// config version that let the receiver decide whether this payload is newer than
// what it already holds (docs/TEST-PLAN.md defects #5 and #38).
//
// ConfigSync recognises a full config by its "pulseha" root key and reads
// member_states/epoch/leader_id off the same JSON object, so one message can carry
// both. Keeping them in one message is the whole point — see
// broadcastConfigAndStates.
//
// There is deliberately no unstamped wrapper around this. There was one —
// buildConfigAndStatePayload, hardcoding an empty senderID and stamp — and
// SetMode's direct push was its only caller, which is how the cluster's most
// ordering-sensitive broadcast came to be the one broadcast nobody could order.
// Callers that genuinely want the unversioned form pass it explicitly.
//
// An empty origin / version 0 means unversioned: the receiver cannot order it and
// applies it. That is the required behaviour for a peer still running an older
// binary during a rolling upgrade, so the guard degrades to today's
// last-writer-wins rather than dropping the message. The key is deliberately
// `config_version` and not the `config_generation` it replaced: an older binary
// reads that key as a per-sender generation with different semantics, and a mixed
// cluster is better off treating the field as absent than comparing two clocks.
//
// `config_origin` is sent alongside `sender_id` and is not the same field.
// sender_id is whoever put these bytes on the wire; config_origin is whoever's
// mutation produced the content, and they differ on every re-broadcast — the
// coordinator re-sends the cluster's config once a minute without having authored
// it. The receiver's tiebreak needs the latter (see configStamp). A peer running a
// binary that omits the key falls back to sender_id, which is correct for a direct
// mutation broadcast and no worse than the previous behaviour otherwise.
func buildFullConfigPayload(cfg *config.Config, states map[string]membership.MemberStatus,
	epoch int64, leaderID, senderID string, stamp configStamp) ([]byte, error) {

	configBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{}
	if err := json.Unmarshal(configBytes, &payload); err != nil {
		return nil, err
	}

	ms := make(map[string]int, len(states))
	for id, st := range states {
		ms[id] = int(st)
	}
	payload["member_states"] = ms
	payload["epoch"] = epoch
	payload["leader_id"] = leaderID
	if senderID != "" && !stamp.isEmpty() {
		payload["sender_id"] = senderID
		payload["config_version"] = stamp.version
		payload["config_origin"] = stamp.origin
	}

	// The fingerprint of the shared content, so a receiver can tell divergence at
	// an equal generation from agreement -- which the generation alone cannot
	// (docs/TEST-PLAN.md #103). The error is propagated rather than swallowed
	// because it is unreachable: the same config marshalled successfully above.
	hash, err := sharedConfigHash(cfg)
	if err != nil {
		return nil, err
	}
	if hash != "" {
		payload["config_hash"] = hash
	}

	return json.Marshal(payload)
}

// buildRepairConfigPayload is buildFullConfigPayload plus the addressed exemption
// that makes a repair applicable at a generation the requester already holds.
//
// `repair_for` names exactly one node. That is what keeps the exemption from being
// a hole in the generation guard: a copy of this payload delivered to a third node
// carries another node's name and is refused there like any other stale config, so
// the only node it can force anything on is the one that asked.
func buildRepairConfigPayload(cfg *config.Config, states map[string]membership.MemberStatus,
	epoch int64, leaderID, senderID string, stamp configStamp, repairFor string) ([]byte, error) {

	payloadBytes, err := buildFullConfigPayload(cfg, states, epoch, leaderID, senderID, stamp)
	if err != nil {
		return nil, err
	}
	if repairFor == "" {
		return payloadBytes, nil
	}

	payload := map[string]interface{}{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, err
	}
	payload["repair_for"] = repairFor
	return json.Marshal(payload)
}

// nextConfigStamp moves the clock one above everything this node has seen and
// stamps this node as the origin, so the mutation about to be broadcast outranks
// the config every peer holds. Two concurrent mutations on the same node can
// never be handed the same number, and two on different nodes are separated by
// the origin — the receiver's whole ability to order them depends on both.
// Versions start at 1, since 0 means "unversioned".
//
// Takes no lock deliberately: its callers hold s.Lock() (see configStamp).
func (s *Server) nextConfigStamp(origin string) configStamp {
	next := &configStamp{origin: origin}
	for {
		heldPtr := s.configStamp.Load()
		next.version = 1
		if heldPtr != nil {
			next.version = heldPtr.version + 1
		}
		if s.configStamp.CompareAndSwap(heldPtr, next) {
			return *next
		}
	}
}

// markConfigDirty records that the local config changed and wakes the
// broadcaster.
//
// This replaced a `go broadcastFullConfigToPeers()` (since deleted) that used to
// end every group mutation. Each of those goroutines marshalled s.config whenever
// it was scheduled, so N concurrent mutations put N unordered snapshots on the wire and
// the last to arrive won even when it was the oldest — 200 rapid add-ip calls
// left whitecrane's four nodes at 200/189/192/193, permanently. Signalling a
// single broadcaster instead means concurrent mutations coalesce into one
// broadcast of the final state, and only one broadcast from this node is ever in
// flight.
//
// The local node ID is read from s.config directly rather than through
// currentConfig(): every group mutation calls this while holding s.Lock(), where
// taking the read lock self-deadlocks. That makes the read safe at those call
// sites and no less safe than the surrounding code at the one that holds no lock
// (HandleNodeJoin, which mutates s.config bare throughout) — part of the residual
// noted against defect #32, not new exposure. An empty ID degrades the tiebreak
// to last-writer-wins rather than corrupting it.
func (s *Server) markConfigDirty() {
	origin, err := s.config.GetLocalNodeUUID()
	if err != nil {
		origin = ""
	}
	s.nextConfigStamp(origin)
	s.requestConfigBroadcast()
}

// RequestConfigReconcile re-sends the current config to every peer without
// claiming a new generation, repairing a peer that missed a broadcast. Called by
// the health checker's periodic reconcile, which gates it on this node being the
// coordinator — see HealthChecker.reconcileConfigAcrossPeers for why that gate
// matters. A peer already holding this generation ignores the message.
func (s *Server) RequestConfigReconcile() {
	s.requestConfigBroadcast()
}

// requestConfigBroadcast wakes the broadcaster without claiming a new
// generation, for a re-send of the current config rather than a change to it.
//
// The trigger channel has capacity 1 and the send is non-blocking, so a signal
// arriving while a broadcast is already queued is dropped: the queued broadcast
// has not snapshotted the config yet and will pick up everything. A nil channel
// (a Server built directly in a test) makes the select fall to default, so this
// is inert rather than a panic.
func (s *Server) requestConfigBroadcast() {
	select {
	case s.broadcastTrigger <- struct{}{}:
	default:
	}
}

// startConfigBroadcaster runs the single goroutine that owns pushing this node's
// config to its peers. Idempotent — Start and the tests may both call it.
//
// It wakes on a mutation and on its own retry timer. The timer is the fix for
// defect #43: a broadcast that exhausted its four attempts used to log a warning
// deferring to "the periodic reconcile", which only runs on the coordinator, so a
// mutation taken on any other node stayed local for an unbounded time. The node
// that owns the change is the one that retries it now, regardless of who
// coordinates.
func (s *Server) startConfigBroadcaster() {
	s.broadcastOnce.Do(func() {
		if s.broadcastTrigger == nil {
			return
		}
		go func() {
			var (
				retry      <-chan time.Time
				retryTimer *time.Timer
			)
			// A nil retry channel blocks forever, which is the "nothing
			// outstanding" state.
			stopRetry := func() {
				if retryTimer != nil {
					retryTimer.Stop()
					retryTimer = nil
				}
				retry = nil
			}
			for {
				mutated := false
				select {
				case <-s.broadcastStop:
					stopRetry()
					return
				case <-s.broadcastTrigger:
					mutated = true
				case <-retry:
				}
				stopRetry()

				if mutated && !s.lingerForMoreMutations() {
					return
				}

				s.broadcastConfigToPeersOnce()

				if outstanding, ok := s.pendingPropagation(); ok {
					s.logger.Debug("CONFIG_PROPAGATION: scheduling a re-push of an unpropagated config",
						"version", outstanding.version, "peers", outstanding.peers,
						"attempt", outstanding.attempts, "in", outstanding.backoff)
					retryTimer = time.NewTimer(outstanding.backoff)
					retry = retryTimer.C
				}
			}
		}()
	})
}

// configBroadcastLinger is how long the broadcaster waits after a mutation
// before pushing, so a burst of them costs one push rather than one each.
//
// The trigger channel already coalesces *concurrent* mutations: it holds one
// slot and the send is non-blocking, so anything that lands while a broadcast is
// in flight folds into the next one. Serial mutations get no such benefit. Since
// #37 an `add-ip` completes in ~38ms and a healthy broadcast finishes well
// inside that, so the 248 back-to-back adds that produced defect #62 produced
// ~248 broadcasts — 744 ConfigSync RPCs at the receivers, each one a parse, a
// file write, a member-list reload and a `go Reconfigure()`. That is the "every
// version is pushed separately" half of #62: the deadline fix stops the pushes
// being abandoned, this stops most of them being made. The receiver those
// pushes saturate is the same receiver whose slowness the deadline had to
// absorb, so the two fixes work the same problem from both ends.
//
// The window is FIXED, not sliding, for the reason #37 settled on the same
// shape for the bring-up fan-out: a sliding window resets on every mutation, so
// a long enough burst starves the push entirely and the config propagates only
// once the operator stops typing. A fixed window bounds the delay for any single
// mutation at one window and caps the push rate at one per window however long
// the burst runs.
//
// Only a mutation lingers. A retry firing is already late by construction and
// re-pushes immediately.
const configBroadcastLinger = 250 * time.Millisecond

// lingerForMoreMutations waits out the window and then consumes any trigger that
// arrived inside it, reporting false if the broadcaster was stopped instead.
//
// Draining before the caller broadcasts is what makes the coalescing real: the
// snapshot taken next carries every mutation that landed during the window, so
// leaving their triggers behind would push the same config again. It also
// restores the "superseded by a newer broadcast" check in
// broadcastConfigToPeersOnce, which reads the same channel — under a burst that
// check used to see a non-empty trigger on every retry and abandon the pass.
//
// A mutation landing between the drain and the snapshot is not lost either way:
// it is either included in the snapshot, or it re-arms the trigger and gets the
// next window.
func (s *Server) lingerForMoreMutations() bool {
	select {
	case <-s.broadcastStop:
		return false
	case <-time.After(configBroadcastLinger):
	}

	select {
	case <-s.broadcastTrigger:
	default:
	}
	return true
}

// unpropagatedConfig is a config version this node committed but could not push
// to every peer, together with the state of the retry that will.
//
// peers is the diagnostic rather than the work list: the retry re-snapshots and
// re-pushes to everyone, because by then this node's config may have moved on
// again and a peer not in this list may have fallen behind for its own reasons. A
// peer already holding the version ignores the message, so the redundancy costs
// one RPC and cannot diverge anything.
type unpropagatedConfig struct {
	version  int64
	peers    []string
	attempts int
	backoff  time.Duration
}

// The retry schedule. The first wait deliberately outlasts a listener rebind:
// under defect #31 a peer refuses connections for seconds at a time while it
// reconfigures, which is precisely the window the four in-pass attempts (~1.75s
// of backoff) fall inside, so retrying sooner just spends attempts against a
// socket that is not there. The ceiling matches the coordinator's once-a-minute
// reconcile, so a peer that is genuinely down is retried no more often than it
// was before.
const (
	propagationRetryBase = 5 * time.Second
	propagationRetryMax  = 60 * time.Second
)

// recordUnpropagated notes that a broadcast did not reach every peer and returns
// the delay before the next attempt, doubling it on each consecutive failure.
func (s *Server) recordUnpropagated(stamp configStamp, peers []string) time.Duration {
	s.propagationMu.Lock()
	defer s.propagationMu.Unlock()

	next := propagationRetryBase
	attempts := 0
	if s.unpropagated != nil {
		attempts = s.unpropagated.attempts
		if next = s.unpropagated.backoff * 2; next > propagationRetryMax {
			next = propagationRetryMax
		}
	}
	s.unpropagated = &unpropagatedConfig{
		version:  stamp.version,
		peers:    peers,
		attempts: attempts + 1,
		backoff:  next,
	}
	return next
}

// clearUnpropagated drops the retry state after a broadcast every peer accepted,
// reporting how many attempts it took so the repair is visible in the log rather
// than only inferable from the absence of further warnings.
func (s *Server) clearUnpropagated() (attempts int, wasOutstanding bool) {
	s.propagationMu.Lock()
	defer s.propagationMu.Unlock()

	if s.unpropagated == nil {
		return 0, false
	}
	attempts = s.unpropagated.attempts
	s.unpropagated = nil
	return attempts, true
}

// pendingPropagation returns the outstanding retry state, if any.
func (s *Server) pendingPropagation() (unpropagatedConfig, bool) {
	s.propagationMu.Lock()
	defer s.propagationMu.Unlock()

	if s.unpropagated == nil {
		return unpropagatedConfig{}, false
	}
	return *s.unpropagated, true
}

// stopConfigBroadcaster shuts the broadcaster down. Safe to call more than once.
func (s *Server) stopConfigBroadcaster() {
	if s.broadcastStop == nil {
		return
	}
	select {
	case <-s.broadcastStop:
	default:
		close(s.broadcastStop)
	}
}

// Config-push deadline sizing.
//
// A flat 2s on every ConfigSync push was defect #62. Building a 248-address
// group with back-to-back add-ip calls left one peer without the group key at
// all for ~3 minutes: the sender logged `CONFIG_BROADCAST: ConfigSync failed,
// will retry … error=DeadlineExceeded` through all four attempts, and only
// #43's 40s re-push eventually carried it — while every one of those mutations
// had already returned success to the operator.
//
// The defect was written up as #57's mistake on the config path, and the fix
// shape it suggested was to scale with the payload. Measuring the receiver says
// the shape is right but the reason is not, and the difference decides the
// numbers. The payload-proportional half of the handler — three full parses of
// the message, the group deep-copies, the MarshalIndent and the file write —
// costs well under a millisecond per KiB (~1ms for the 9KiB payload run 32
// timed out on, ~4ms at 5000 addresses). The payload is not what overran 2s.
//
// What overran it is everything the handler has to get past before it can start:
// s.Lock(), which every group mutation and the sync's own predecessors hold,
// and the member-list write lock inside UpdateConfig, which the health-check
// cycle holds while it does IP work. On top of that the RPC pays for the
// transport itself — grpc.NewClient dials lazily, so a cached but idle peer
// client establishes TCP, TLS and the HTTP/2 handshake inside this deadline. A
// receiver in the middle of the burst that produced the config is the normal
// case for this RPC, not the pathological one.
//
// So the base carries the fix and is sized for a busy receiver, while the
// per-KiB term keeps the deadline from being blind to the payload the way the
// flat 2s was. That coefficient is headroom, not the measurement above: a bigger
// config also means more enforce work, more netlink and more member state in
// flight on the node receiving it.
//
// The cap is where this deadline hands over to #43's retry, and the two cover
// different faults on purpose. A *busy* peer is what a deadline should wait out;
// an *unavailable* one — run 32's coordinator was unresponsive for ~40s — is
// what the retry exists for, and blocking the broadcaster on it only delays the
// next config behind a peer that is not going to answer either way.
const (
	configSyncBaseTimeout   = 10 * time.Second
	configSyncPerKiBTimeout = 250 * time.Millisecond
	configSyncMaxTimeout    = 20 * time.Second
)

func configSyncTimeoutFor(payloadBytes int) time.Duration {
	if payloadBytes < 0 {
		payloadBytes = 0
	}
	kib := (payloadBytes + 1023) / 1024
	timeout := configSyncBaseTimeout + time.Duration(kib)*configSyncPerKiBTimeout
	if timeout > configSyncMaxTimeout {
		return configSyncMaxTimeout
	}
	return timeout
}

// broadcastConfigToPeersOnce snapshots the config under the read lock, then
// pushes it to every peer, retrying the ones that fail.
//
// The retry is the other half of defect #5. The old code discarded both the
// response and the error (`_, _ = ...ConfigSync(...)`) on a 2s timeout with
// nothing behind it, so a single dropped RPC was permanent divergence — which is
// how run 17 left node-3 holding precisely the last four adds it had missed,
// under serial mutation where there was no reordering to blame. #31 (a
// ConfigSync cycling the peer's gRPC listener) makes a refused RPC mid-switch a
// routine event rather than a rare one.
func (s *Server) broadcastConfigToPeersOnce() {
	const (
		maxAttempts = 4
		baseBackoff = 250 * time.Millisecond
	)

	s.RLock()
	stamp := s.loadConfigStamp()
	localID, _ := s.config.GetLocalNodeUUID()
	states := s.memberStatesForBroadcast()
	payloadBytes, err := buildFullConfigPayload(s.config, states, s.clusterEpoch, s.leaderID, localID, stamp)
	pending := make(map[string]*config.Node, len(s.config.Nodes))
	for id, node := range s.config.Nodes {
		if id != localID {
			pending[id] = node
		}
	}
	s.RUnlock()

	if err != nil {
		s.logger.Error("CONFIG_BROADCAST: failed to build payload", "error", err)
		return
	}

	// Peers that answered with a rejection no retry can fix. Tracked separately from
	// `pending` because the two need opposite handling: these must not be re-sent,
	// but they must also not be mistaken for peers that accepted the config.
	var permanentlyRejected []string

	for attempt := 1; attempt <= maxAttempts && len(pending) > 0; attempt++ {
		// A newer broadcast is already queued; it carries everything this one
		// would have delivered, so stop rather than compete with it.
		if attempt > 1 && len(s.broadcastTrigger) > 0 {
			s.logger.Debug("CONFIG_BROADCAST: superseded by a newer broadcast, abandoning retries",
				"version", stamp.version, "peersOutstanding", len(pending))
			return
		}

		if attempt > 1 {
			select {
			case <-s.broadcastStop:
				return
			case <-time.After(baseBackoff * time.Duration(1<<uint(attempt-2))):
			}
		}

		for id, node := range pending {
			remoteClient, err := s.getPeerClient(id, node)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), configSyncTimeoutFor(len(payloadBytes)))
			resp, err := remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: payloadBytes})
			cancel()
			switch {
			case err != nil:
				s.logger.Debug("CONFIG_BROADCAST: ConfigSync failed, will retry",
					"peer", id, "attempt", attempt, "version", stamp.version, "error", err)
			case resp != nil && resp.Message == supersededConfigMessage:
				// This node's config is older than the cluster's, so the change it
				// just made will not propagate — the next sync will overwrite it.
				// Warn, because the mutation reported success to the operator and
				// is about to be undone: defect #38's signature seen from the
				// sending end. Reachable when a node is mutated between its restart
				// and its first full ConfigSync, while configVersion still reads 0.
				//
				// Only the peer-is-ahead case reaches here. A peer that holds this
				// exact version answers configAlreadyHeldMessage instead and falls
				// into the default arm below as the plain accept it is.
				s.logger.Warn("CONFIG_BROADCAST: peer holds a newer config; this node's "+
					"change will not propagate and will be reverted by the next sync",
					"peer", id, "version", stamp.version)
				delete(pending, id)
			case resp != nil && !resp.Success && isPermanentRejection(resp.Message):
				// The payload itself is unusable to this peer, so re-sending the same
				// bytes cannot change the answer. Dropped from the retry set, but
				// recorded and logged loudly: the peer is diverged and nothing here
				// will repair it, which is worse than a peer that is merely behind.
				s.logger.Error("CONFIG_BROADCAST: peer permanently rejected the config; "+
					"retrying cannot fix this and the peer is now diverged",
					"peer", id, "version", stamp.version, "message", resp.Message)
				permanentlyRejected = append(permanentlyRejected, id)
				delete(pending, id)
			case resp != nil && !resp.Success:
				// A rejection on the peer's own terms that is *not* marked permanent:
				// a save failure, or any rejection from a binary predating the marker.
				// Both are transient as far as this node can tell, so the peer stays in
				// `pending` — it gets the in-pass retries and, failing those, reaches
				// recordUnpropagated. Warn rather than Debug: this used to be the quiet
				// path that let a diverged peer sit behind a broadcast reporting
				// success.
				s.logger.Warn("CONFIG_BROADCAST: peer declined ConfigSync, will retry",
					"peer", id, "attempt", attempt, "version", stamp.version,
					"message", resp.Message)
			default:
				delete(pending, id)
			}
		}
	}

	// A permanent rejection is not propagation, so it is recorded like any other
	// peer this config did not reach. Retrying it is futile, and the retry state is
	// what makes the divergence visible at all — clearing it here would report full
	// propagation to a peer that rejected the config outright, which is the exact lie
	// this branch removed everywhere else.
	unreached := make([]string, 0, len(pending)+len(permanentlyRejected))
	for id := range pending {
		unreached = append(unreached, id)
	}
	unreached = append(unreached, permanentlyRejected...)
	sort.Strings(unreached)

	if len(unreached) > 0 {
		// Warn, not Debug: this is the state that diverges a config. It used to say
		// "waiting for the periodic reconcile", which was only true on the
		// coordinator — see startConfigBroadcaster and defect #43.
		s.logger.Warn("CONFIG_BROADCAST: peers did not accept the config after all retries; "+
			"this node will keep retrying",
			"version", stamp.version, "peers", unreached,
			"permanentlyRejected", permanentlyRejected,
			"retryIn", s.recordUnpropagated(stamp, unreached))
		return
	}
	if attempts, wasOutstanding := s.clearUnpropagated(); wasOutstanding {
		// Info: this is the repair landing, and the whole point of #43 is that it
		// previously never did on a non-coordinator.
		s.logger.Info("CONFIG_PROPAGATION: every peer accepted the config",
			"version", stamp.version, "afterRetries", attempts)
	}
}

// memberStatesForBroadcast reads the current member statuses to send alongside
// the config, so a peer never has to reconcile a config against states that
// arrived in a different message (defect #27).
func (s *Server) memberStatesForBroadcast() map[string]membership.MemberStatus {
	states := map[string]membership.MemberStatus{}
	if s.memberList == nil {
		return states
	}
	for id, m := range s.memberList.MembersSnapshot() {
		if m == nil {
			continue
		}
		states[id] = m.GetStatus()
	}
	return states
}

// currentConfig returns the live config pointer, read under the read lock.
//
// ConfigSync spawns `go s.Reconfigure()`, which swaps the pointer under the write
// lock, so a second ConfigSync arriving while that goroutine is still running
// races every bare `s.config` read — and two ConfigSyncs landing close together
// is ordinary behaviour on a four-node cluster, not an edge case. The race
// detector catches it via the tests in config_generation_test.go.
//
// This closes the reads in ConfigSync only. The wider residual noted against
// defect #32 stands: ~278 unsynchronized s.config reads remain across
// internal/server, and closing those properly means this accessor (or an
// atomic.Pointer) applied at every one of them.
func (s *Server) currentConfig() *config.Config {
	s.RLock()
	defer s.RUnlock()
	return s.config
}

// permanentRejectionPrefix marks a ConfigSync rejection that re-sending the same
// bytes cannot fix: the payload itself is unusable, as opposed to the receiver
// having been briefly unable to store a payload it understood perfectly well.
//
// Both used to come back as a bare Success:false, and the sender dropped the peer
// from its retry set for either — which is right for the first and wrong for the
// second, since a save failure is ENOSPC, EIO or a read-only mount and clears on
// its own. The peer left the retry set before recordUnpropagated could see it, so
// the broadcast went on to report full propagation while the peer held none of the
// config. That is defect #43's signature reached by a different route.
//
// A marker on the message rather than a new proto field, deliberately: the wire
// contract here is two fields wide and shared with binaries that predate this
// branch, and an older peer simply omits the marker. That reads as transient, which
// is the safe direction — four retries and a warning, rather than a peer silently
// dropped. isPermanentRejection is prefix-anchored so the classification cannot be
// changed by editing an error string.
const permanentRejectionPrefix = "permanent: "

func isPermanentRejection(message string) bool {
	return strings.HasPrefix(message, permanentRejectionPrefix)
}

// supersededConfigMessage is the reply to a config payload older than the one
// this node holds. Success is true — the sender has nothing to fix and no reason
// to retry — but the sender reads the message to notice that it is the one
// behind, so the string is part of the wire contract, not just a log line.
const supersededConfigMessage = "superseded config version ignored"

// configAlreadyHeldMessage is the reply to a payload the receiver holds already —
// same version, same origin — as opposed to one it has moved past.
//
// Both are "not applied", and both used to come back as supersededConfigMessage,
// but they mean opposite things to the sender. Superseded means the sender is
// behind and its mutation is about to be undone, which is worth a Warn. This one
// means the sender's change arrived and is held at exactly the version it claims,
// which is a plain accept. SetMode makes the difference routine rather than
// theoretical: since #68 it puts the same stamp on the wire twice — the direct
// broadcastConfigAndStates push, and the markConfigDirty broadcast 250ms behind
// it — so every mode switch produced one false "will be reverted" Warn per peer.
//
// The sender needs no new case for this: Success is true and the message is not
// supersededConfigMessage, so it lands in broadcastConfigToPeersOnce's default
// arm and counts as delivered. That is deliberate, and it is what makes the new
// message safe against a mixed-version cluster in the sending direction too.
const configAlreadyHeldMessage = "config version already held"

// configNotAppliedMessage names which of the two "not applied" replies fits: the
// receiver is the only party that can tell an equal config from a stale one, so
// the distinction is made here rather than guessed at by the sender.
//
// Exact equality is version *and* origin. An equal version from a different
// origin is the concurrent-mutation case, where one of the two really is lost and
// the sender does need to hear about it.
func configNotAppliedMessage(incoming, held configStamp) string {
	if incoming == held {
		return configAlreadyHeldMessage
	}
	return supersededConfigMessage
}

// configStamp identifies a config's content: the Lamport clock value, and the ID
// of the node whose mutation produced it.
//
// The origin has to travel with the version because breaking a tie between two
// equal versions is only convergent if every node compares the same two things.
// The tiebreak used to be sender-versus-receiver, which is not that: if nodes A
// and B concurrently mint version N+1, a third node C whose UUID sorts above both
// rejects each late arrival and keeps whichever it applied first, while A and B
// each accept the other's and settle on max(A,B). The periodic reconcile re-runs
// the same receiver-relative comparison, so it cannot separate them either and
// the split is *stable* — the permanent divergence this clock exists to prevent.
// Comparing origin against origin gives every node the same answer,
// max(originA, originB), so all three converge.
type configStamp struct {
	version int64
	origin  string
}

// isEmpty reports a stamp that carries no ordering information, either because
// nothing has been applied yet or because the payload came from a binary that
// does not version its configs.
func (c configStamp) isEmpty() bool { return c.origin == "" || c.version <= 0 }

// loadConfigStamp reads the stamp for the config this node currently holds. The
// zero stamp — a node that has applied nothing since it started — orders below
// everything.
func (s *Server) loadConfigStamp() configStamp {
	if held := s.configStamp.Load(); held != nil {
		return *held
	}
	return configStamp{}
}

// shouldApplyIncomingConfig decides whether a full config payload carries newer
// content than what this node already holds.
//
// Strictly greater wins. A peer re-sending the version this node already holds
// has nothing to add — the periodic reconcile repairs a node that is *behind*,
// and that node's version is lower, so it still heals.
//
// Equal versions from two different origins are the concurrent-mutation case, and
// they have to be broken deterministically or each side rejects the other and the
// reconcile, also equal, cannot separate them. The origin node ID is the tiebreak
// because it is the one input every node agrees on. The losing mutation is lost;
// that is the standing limitation of applying a config wholesale, and convergence
// is worth more than a permanent split.
func (s *Server) shouldApplyIncomingConfig(incoming configStamp) bool {
	return configIsNewer(incoming, s.loadConfigStamp())
}

// configIsNewer is the comparison itself, taking both stamps as arguments so
// ConfigSync can re-run it under s.Lock() without re-entering the read lock.
//
// An empty held stamp loses every tie, which is the same "cannot order this,
// apply it" default as an unversioned payload.
func configIsNewer(incoming, held configStamp) bool {
	if incoming.isEmpty() {
		return true // unversioned: cannot be ordered, so apply it
	}
	if held.isEmpty() {
		return true
	}
	if incoming.version != held.version {
		return incoming.version > held.version
	}
	return incoming.origin > held.origin
}

// adoptConfigStamp raises this node's stamp to the one it just applied, which is
// what keeps the clock shared: a node that only ever receives configs still
// speaks the cluster's current version rather than its own zero.
//
// Never lowers it. The tiebreak above can apply a config at the version already
// held, and a concurrent local mutation may have moved past it. At equal versions
// the origin still moves, so the stamp continues to describe the content actually
// held rather than the tie it won.
func (s *Server) adoptConfigStamp(incoming configStamp) {
	if incoming.isEmpty() {
		return
	}
	next := &configStamp{version: incoming.version, origin: incoming.origin}
	for {
		heldPtr := s.configStamp.Load()
		var held configStamp
		if heldPtr != nil {
			held = *heldPtr
		}
		if !configIsNewer(incoming, held) {
			return
		}
		if s.configStamp.CompareAndSwap(heldPtr, next) {
			return
		}
	}
}

// broadcastConfigAndStates pushes the config and the member states it implies to
// every peer in one ConfigSync.
//
// Splitting the two is what broke the return to active-passive. SetMode used to
// send the config on its own and the states afterwards, both only once its IP
// work had finished, which left every peer holding two contradictory beliefs for
// the length of the switch: the cluster is active-passive, and I am still Active.
// On whitecrane at 19:52 on 2026-07-27 that window was over thirty seconds, and
// a peer spent it still acting as active-active coordinator — redistributing 150
// addresses it considered orphaned, then consolidating the group onto a target of
// its own choosing while the node handling the request consolidated onto another.
// Two whole-group consolidations onto two different nodes is how the entire group
// ended up on two nodes at once (docs/TEST-PLAN.md defect #27).
//
// Peers must therefore learn the mode and their new status together, and before
// any address moves.
//
// The payload is stamped, like every other broadcast on this path. It used to go
// through an unstamped wrapper, so ConfigSync's ordering guard read it as
// unversioned and every peer applied it unconditionally — including one holding
// strictly newer content, which reopened the #5/#38 window for the length of a
// switch. SetMode's caller has already run markConfigDirty(), so the clock the
// receiver needs is minted before this is reached; reading it here under the same
// lock as the config keeps the two describing each other.
func (s *Server) broadcastConfigAndStates(states map[string]membership.MemberStatus,
	epoch int64, leaderID string) {

	s.Lock()
	localID, _ := s.config.GetLocalNodeUUID()
	payloadBytes, buildErr := buildFullConfigPayload(
		s.config, states, epoch, leaderID, localID, s.loadConfigStamp())
	peers := make(map[string]*config.Node, len(s.config.Nodes))
	for id, node := range s.config.Nodes {
		if id != localID {
			peers[id] = node
		}
	}
	s.Unlock()

	if buildErr != nil {
		s.logger.Error("Failed to build the combined config and state payload", "error", buildErr)
		return
	}

	for id, node := range peers {
		remoteClient, err := s.getPeerClient(id, node)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), configSyncTimeoutFor(len(payloadBytes)))
		_, _ = remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: payloadBytes})
		cancel()
	}
}
