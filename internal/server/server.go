package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/packages/utils"
	rpc "github.com/syleron/pulseha/rpc"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
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
	sync.RWMutex
	config        *config.Config
	logger        *log.Logger
	memberList    *membership.MemberList
	healthCheck   *membership.HealthChecker
	ipMonitor     *membership.IPMonitor
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
	clientMutex sync.RWMutex
	// Unix socket path used by the CLI gRPC server
	cliSocketPath string
	// reconfigureMu serializes Reconfigure() so concurrent callers (such as
	// ConfigSync-triggered reconfigures) don't race on the cluster listener
	// bind to IP:port.
	reconfigureMu sync.Mutex
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

	// Load initial members from config
	s.logger.Debug("Loading initial members from configuration...")
	if s.memberList == nil {
		return fmt.Errorf("member list is nil")
	}
	if err := s.loadInitialMembers(); err != nil {
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

	// Start the health checker
	s.startHealthChecker()

	// Start the IP monitor - re-enabled with clean architectural separation from health checker
	if err := s.ipMonitor.Start(); err != nil {
		s.logger.Error("Failed to start IP monitor", "error", err)
		// Continue anyway, as this is not critical
	}

	// Only start health checker if we have a configured cluster
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
		}
	}

	// Capture the gRPC server pointer locally so a concurrent Reconfigure()
	// swapping s.grpcServer doesn't race with this goroutine's read.
	grpcSrv := s.grpcServer
	go func() {
		s.logger.Debug("Serving cluster gRPC", "addr", listener.Addr().String())
		if err := grpcSrv.Serve(listener); err != nil {
			s.logger.Error("Cluster gRPC server failed", "error", err)
		}
	}()

	return nil
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
}

// Stop gracefully shuts down the server
func (s *Server) Stop() {
	s.logger.Info("Stopping PulseHA server")

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
				states[id] = m.Status
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
func (s *Server) loadInitialMembers() error {
	s.logger.Info("Beginning initial member loading process...")

	if s.config == nil {
		s.logger.Error("FATAL: Cannot load members - config is nil!")
		return fmt.Errorf("config is nil")
	}
	s.logger.Debug("Config validation passed")

	if s.config.Nodes == nil {
		s.logger.Info("No nodes section found in config, starting with empty member list")
		return nil
	}
	s.logger.Debug("Nodes section found in config")

	// Deduplicate nodes by hostname and IP:Port to avoid duplicate members lingering after joins
	{
		localID, _ := s.config.GetLocalNodeUUID()
		seenByHost := make(map[string]string)
		seenByEP := make(map[string]string)
		var toDelete []string
		for id, n := range s.config.Nodes {
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
				delete(s.config.Nodes, id)
				_ = s.memberList.RemoveMember(id)
			}
			_ = s.config.Save()
		}
	}

	nodeCount := len(s.config.Nodes)
	s.logger.Infof("Found %d node(s) in configuration", nodeCount)

	// Log the actual nodes found
	s.logger.Info("Nodes in configuration:")
	for id, node := range s.config.Nodes {
		s.logger.Infof("  - %s (IP: %s, Port: %s)", id, node.IP, node.Port)
	}

	for id, node := range s.config.Nodes {
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

			// Set member details from config
			member.IP = node.IP
			member.Port = node.Port
			member.Hostname = node.Hostname

			// Determine initial status based on node role and cluster state
			localNodeID, err := s.config.GetLocalNodeUUID()
			isLocalNode := err == nil && id == localNodeID

			// Default to Unknown for all nodes initially - health checks will determine actual status
			member.Status = membership.StatusUnknown

			// For local node, we can set initial status based on cluster mode
			if isLocalNode {
				// Restore maintenance state across restarts before applying any other default
				if node.Maintenance {
					member.Status = membership.StatusMaintenance
					member.ActiveIPs = nil
					s.logger.Infof("Restoring local node %s to Maintenance (persisted in config)", id)
				} else if s.config.Pulse.Mode == "active-passive" {
					// In active-passive mode, determine who should be active
					totalNodes := len(s.config.Nodes)

					if totalNodes == 1 {
						// Single node cluster - should be active
						member.Status = membership.StatusActive
						s.logger.Infof("Setting single node %s as Active", id)
					} else {
						// Multi-node cluster - start as passive, election will determine active
						member.Status = membership.StatusPassive
						// Clear ActiveIPs for passive nodes to prevent health check loops
						member.ActiveIPs = nil
						s.logger.Infof("Setting local node %s as Passive (election will determine active)", id)
					}
				} else {
					// Active-active mode - start as passive
					member.Status = membership.StatusPassive
					// Clear ActiveIPs for passive nodes to prevent health check loops
					member.ActiveIPs = nil
					s.logger.Infof("Setting local node %s as Passive in active-active mode", id)
				}
			} else {
				// Remote nodes start as Unknown until health checks establish connection
				member.Status = membership.StatusUnknown
				s.logger.Infof("Setting remote node %s as Unknown (will be determined via health checks)", id)
			}

			// No longer try to determine status based on node order
			// Let the health checks and election process handle this

			s.logger.Debugf("Set initial details for member %s: IP=%s, Port=%s, Hostname=%s, Status=%s",
				id, member.IP, member.Port, member.Hostname, membership.StatusToString(member.Status))
		} else {
			s.logger.Warnf("Member %s was not found in member list after adding!", id)
		}
	}

	s.logger.Info("All members loaded successfully from configuration")
	s.logger.Debugf("Final member list contains %d members", s.memberList.GetMemberCount())

	// After members are loaded, perform one-shot VIP reconcile on local node
	go func() {
		// small delay to ensure listeners up
		time.Sleep(500 * time.Millisecond)
		if localID, err := s.config.GetLocalNodeUUID(); err == nil {
			member := s.memberList.GetMemberByID(localID)
			node := s.config.Nodes[localID]
			if member != nil && node != nil {
				if member.Status == membership.StatusActive {
					// Bring up any missing expected VIPs
					for iface, groups := range node.IPGroups {
						var ips []string
						for _, g := range groups {
							if gips, ok := s.config.Groups[g]; ok {
								ips = append(ips, gips...)
							}
						}
						if len(ips) > 0 {
							_, _ = s.BringUpIP(context.Background(), &rpc.UpIpRequest{Iface: iface, Ips: ips})
						}
					}
				} else {
					// Passive: drop any VIPs found on local interfaces
					for iface, groups := range node.IPGroups {
						var ips []string
						for _, g := range groups {
							if gips, ok := s.config.Groups[g]; ok {
								ips = append(ips, gips...)
							}
						}
						if len(ips) > 0 {
							_, _ = s.BringDownIP(context.Background(), &rpc.DownIpRequest{Iface: iface, Ips: ips})
						}
					}
				}
			}
		}
	}()

	return nil
}

// HandleNodeJoin processes a new node joining the cluster
func (s *Server) HandleNodeJoin(ctx context.Context, req *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	s.logger.Infof("Handling join request from node: %s", req.Hostname)
	s.logger.Debugf("Join request details - NodeID: %s, BindIP: %s, BindPort: %s, Token provided: %v",
		req.NodeId, req.BindIp, req.BindPort, req.Token != "")

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
		go s.broadcastFullConfigToPeers()
	}

	// Post-join: ensure health checker is running and member list is initialized promptly (async)
	go func() {
		s.startHealthChecker()
		if err := s.loadInitialMembers(); err != nil {
			s.logger.Warn("Join receiver failed to load members immediately", "error", err)
		}
	}()

	// Trigger a quick convergence broadcast asynchronously
	go func() {
		states := getStatusMap()
		membersSnapshot := s.memberList.MembersSnapshot()
		for id, m := range membersSnapshot {
			states[id] = m.Status
		}
		_ = s.BroadcastClusterState(states, s.GetClusterEpoch()+1, s.leaderID, nil)
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

// GetClusterStatus returns the current status of all nodes
func (s *Server) GetClusterStatus(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	s.RLock()
	defer s.RUnlock()

	var members []*rpc.Member
	membersSnapshot := s.memberList.MembersSnapshot()
	for _, member := range membersSnapshot {
		health := member.GetHealthStatus()
		var st rpc.MemberStatusEnum
		switch health.Status {
		case membership.StatusActive:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE
		case membership.StatusPassive:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE
		case membership.StatusMaintenance:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE
		default:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN
		}

		// Stamp a fresh last response for local display if empty/stale
		lastResp := ""
		if !health.LastResponse.IsZero() {
			lastResp = time.Now().Format(time.RFC3339)
		}

		members = append(members, &rpc.Member{
			Hostname:      health.Hostname,
			Status:        st,
			ActiveIps:    health.ActiveIPs,
			LastResponse: lastResp,
			Latency:      health.Latency,
			Ip:           member.IP,
			Port:          member.Port,
			NodeId:        member.ID,
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

// PromoteNode promotes a node to active status
func (s *Server) PromoteNode(ctx context.Context, req *rpc.PromoteRequest) (*rpc.PromoteResponse, error) {
	s.Lock()
	defer s.Unlock()

	s.logger.Infof("Handling promote request for node ID: %s", req.NodeId)

	// Get node ID for the request
	var nodeID string

	if req.NodeId != "" {
		nodeID = req.NodeId
		s.logger.Debugf("Using provided node_id for promote: %s", nodeID)
	} else {
		// No node_id provided
		return &rpc.PromoteResponse{
			Success: false,
			Message: "missing node_id",
		}, nil
	}

	// Get the member
	member := s.memberList.GetMemberByID(nodeID)
	if member == nil {
		s.logger.Error("PROMOTE: Member not found in member list",
			"requested_node_id", nodeID,
			"available_members", len(s.memberList.MembersSnapshot()))
		return &rpc.PromoteResponse{
			Success: false,
			Message: fmt.Sprintf("node not found with ID %s", nodeID),
		}, nil
	}

	// Check if member is local for routing decision
	isLocal := member.IsLocal()
	localNodeID, _ := s.config.GetLocalNodeUUID()

	s.logger.Info("PROMOTE: Determining promotion route",
		"requested_node_id", nodeID,
		"member_hostname", member.Hostname,
		"is_local", isLocal,
		"config_local_node_id", localNodeID)

	// If the member is local, promote locally; otherwise, connect to the target node and ask it to promote itself
	if isLocal {
		s.logger.Info("PROMOTE: Performing local promotion",
			"node_id", nodeID,
			"hostname", member.Hostname,
			"current_status", membership.StatusToString(member.Status))

		if err := member.MakeActive(req.Ips); err != nil {
			s.logger.Error("PROMOTE: Failed to promote local member", "error", err)
			return &rpc.PromoteResponse{Success: false, Message: "Failed to promote member: " + err.Error()}, nil
		}

		s.logger.Info("PROMOTE: Local promotion successful", "node_id", nodeID)
	} else {
		node := s.config.Nodes[req.NodeId]
		if node == nil {
			s.logger.Error("PROMOTE: Node configuration not found for remote promotion",
				"node_id", req.NodeId)
			return &rpc.PromoteResponse{Success: false, Message: "Node configuration not found"}, nil
		}

		s.logger.Info("PROMOTE: Performing remote promotion",
			"target_node_id", nodeID,
			"target_hostname", member.Hostname,
			"target_ip", node.IP,
			"target_port", node.Port,
			"local_node_id", localNodeID)

		remoteClient, err := client.New()
		if err != nil {
			s.logger.Error("PROMOTE: Failed to create client for remote promotion", "error", err)
			return &rpc.PromoteResponse{Success: false, Message: "Failed to create client: " + err.Error()}, nil
		}
		defer remoteClient.Close()

		s.logger.Debug("PROMOTE: Connecting to remote node", "ip", node.IP, "port", node.Port)
		if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
			s.logger.Error("PROMOTE: Failed to connect to target node", "error", err, "ip", node.IP, "port", node.Port)
			return &rpc.PromoteResponse{Success: false, Message: "Failed to connect to target node: " + err.Error()}, nil
		}

		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		s.logger.Debug("PROMOTE: Sending remote promotion request", "timeout", "5s")
		rresp, rerr := remoteClient.CLI().Promote(ctx2, &rpc.PromoteRequest{NodeId: req.NodeId, Ips: req.Ips, ForceDemote: req.ForceDemote})
		if rerr != nil {
			s.logger.Error("PROMOTE: Remote promote RPC failed", "error", rerr)
			return &rpc.PromoteResponse{Success: false, Message: "Remote promote failed: " + rerr.Error()}, nil
		}
		if !rresp.Success {
			s.logger.Warn("PROMOTE: Remote promote returned failure", "message", rresp.Message)
			return &rpc.PromoteResponse{Success: false, Message: rresp.Message}, nil
		}

		s.logger.Info("PROMOTE: Remote promotion successful", "node_id", nodeID)
		// Reflect the status change locally
		member.Status = membership.StatusActive
	}

	// Capture state for broadcast before releasing lock
	states := make(map[string]membership.MemberStatus)
	for id, m := range s.memberList.MembersSnapshot() {
		states[id] = m.Status
	}
	epoch := s.GetClusterEpoch() + 1

	// Release lock before calling BroadcastClusterState to avoid deadlock
	// (BroadcastClusterState acquires the lock internally)
	s.Unlock()

	// Broadcast convergence state so peers adopt the same active (active-passive)
	_ = s.BroadcastClusterState(states, epoch, nodeID, nil)

	// REMOVED: Redundant refresh call - health checker already handles VIP reconciliation after promotion
	// The BroadcastClusterState above will trigger health checker updates that handle VIP assignments
	// go s.refreshLocalMonitorExpectedIPs()

	// Success
	return &rpc.PromoteResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully promoted node %s to active", req.NodeId),
	}, nil
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

		// Wipe local cluster configuration (nodes and groups) and clear local identifiers
		s.config.Nodes = make(map[string]*config.Node)
		s.config.Groups = make(map[string][]string)
		s.config.Pulse.LocalNode = ""
		s.config.Pulse.ClusterToken = ""
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
	if member.Status == membership.StatusActive {
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
	if s.config.Pulse.Mode == "active-passive" {
		for id, m := range s.memberList.MembersSnapshot() {
			if m.Status == membership.StatusActive {
				prevActiveID = id
				s.logger.Info("PROMOTE_ASYNC: Found current active node", "active_node", id, "hostname", m.Hostname)
				break
			}
		}
	}
	if prevActiveID == "" {
		s.logger.Info("PROMOTE_ASYNC: No active node found in cluster")
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
		originalStates[id] = m.Status
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
				for id, st := range originalStates {
					if mm := s.memberList.GetMemberByID(id); mm != nil {
						mm.Status = st
					}
				}
				_ = s.BroadcastClusterState(originalStates, s.GetClusterEpoch()+1, s.leaderID, nil)
				return
			}

			demotionFailed = true
			s.logger.Warn("PROMOTE_ASYNC: Continuing with promotion despite demotion failure (force_demote=true)",
				"previous_active", prevActiveID)

			if failingMember := s.memberList.GetMemberByID(prevActiveID); failingMember != nil {
				failingMember.Status = membership.StatusUnknown
				failingMember.ActiveIPs = nil
			}
		} else {
			s.logger.Info("PROMOTE_ASYNC: Successfully demoted previous active", "previous_active", prevActiveID)
		}
	} else if shouldDemote && isTargetNodePromotion {
		s.logger.Info("PROMOTE_ASYNC: Skipping demotion (remote-initiated promotion)",
			"previous_active", prevActiveID,
			"new_active", targetNodeID)
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
			for id, st := range originalStates {
				if mm := s.memberList.GetMemberByID(id); mm != nil {
					mm.Status = st
				}
			}
			_ = s.BroadcastClusterState(originalStates, s.GetClusterEpoch()+1, s.leaderID, nil)
			return
		}
		s.logger.Info("PROMOTE_ASYNC: Successfully promoted local node to Active", "node_id", targetNodeID)
	} else {
		s.logger.Info("PROMOTE_ASYNC: Target is remote, forwarding promote request", "node_id", targetNodeID)
		node := s.config.Nodes[targetNodeID]
		if node == nil {
			s.logger.Error("PROMOTE_ASYNC: Node configuration not found", "node_id", targetNodeID)
			_ = s.BroadcastClusterState(originalStates, s.GetClusterEpoch()+1, s.leaderID, nil)
			return
		}

		remoteClient, err := client.New()
		if err != nil {
			s.logger.Error("PROMOTE_ASYNC: Failed to create client", "error", err)
			_ = s.BroadcastClusterState(originalStates, s.GetClusterEpoch()+1, s.leaderID, nil)
			return
		}
		defer remoteClient.Close()

		if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
			s.logger.Error("PROMOTE_ASYNC: Failed to connect to target node", "error", err)
			_ = s.BroadcastClusterState(originalStates, s.GetClusterEpoch()+1, s.leaderID, nil)
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
			for id, st := range originalStates {
				if mm := s.memberList.GetMemberByID(id); mm != nil {
					mm.Status = st
				}
			}
			_ = s.BroadcastClusterState(originalStates, s.GetClusterEpoch()+1, s.leaderID, nil)
			return
		}

		member.Status = membership.StatusActive
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
			states[id] = m.Status
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
		// Proactively bring down all floating IPs assigned to this node per config
		var ipsToDrop []string
		if localNodeCfg := s.config.Nodes[member.ID]; localNodeCfg != nil {
			for _, groups := range localNodeCfg.IPGroups {
				for _, g := range groups {
					if ipList, ok := s.config.Groups[g]; ok {
						ipsToDrop = append(ipsToDrop, ipList...)
					}
				}
			}
		}
		if len(ipsToDrop) > 0 {
			if err := member.BringDownIPs(ipsToDrop); err != nil {
				s.logger.Warn("Failed to bring down IPs during demotion", "error", err)
			}
		}
		member.Status = membership.StatusPassive
		member.ActiveIPs = nil
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
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rresp, rerr := remoteClient.Server().MakePassive(ctx2, &rpc.MakePassiveRequest{NodeId: req.NodeId})
		if rerr != nil {
			return &rpc.MakePassiveResponse{Success: false, Message: "Remote make passive failed: " + rerr.Error()}, nil
		}
		if !rresp.Success {
			return &rpc.MakePassiveResponse{Success: false, Message: rresp.Message}, nil
		}
		// Reflect locally
		member.Status = membership.StatusPassive
		member.ActiveIPs = nil
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

	// Update last response time
	member.LastHCResponse = time.Now()

	// Calculate and update latency
	latency := time.Since(member.LastHCResponse).String()
	member.Latency = latency
	s.logger.Debugf("Member %s latency: %s", member.Hostname, latency)

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

	// Reload config (uses config's own lock)
	s.logger.Debug("Reloading configuration...")
	if err := s.config.Reload(); err != nil {
		return fmt.Errorf("failed to reload config: %v", err)
	}

	// Get local node config (no server lock needed)
	s.logger.Debug("Getting updated local node configuration...")
	localNode, err := s.config.GetLocalNode()
	if err != nil {
		return fmt.Errorf("failed to get local node config: %v", err)
	}
	s.logger.Infof("Updated local node configuration: IP=%s, Port=%s", localNode.IP, localNode.Port)

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

	s.logger.Info("REFRESH: Processing node", "nodeID", localID, "status", membership.StatusToString(member.Status))

	if member.Status != membership.StatusActive {
		// For passive/unknown/maintenance nodes, trigger enforcement to clean up any floating IPs
		s.logger.Info("REFRESH: Node is not Active, cleanup needed", "status", membership.StatusToString(member.Status))
		if s.ipMonitor != nil {
			s.logger.Debug("REFRESH: Calling TriggerEnforce for passive node cleanup")
			s.ipMonitor.TriggerEnforce()
		} else {
			s.logger.Debug("REFRESH: IP monitor disabled, skipping cleanup TriggerEnforce")
			s.cleanupFloatingIPsDirectly(node)
		}
		return
	}

	// In active-active, only expect the IPs assigned to this node, not all group IPs.
	assignedIPs := make(map[string]bool)
	if s.config.Pulse.Mode == "active-active" {
		for _, ip := range member.ActiveIPs {
			assignedIPs[ip] = true
		}
	}

	s.logger.Info("REFRESH: Node is Active, setting up expected IPs", "status", membership.StatusToString(member.Status))
	for iface := range node.IPGroups {
		var ifaceIPs []string
		s.logger.Debug("REFRESH: Processing interface", "iface", iface, "groups", node.IPGroups[iface])
		for _, g := range node.IPGroups[iface] {
			if ips, ok := s.config.Groups[g]; ok {
				for _, ip := range ips {
					if len(assignedIPs) == 0 || assignedIPs[ip] {
						ifaceIPs = append(ifaceIPs, ip)
					}
				}
				s.logger.Debug("REFRESH: Added IPs from group", "group", g, "ips", ips)
			} else {
				s.logger.Warn("REFRESH: Group not found in config", "group", g)
			}
		}
		s.ipMonitor.ClearExpectedIPs(iface)
		if len(ifaceIPs) > 0 {
			s.logger.Info("REFRESH: Updating expected IPs for Active node", "iface", iface, "ips", ifaceIPs)
			s.ipMonitor.UpdateExpectedIPs(iface, ifaceIPs)
			// Proactively bring up any missing expected IPs on this interface
			var missing []string
			for _, ip := range ifaceIPs {
				ipOnly, _ := utils.GetCIDR(ip)
				if ipOnly == nil {
					s.logger.Debug("REFRESH: Skipping invalid IP", "ip", ip)
					continue
				}
				exists, existingIface, _ := network.CheckIfIPExists(ipOnly.String())
				s.logger.Debug("REFRESH: IP existence check for Active node", "ip", ipOnly.String(), "exists", exists, "existingIface", existingIface, "targetIface", iface)
				if !exists || existingIface != iface {
					missing = append(missing, ip)
					s.logger.Debug("REFRESH: IP is missing and will be brought up", "ip", ip)
				}
			}
			if len(missing) > 0 {
				s.logger.Info("REFRESH: Bringing up missing IPs on Active node", "iface", iface, "missingIPs", missing, "status", membership.StatusToString(member.Status))
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

	// Update mode in config
	s.config.Pulse.Mode = req.Mode

	// Save config
	if err := s.config.Save(); err != nil {
		return &rpc.SetModeResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	// If switching to active-active, redistribute IPs
	if req.Mode == "active-active" {
		s.logger.Info("Redistributing IPs for active-active mode")
		// Use config.Groups as the authoritative IP source.
		// member.ActiveIPs is nil when the active node was promoted via election
		// (elections set Status=StatusActive but never populate ActiveIPs).
		var allIPs []string
		for _, ips := range s.config.Groups {
			allIPs = append(allIPs, ips...)
		}
		// Clear current per-member assignments before redistributing
		for _, member := range s.memberList.MembersSnapshot() {
			member.ActiveIPs = nil
		}

		if err := s.memberList.RedistributeIPs(allIPs); err != nil {
			s.logger.Error("Failed to redistribute IPs", "error", err)
			// Continue anyway as the mode change is already saved
		}
	}

	// If switching to active-passive, move all IPs to the active node
	if req.Mode == "active-passive" {
		s.logger.Info("Moving all IPs to active node")
		var activeNode *membership.Member
		var allIPs []string

		// Find active node and collect all IPs
		for _, member := range s.memberList.MembersSnapshot() {
			if member.Status == membership.StatusActive && activeNode == nil {
				activeNode = member // take the first active node found
			}
			allIPs = append(allIPs, member.ActiveIPs...)
			member.ActiveIPs = nil // Clear current assignments
		}

		// Prefer the current leader when no explicit active node is found
		if activeNode == nil {
			if s.leaderID != "" {
				activeNode = s.memberList.GetMemberByID(s.leaderID)
			}
		}

		// Assign all IPs to active node
		if activeNode != nil {
			// Demote all non-selected nodes to Passive before promoting the winner.
			// Without this, nodes remain Active and are invisible to the
			// election's selectBestCandidate, which only scores Passive/Unknown.
			for _, member := range s.memberList.MembersSnapshot() {
				if member.ID != activeNode.ID {
					member.Status = membership.StatusPassive
				}
			}
			if err := activeNode.MakeActive(allIPs); err != nil {
				s.logger.Error("Failed to bring up IPs on active node during mode switch", "error", err)
			}
		} else {
			s.logger.Warn("No eligible node found to become active during mode switch")
		}
	}

	s.logger.Infof("Successfully changed cluster mode to: %s", req.Mode)

	// Bump epoch by 2 so our health-check broadcasts (epoch+1) supersede any stale
	// broadcasts from peers that still carry the pre-switch epoch.
	s.clusterEpoch += 2

	// Update local monitor expectations based on new role
	s.refreshLocalMonitorExpectedIPs()

	// Propagate the new config (including the mode change) to all peers.
	// The goroutine runs after s.Lock() is released via the deferred s.Unlock().
	go s.broadcastFullConfigToPeers()

	return &rpc.SetModeResponse{
		Success: true,
		Message: fmt.Sprintf("cluster mode changed to %s", req.Mode),
	}, nil
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
	go s.broadcastFullConfigToPeers()

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

	// Determine active-passive gating context
	activePassive := s.config.Pulse.Mode == "active-passive"
	activeID := ""
	if activePassive {
		for id, m := range s.memberList.MembersSnapshot() {
			if m.Status == membership.StatusActive {
				activeID = id
				break
			}
		}
		if activeID == "" {
			warnings = append(warnings, "No active node currently; IP will be enforced when a node becomes active")
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

	// Find nodes that have this group assigned and try to bring up the IP
	ipBroughtUp := false
	localBroughtUp := false
	for nodeID, node := range s.config.Nodes {
		for iface, groups := range node.IPGroups {
			for _, g := range groups {
				if g == req.GroupName {
					// In active-passive mode, only enforce on the current active node
					if activePassive && activeID != "" && nodeID != activeID {
						// Skip bringing IP up on passive nodes; config still records the IP
						continue
					}
					// Check if this is the local node
					if nodeID == s.config.Pulse.LocalNode {
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
									ipBroughtUp = true
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
						ipBroughtUp = true
						localBroughtUp = true
						s.logger.Infof("Successfully brought up IP %s on interface %s", ipToUse, iface)
					} else {
						// This is a remote node, send RPC to bring up the IP
						s.logger.Infof("Sending request to bring up IP %s on node %s", ipToUse, node.Hostname)
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

						// Send request to bring up IP
						resp, err := remoteClient.Server().BringUpIP(ctx, &rpc.UpIpRequest{
							Iface: iface,
							Ips:   []string{ipToUse},
						})

						if err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to bring up IP %s on node %s: %v", ipToUse, node.Hostname, err))
							continue
						}

						if !resp.Success {
							warnings = append(warnings, fmt.Sprintf("Failed to bring up IP %s on node %s: %s", ipToUse, node.Hostname, resp.Message))
							continue
						}

						ipBroughtUp = true
						s.logger.Infof("Successfully brought up IP %s on node %s", ipToUse, node.Hostname)
					}
				}
			}
		}
	}

	// If we couldn't bring up the IP immediately, decide whether to treat as fatal
	if !ipBroughtUp && len(warnings) > 0 {
		if activePassive {
			// In active-passive mode, lack of immediate bring-up may be expected (no active yet or gated)
			s.logger.Info("IP not brought up immediately due to active-passive gating or no active present", "ip", ipToUse)
		} else {
			return &rpc.AddIPToGroupResponse{
				Success:  false,
				Message:  "Failed to bring up IP on any node",
				Warnings: warnings,
			}, nil
		}
	}

	// Track the IP on the local member so status and the active-active
	// reconciler see it as hosted — otherwise it is treated as orphaned and
	// endlessly re-redistributed. Mirrors what the BringUpIP RPC handler does
	// for remote nodes, including the transition to Active in active-active.
	if localBroughtUp {
		if member := s.memberList.GetMemberByID(s.config.Pulse.LocalNode); member != nil {
			member.Lock()
			alreadyTracked := false
			for _, ip := range member.ActiveIPs {
				if ip == ipToUse {
					alreadyTracked = true
					break
				}
			}
			if !alreadyTracked {
				member.ActiveIPs = append(member.ActiveIPs, ipToUse)
			}
			if s.config.Pulse.Mode == "active-active" && member.Status == membership.StatusPassive {
				member.Status = membership.StatusActive
			}
			member.Unlock()
		}
	}

	// Add IP to group in config
	s.config.Groups[req.GroupName] = append(s.config.Groups[req.GroupName], ipToUse)

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.AddIPToGroupResponse{
			Success:  false,
			Message:  fmt.Sprintf("failed to save config: %v", err),
			Warnings: warnings,
		}, nil
	}
	// Broadcast updated config to peers
	go s.broadcastFullConfigToPeers()

	s.logger.Infof("Successfully added IP %s to group %s", ipToUse, req.GroupName)
	return &rpc.AddIPToGroupResponse{
		Success:  true,
		Message:  fmt.Sprintf("successfully added IP %s to group %s", ipToUse, req.GroupName),
		Warnings: warnings,
	}, nil
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
	go s.broadcastFullConfigToPeers()

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
	go s.broadcastFullConfigToPeers()

	// REMOVED: Redundant refresh call - health checker already handles VIP reconciliation after config changes
	// The broadcastFullConfigToPeers above will trigger config updates that activate health checker logic
	// go s.refreshLocalMonitorExpectedIPs()

	// If assigning on the local node, refresh expected IPs for this interface
	if s.ipMonitor != nil {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil && req.NodeId == localID {
			node := s.config.Nodes[localID]
			if node != nil {
				iface := req.Interface
				var ifaceIPs []string
				for _, g := range node.IPGroups[iface] {
					if ips, ok := s.config.Groups[g]; ok {
						ifaceIPs = append(ifaceIPs, ips...)
					}
				}
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
	go s.broadcastFullConfigToPeers()

	// REMOVED: Redundant refresh call - health checker already handles VIP reconciliation after config changes
	// The broadcastFullConfigToPeers above will trigger config updates that activate health checker logic
	// go s.refreshLocalMonitorExpectedIPs()

	// If unassigning on the local node, refresh expected IPs for this interface
	if s.ipMonitor != nil {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil && req.NodeId == localID {
			node := s.config.Nodes[localID]
			if node != nil {
				iface := req.Interface
				var ifaceIPs []string
				for _, g := range node.IPGroups[iface] {
					if ips, ok := s.config.Groups[g]; ok {
						ifaceIPs = append(ifaceIPs, ips...)
					}
				}
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

// DeleteGroup implements the CLI.DeleteGroup RPC method
func (s *Server) DeleteGroup(ctx context.Context, req *rpc.DeleteGroupRequest) (*rpc.DeleteGroupResponse, error) {
	s.logger.Infof("Received DeleteGroup request for group: %s (caller: %s)", req.GroupName, callerAddr(ctx))
	s.Lock()
	defer s.Unlock()

	if !s.config.ClusterCheck() {
		return &rpc.DeleteGroupResponse{Success: false, Message: "no cluster configured"}, nil
	}

	// Validate group exists (idempotent success if missing)
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		s.logger.Infof("Group %s does not exist; treating delete as success", req.GroupName)
		return &rpc.DeleteGroupResponse{Success: true, Message: fmt.Sprintf("group %s does not exist", req.GroupName)}, nil
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
		}, nil
	}

	// If force is true and group is assigned, remove assignments and add warnings
	var warnings []string
	if len(assignedNodes) > 0 && req.Force {
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
	}

	// Delete the group
	delete(s.config.Groups, req.GroupName)

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Error("Failed to save config", "error", err)
		return &rpc.DeleteGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}
	// Broadcast updated config to peers
	go s.broadcastFullConfigToPeers()

	s.logger.Infof("Successfully deleted group %s", req.GroupName)
	return &rpc.DeleteGroupResponse{
		Success:  true,
		Message:  fmt.Sprintf("successfully deleted group %s", req.GroupName),
		Warnings: warnings,
	}, nil
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

	// Check if cluster is already configured
	if s.config.ClusterCheck() {
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

	// Make it active
	member := s.memberList.GetMemberByID(nodeID)
	if member != nil {
		member.Status = membership.StatusActive
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
	address := fmt.Sprintf("%s:%s", utils.FormatIPv6(req.BindIp), finalPort)
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
			Message: "no configuration data provided",
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
		}
	}

	if isFullConfig {
		// Create a new config instance to hold incoming cluster-wide configuration
		newConfig := &config.Config{}

		// Unmarshal the received configuration
		if err := json.Unmarshal(req.Config, newConfig); err != nil {
			s.logger.Error("Failed to unmarshal configuration", "error", err)
			return &rpc.ConfigSyncResponse{
				Success: false,
				Message: fmt.Sprintf("failed to unmarshal configuration: %v", err),
			}, nil
		}

		// Preserve the local node identity from our existing configuration to avoid adopting remote LocalNode
		prevLocalID := s.config.Pulse.LocalNode
		if prevLocalID != "" && newConfig.Pulse.LocalNode != prevLocalID {
			s.logger.Debugf("ConfigSync: preserving local node identity: %s (incoming had %s)", prevLocalID, newConfig.Pulse.LocalNode)
			newConfig.Pulse.LocalNode = prevLocalID
			// Ensure the node entry exists in incoming config
			if newConfig.Nodes == nil {
				newConfig.Nodes = map[string]*config.Node{}
			}
			if _, ok := newConfig.Nodes[prevLocalID]; !ok {
				if existing := s.config.Nodes[prevLocalID]; existing != nil {
					// Shallow copy to avoid aliasing
					copied := *existing
					newConfig.Nodes[prevLocalID] = &copied
				}
			}
		}

		// Preserve local-specific settings before applying cluster config
		// These should not be overwritten by a remote ConfigSync
		localIDPreserve := s.config.Pulse.LocalNode
		loggingLevelPreserve := s.config.Pulse.LoggingLevel
		logToFilePreserve := s.config.Pulse.LogToFile
		logFileLocationPreserve := s.config.Pulse.LogFileLocation
		logToSyslogPreserve := s.config.Pulse.LogToSyslog
		syslogNetworkPreserve := s.config.Pulse.SyslogNetwork
		syslogAddressPreserve := s.config.Pulse.SyslogAddress
		syslogFacilityPreserve := s.config.Pulse.SyslogFacility
		syslogTagPreserve := s.config.Pulse.SyslogTag

		s.Lock()

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

		// Merge: preserve/merge groups and interface assignments when missing or empty in incoming
		if len(newConfig.Groups) == 0 && len(s.config.Groups) > 0 {
			// Deep copy local groups
			newConfig.Groups = make(map[string][]string, len(s.config.Groups))
			for g, ips := range s.config.Groups {
				copySlice := make([]string, len(ips))
				copy(copySlice, ips)
				newConfig.Groups[g] = copySlice
			}
		}
		// For any group present with empty list, prefer local non-empty list
		if len(s.config.Groups) > 0 {
			if newConfig.Groups == nil {
				newConfig.Groups = make(map[string][]string)
			}
			for g, localIPs := range s.config.Groups {
				if len(localIPs) == 0 {
					continue
				}
				incomingIPs, ok := newConfig.Groups[g]
				if !ok || len(incomingIPs) == 0 {
					copySlice := make([]string, len(localIPs))
					copy(copySlice, localIPs)
					newConfig.Groups[g] = copySlice
				}
			}
		}
		// Preserve per-node interface group assignments when missing in incoming
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
			if len(nIncoming.IPGroups) == 0 && len(existing.IPGroups) > 0 {
				nIncoming.IPGroups = make(map[string][]string, len(existing.IPGroups))
				for iface, groups := range existing.IPGroups {
					gg := make([]string, len(groups))
					copy(gg, groups)
					nIncoming.IPGroups[iface] = gg
				}
			}
			// For any interface present with empty group list, prefer local list
			for iface, localGroups := range existing.IPGroups {
				if len(localGroups) == 0 {
					continue
				}
				incomingGroups, ok := nIncoming.IPGroups[iface]
				if !ok || len(incomingGroups) == 0 {
					gg := make([]string, len(localGroups))
					copy(gg, localGroups)
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
		s.Unlock()

		// Update convergence metadata if newer
		if incomingEpoch > s.clusterEpoch {
			s.logger.Debug("CONFIG_SYNC: Updating cluster epoch",
				"oldEpoch", s.clusterEpoch,
				"newEpoch", incomingEpoch,
				"leaderID", incomingLeaderID)
			s.clusterEpoch = incomingEpoch
			s.leaderID = incomingLeaderID
		}

		// Immediately refresh member list from new configuration so peers become visible
		s.logger.Debug("CONFIG_SYNC: Updating member list with new config")
		s.memberList.UpdateConfig(s.config)

		s.logger.Debug("CONFIG_SYNC: Loading initial members")
		if err := s.loadInitialMembers(); err != nil {
			s.logger.Error("CONFIG_SYNC: Failed to load members after sync", "error", err)
		}
	} else {
		// Envelope-only update: do NOT overwrite config; just apply incoming states and metadata
		if incomingEpoch > s.clusterEpoch {
			s.clusterEpoch = incomingEpoch
			s.leaderID = incomingLeaderID
		}
		// Keep member list as-is for envelope updates to avoid clobbering runtime states
		// s.memberList.UpdateConfig(s.config)
		// Skip loadInitialMembers here; members are stable
	}

	// Apply incoming member states if provided
	if len(incomingMemberStates) > 0 {
		// Decide whether to apply incoming states based on epoch and leader to avoid cross-over
		applyStates := false
		currentEpoch := s.clusterEpoch
		currentLeader := s.leaderID
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
			// Update epoch/leader if needed
			if incomingEpoch >= currentEpoch {
				s.clusterEpoch = incomingEpoch
				s.leaderID = incomingLeaderID
				s.logger.Debug("CONFIG_SYNC: Updated cluster epoch and leader", "epoch", incomingEpoch, "leader", incomingLeaderID)
			}

			// Capture LOCAL node's status before applying incoming states
			localID, err := s.config.GetLocalNodeUUID()
			var oldLocalStatus membership.MemberStatus
			var hadLocalMember bool
			if err == nil {
				if localMember := s.memberList.GetMemberByID(localID); localMember != nil {
					oldLocalStatus = localMember.Status
					hadLocalMember = true
				}
			}

			s.logger.Debug("CONFIG_SYNC: Applying member states to local member list", "count", len(incomingMemberStates))
			syncLocalID, _ := s.config.GetLocalNodeUUID()
			for id, st := range incomingMemberStates {
				if m := s.memberList.GetMemberByID(id); m != nil {
					// Peers must not override the local node's maintenance state;
					// only the local daemon controls its own maintenance flag.
					if id == syncLocalID {
						if node := s.config.Nodes[id]; node != nil {
							if node.Maintenance {
								// Config says we're in maintenance — enforce it.
								if m.Status != membership.StatusMaintenance {
									m.Status = membership.StatusMaintenance
									s.logger.Debug("CONFIG_SYNC: Restored local maintenance status overridden by peer", "node_id", id)
								}
								continue
							}
							// Config says we're NOT in maintenance — reject stale StatusMaintenance from peers.
							if st == membership.StatusMaintenance {
								s.logger.Debug("CONFIG_SYNC: Rejected stale maintenance status from peer; local config shows not in maintenance", "node_id", id)
								continue
							}
						}
					}
					oldStatus := m.Status
					m.Status = st
					s.logger.Debug("CONFIG_SYNC: Updated member status", "node_id", id, "old_status", membership.StatusToString(oldStatus), "new_status", membership.StatusToString(st))
				} else {
					s.logger.Warn("CONFIG_SYNC: Member not found in member list", "node_id", id)
				}
			}

			// Check if LOCAL node transitioned to Active - if so, bring up VIPs
			// This handles the case where a node learns it's Active from ConfigSync (e.g., after restart during election)
			// Only triggers on state CHANGE, not continuous heartbeats, so no GARP storm risk
			if hadLocalMember && oldLocalStatus != membership.StatusActive {
				if newLocalMember := s.memberList.GetMemberByID(localID); newLocalMember != nil && newLocalMember.Status == membership.StatusActive {
					s.logger.Debug("ConfigSync: LOCAL node transitioned to Active, triggering VIP setup", "oldStatus", membership.StatusToString(oldLocalStatus), "newStatus", "Active")
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
	if s.config.Pulse.Mode == "active-active" && senderID != "" && senderActiveIPs != nil {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil && senderID != localID {
			if senderMember := s.memberList.GetMemberByID(senderID); senderMember != nil {
				senderMember.Lock()
				senderMember.ActiveIPs = append([]string{}, senderActiveIPs...)
				senderMember.Unlock()
			}
		}
	}

	// Only reconfigure when full config changed; skip for envelope-only state updates
	if isFullConfig {
		go func() {
			if err := s.Reconfigure(); err != nil {
				s.logger.Error("Async reconfigure failed after ConfigSync", "error", err)
			} else {
				s.logger.Debug("Async reconfigure completed after ConfigSync")
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

// UpdateConfig implements CLI.UpdateConfig
func (s *Server) UpdateConfig(ctx context.Context, req *rpc.UpdateConfigRequest) (*rpc.UpdateConfigResponse, error) {
	s.Lock()
	defer s.Unlock()

	if req == nil || req.Key == "" {
		return &rpc.UpdateConfigResponse{Success: false, Message: "invalid request"}, nil
	}

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

	return &rpc.UpdateConfigResponse{Success: true, Message: "updated"}, nil
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
			m.Lock()
			st := m.Status
			m.Unlock()
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
		member.Lock()
		if enable {
			member.Status = membership.StatusMaintenance
			member.ActiveIPs = nil
			member.LoadFactor = 0
		} else {
			member.Status = membership.StatusPassive
		}
		member.Unlock()
	}

	states := getStatusMap()
	for id, m := range s.memberList.MembersSnapshot() {
		states[id] = m.Status
	}
	_ = s.BroadcastClusterState(states, s.GetClusterEpoch()+1, s.leaderID, nil)
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
		member.Lock()
		currentStatus := member.Status
		member.Unlock()

		if currentStatus == membership.StatusMaintenance {
			return &rpc.SetMaintenanceResponse{Success: true, Message: "node is already in maintenance mode"}, nil
		}

		// Refuse if this would leave no available nodes
		availableAfter := 0
		for id, m := range s.memberList.MembersSnapshot() {
			if id == localID {
				continue
			}
			m.Lock()
			st := m.Status
			m.Unlock()
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
			states[id] = m.Status
		}
		_ = s.BroadcastClusterState(states, s.GetClusterEpoch()+1, s.leaderID, nil)
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
		states[id] = m.Status
	}
	_ = s.BroadcastClusterState(states, s.GetClusterEpoch()+1, s.leaderID, nil)
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
		member.Lock()
		member.Capacity = int(req.Capacity)
		member.Unlock()
	}

	// Broadcast updated config to peers
	go s.broadcastFullConfigToPeers()

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
						var ifaceIPs []string
						for _, g := range node.IPGroups[iface] {
							if ips, ok := s.config.Groups[g]; ok {
								ifaceIPs = append(ifaceIPs, ips...)
							}
						}
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

		if err := s.loadInitialMembers(); err != nil {
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
			address := fmt.Sprintf("%s:%s", utils.FormatIPv6(localNode.IP), localNode.Port)
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
				if presenceCount[id] < majority && member.Status == membership.StatusUnknown {
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

	for _, raw := range req.Ips {
		ip := raw
		// Normalize to CIDR
		if !utils.IsCIDR(ip) {
			if utils.IsIPv4(ip) {
				ip = ip + "/32"
			} else if utils.IsIPv6(ip) {
				ip = ip + "/128"
			} else {
				return &rpc.UpIpResponse{Success: false, Message: "invalid IP"}, nil
			}
		}

		// Inform monitor of expectation before manipulations
		s.ipMonitor.AddExpectedIPs(req.Iface, []string{ip})

		// Pre-check if already present
		ipOnly, ipNet := utils.GetCIDR(ip)
		s.logger.Warn("DEBUG: GetCIDR result", "inputIP", ip, "ipOnly", ipOnly, "ipNet", ipNet)
		if ipOnly != nil {
			ex, eIface, checkErr := network.CheckIfIPExists(ipOnly.String())
			s.logger.Warn("DEBUG: CheckIfIPExists for IP", "ip", ipOnly.String(), "exists", ex, "iface", eIface, "targetIface", req.Iface, "error", checkErr)
			if ex {
				if eIface == req.Iface {
					// Already present on desired interface: send GARP and continue
					s.logger.Info("IP already exists on target interface, skipping", "ip", ip, "iface", req.Iface)
					_ = network.SendGARP(req.Iface, ip)
					continue
				}
				// Present on a different interface: try to remove there first (best-effort)
				_ = network.BringIPdown(eIface, ip)
			}
		}
		if err := network.BringIPup(req.Iface, ip); err != nil {
			// If add failed, recheck if it is now present on target iface (treat as success)
			if ipOnly != nil {
				ex, eIface, _ := network.CheckIfIPExists(ipOnly.String())
				if ex && eIface == req.Iface {
					s.logger.Info("BringUpIP: IP assignment failed but IP is now present on target interface", "ip", ip, "iface", req.Iface)
					_ = network.SendGARP(req.Iface, ip)
					continue
				}
			}
			// Additional fallback check - this prevents the emergency loop
			s.logger.Warn("BringUpIP failed, doing final verification", "iface", req.Iface, "ip", ip, "error", err)
			if ipOnly != nil {
				ex, eIface, _ := network.CheckIfIPExists(ipOnly.String())
				if ex && eIface == req.Iface {
					s.logger.Info("BringUpIP: Final check confirms IP is present on target interface, treating as success")
					_ = network.SendGARP(req.Iface, ip)
					continue
				}
			}
			s.logger.Error("BringUpIP failed", "iface", req.Iface, "ip", ip, "error", err)
			return &rpc.UpIpResponse{Success: false, Message: err.Error()}, nil
		}

		// Best-effort GARP
		_ = network.SendGARP(req.Iface, ip)
	}

	// In active-active mode: if the local node is Passive/Unknown, this BringUpIP
	// call means the coordinator has assigned these IPs to us. Transition to
	// Active so the ENFORCE loop doesn't immediately strip the IPs back off.
	if s.config.Pulse.Mode == "active-active" {
		localID, err := s.config.GetLocalNodeUUID()
		if err == nil {
			localMember := s.memberList.GetMemberByID(localID)
			if localMember != nil {
				localMember.Lock()
				if localMember.Status == membership.StatusPassive || localMember.Status == membership.StatusUnknown {
					localMember.Status = membership.StatusActive
					s.logger.Info("BringUpIP: Transitioned local node to Active for active-active mode")
				}
				for _, ip := range req.Ips {
					found := false
					for _, existing := range localMember.ActiveIPs {
						if existing == ip {
							found = true
							break
						}
					}
					if !found {
						localMember.ActiveIPs = append(localMember.ActiveIPs, ip)
					}
				}
				localMember.Unlock()
				s.refreshLocalMonitorExpectedIPs()
			}
		}
	}

	return &rpc.UpIpResponse{Success: true, Message: "IPs brought up"}, nil
}

// BringDownIP implements the Server.BringDownIP RPC for remote IP removal
func (s *Server) BringDownIP(ctx context.Context, req *rpc.DownIpRequest) (*rpc.DownIpResponse, error) {
	s.logger.Infof("RPC BringDownIP on iface %s for %d IP(s)", req.Iface, len(req.Ips))

	var failed []string
	for _, ip := range req.Ips {
		if !utils.IsCIDR(ip) {
			if utils.IsIPv4(ip) {
				ip = ip + "/32"
			} else if utils.IsIPv6(ip) {
				ip = ip + "/128"
			} else {
				s.logger.Warn("BringDownIP skipping invalid IP", "ip", ip)
				continue
			}
		}

		s.ipMonitor.RemoveExpectedIPs(req.Iface, []string{ip})

		if err := network.BringIPdown(req.Iface, ip); err != nil {
			s.logger.Error("BringDownIP failed", "iface", req.Iface, "ip", ip, "error", err)
			failed = append(failed, ip)
			continue
		}
	}
	// In active-active mode keep the local member's ActiveIPs bookkeeping in
	// sync (mirror of BringUpIP) so a later monitor refresh doesn't resurrect
	// IPs that were deliberately moved to another node.
	if s.config.Pulse.Mode == "active-active" {
		if localID, err := s.config.GetLocalNodeUUID(); err == nil {
			if localMember := s.memberList.GetMemberByID(localID); localMember != nil {
				removed := make(map[string]bool, len(req.Ips))
				for _, ip := range req.Ips {
					removed[ip] = true
				}
				localMember.Lock()
				var remaining []string
				for _, ip := range localMember.ActiveIPs {
					if !removed[ip] {
						remaining = append(remaining, ip)
					}
				}
				localMember.ActiveIPs = remaining
				localMember.Unlock()
			}
		}
	}

	if len(failed) > 0 {
		return &rpc.DownIpResponse{Success: true, Message: "Best-effort: some IPs may not have been present"}, nil
	}
	return &rpc.DownIpResponse{Success: true, Message: "IPs brought down"}, nil
}

// InitiateJoin performs a server-driven join against a target member
func (s *Server) InitiateJoin(ctx context.Context, req *rpc.InitiateJoinRequest) (*rpc.InitiateJoinResponse, error) {
	if req == nil || req.TargetHost == "" {
		return &rpc.InitiateJoinResponse{Success: false, Message: "target_host is required"}, nil
	}

	// Prevent joining if this node is already part of a cluster
	if s.config != nil && s.config.ClusterCheck() {
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

	jResp, jErr := remoteClient.CLI().Join(context.Background(), joinReq)
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
		if err := s.loadInitialMembers(); err != nil {
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
		states[id] = m.Status
	}
	_ = s.BroadcastClusterState(states, s.GetClusterEpoch()+1, s.leaderID, nil)

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
			// Recompute expected IPs for this interface from authoritative config
			var ifaceIPs []string
			if localNode := s.config.Nodes[newNodeID]; localNode != nil {
				for _, g := range localNode.IPGroups[iface] {
					if grpIPs, ok := s.config.Groups[g]; ok {
						ifaceIPs = append(ifaceIPs, grpIPs...)
					}
				}
			}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	localID, _ := s.config.GetLocalNodeUUID()

	// In active-active, self-report this node's hosted IPs so peers keep an
	// accurate view of the assignment map. Without this, a peer that takes
	// over as redistribution coordinator wouldn't know which IPs this node
	// held when it failed.
	if s.config.Pulse.Mode == "active-active" && localID != "" {
		if localMember := s.memberList.GetMemberByID(localID); localMember != nil {
			localMember.Lock()
			// A non-active node cannot host floating IPs — the IP monitor
			// enforces that on the interface — so clear any stale claim
			// before self-reporting. Otherwise peers keep counting these
			// IPs as hosted and the reconciler never re-places them, and
			// status shows IPs on a node that doesn't actually hold them.
			if localMember.Status != membership.StatusActive && len(localMember.ActiveIPs) > 0 {
				localMember.ActiveIPs = nil
				localMember.LoadFactor = 0
			}
			payload["sender_id"] = localID
			payload["sender_active_ips"] = append([]string{}, localMember.ActiveIPs...)
			localMember.Unlock()
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

func (s *Server) broadcastFullConfigToPeers() {
	configBytes, err := json.Marshal(s.config)
	if err != nil {
		return
	}
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
