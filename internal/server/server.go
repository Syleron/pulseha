package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/packages/utils"
	rpc "github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// Server represents the PulseHA server
type Server struct {
	sync.RWMutex
	config        *config.Config
	logger        *logrus.Logger
	memberList    *membership.MemberList
	healthCheck   *membership.HealthChecker
	ipMonitor     *membership.IPMonitor
	quorumManager *quorum.QuorumManager
	quorumHandler *quorum.RPCHandler
	grpcServer    *grpc.Server
	cliServer     *grpc.Server
	rpc.UnimplementedCLIServer
	rpc.UnimplementedServerServer
}

// NewServer creates a new PulseHA server instance
func NewServer(cfg *config.Config, logger *logrus.Logger, memberList *membership.MemberList, healthCheck *membership.HealthChecker) *Server {
	// Create the quorum manager
	quorumMgr := quorum.NewQuorumManager(cfg, logger)

	// Create the quorum RPC handler
	quorumHandler := quorum.NewRPCHandler(quorumMgr, logger)

	// Create IP monitor
	ipMonitor := membership.NewIPMonitor(memberList, logger)

	// Set IP monitor reference in member list
	memberList.SetIPMonitor(ipMonitor)

	// Create server
	s := &Server{
		config:        cfg,
		logger:        logger,
		memberList:    memberList,
		healthCheck:   healthCheck,
		ipMonitor:     ipMonitor,
		quorumManager: quorumMgr,
		quorumHandler: quorumHandler,
	}

	// Set server reference in health checker
	healthCheck.SetServerReference(s)

	return s
}

// Start initializes and starts the server components
func (s *Server) Start() error {
	s.Lock()
	defer s.Unlock()

	// Set log level to debug temporarily during startup
	previousLevel := s.logger.GetLevel()
	s.logger.SetLevel(logrus.DebugLevel)
	defer s.logger.SetLevel(previousLevel)

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

	// Start CLI server on localhost unconditionally
	s.logger.Debug("Starting CLI gRPC server on 127.0.0.1:8080...")
	s.cliServer = grpc.NewServer()
	rpc.RegisterCLIServer(s.cliServer, s)
	cliListener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		return fmt.Errorf("failed to listen for CLI server on 127.0.0.1:8080: %v", err)
	}
	go func() {
		s.logger.Debug("Serving CLI gRPC on 127.0.0.1:8080")
		if err := s.cliServer.Serve(cliListener); err != nil {
			s.logger.WithError(err).Error("CLI server failed")
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
		s.logger.Info("No cluster binding configuration found; cluster RPC server not started. CLI is available on 127.0.0.1:8080 for bootstrap.")
	}

	// Generate certificates if they don't exist
	s.logger.Debug("Checking/Generating TLS certificates...")
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %v", err)
	}
	if err := security.GenerateCertificates(hostname); err != nil {
		s.logger.WithError(err).Warn("Failed to generate certificates, continuing without TLS")
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
		s.logger.Debug("Quorum manager started with node count: ", nodeCount)
	} else {
		s.logger.Warn("Quorum manager is nil, quorum voting will not be available")
	}

	// Quorum RPC handlers are registered via Server methods implementation

	// Register quorum RPC handlers if available
	if s.quorumHandler != nil {
		s.logger.Debug("Registering quorum RPC handlers...")
		// We need to delegate the quorum-related RPC methods to our quorum handler
		// This will be done by implementing the RPC methods in the Server struct
	} else {
		s.logger.Warn("Quorum handler is nil, quorum RPC methods will not be available")
	}

	// Start the health checker
	s.startHealthChecker()

	// Start the IP monitor
	if err := s.ipMonitor.Start(); err != nil {
		s.logger.Errorf("Failed to start IP monitor: %v", err)
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

// watchForClusterJoin periodically checks if this node has joined a cluster
// watchForClusterJoin removed; activation occurs via explicit resync.

// startClusterListener starts the gRPC server that handles inter-node RPC on the configured bind address
func (s *Server) startClusterListener(localNode config.Node) error {
	s.logger.Debugf("Starting cluster RPC server on %s:%s...", localNode.IP, localNode.Port)

	// Create gRPC server if needed
	if s.grpcServer == nil {
		s.grpcServer = grpc.NewServer()
		rpc.RegisterServerServer(s.grpcServer, s)
		// Also register CLI RPCs on the cluster listener to support remote operations like Join
		rpc.RegisterCLIServer(s.grpcServer, s)
	}

	address := fmt.Sprintf("%s:%s", localNode.IP, localNode.Port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", address, err)
	}

	go func() {
		s.logger.Debug("Serving cluster gRPC on ", address)
		if err := s.grpcServer.Serve(listener); err != nil {
			s.logger.WithError(err).Error("Cluster gRPC server failed")
		}
	}()

	return nil
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

	s.logger.Info("Initializing health checker with interval: ", interval)
	s.healthCheck.Start(interval)
	s.logger.Info("Health checker started successfully")
}

// Stop gracefully shuts down the server
func (s *Server) Stop() {
	s.Lock()
	defer s.Unlock()

	s.logger.Info("Stopping PulseHA server")

	// Stop the health checker
	if s.healthCheck != nil {
		s.healthCheck.Stop()
	}

	// Stop the IP monitor
	if s.ipMonitor != nil {
		s.ipMonitor.Stop()
	}

	// Stop the gRPC server
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}

	// Stop the CLI gRPC server
	if s.cliServer != nil {
		s.cliServer.GracefulStop()
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
			s.logger.Errorf("FATAL: Failed to add member %s: %v", id, err)
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
				if s.config.Pulse.Mode == "active-passive" {
					// In active-passive mode, determine who should be active
					totalNodes := len(s.config.Nodes)

					if totalNodes == 1 {
						// Single node cluster - should be active
						member.Status = membership.StatusActive
						s.logger.Infof("Setting single node %s as Active", id)
					} else {
						// Multi-node cluster - start as passive, election will determine active
						member.Status = membership.StatusPassive
						s.logger.Infof("Setting local node %s as Passive (election will determine active)", id)
					}
				} else {
					// Active-active mode - start as passive
					member.Status = membership.StatusPassive
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
	s.logger.Debugf("Final member list contains %d members", len(s.memberList.Members))
	return nil
}

// HandleNodeJoin processes a new node joining the cluster
func (s *Server) HandleNodeJoin(ctx context.Context, req *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	s.logger.Infof("Handling join request from node: %s", req.Address)
	s.logger.Debugf("Join request details - NodeID: %s, BindIP: %s, BindPort: %s, Token provided: %v",
		req.NodeId, req.BindIp, req.BindPort, req.Token != "")

	// Check if this is initial cluster creation
	if len(s.memberList.Members) == 0 && req.Token == "" {
		s.logger.Info("Initializing new cluster with first node: ", req.Address)

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
		if err := s.memberList.AddMember(nodeID, req.Address, req.BindIp, req.BindPort); err != nil {
			s.logger.WithError(err).Error("Failed to add member to member list")
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
			s.logger.WithError(err).Error("Failed to save config")
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
	s.logger.Debugf("Validating cluster token for join...")
	receivedToken := strings.TrimSpace(req.Token)
	receivedToken = strings.TrimPrefix(receivedToken, "Bearer ")
	receivedToken = strings.TrimPrefix(receivedToken, "bearer ")
	clusterToken := strings.TrimSpace(s.config.Pulse.ClusterToken) // Direct read - config token shouldn't change during join
	redact := func(t string) string {
		if len(t) <= 8 {
			return "***"
		}
		return "***" + t[len(t)-6:]
	}
	s.logger.Debugf("Token debug (redacted) - expected=%s received=%s", redact(clusterToken), redact(receivedToken))

	if !strings.EqualFold(receivedToken, clusterToken) {
		s.logger.Warning("Invalid cluster join token received")
		return &rpc.JoinResponse{
			Success: false,
			Message: "Invalid cluster token",
		}, nil
	}
	s.logger.Debugf("Token validation passed")

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
	if err := s.memberList.AddMember(nodeID, req.Address, req.BindIp, req.BindPort); err != nil {
		s.logger.WithError(err).Error("Failed to add member to member list")
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
	s.config.Nodes[nodeID] = &config.Node{
		Hostname: req.Address,
		IP:       req.BindIp,
		Port:     req.BindPort,
		IPGroups: make(map[string][]string),
	}
	s.logger.Debugf("Config updated, releasing config lock...")
	s.config.Unlock()
	s.logger.Debugf("Config lock released")

	s.logger.Infof("Successfully joined node %s (ID: %s) to cluster", req.Address, nodeID)

	// Save the config synchronously to ensure it's available for health checks
	s.logger.Debugf("Saving config with new member %s...", nodeID)
	if err := s.config.Save(); err != nil {
		s.logger.WithError(err).Error("Failed to save config after successful join")
		// Still return success since member was added to memberList
	} else {
		s.logger.Debugf("Config saved successfully after node %s joined", req.Address)
	}

	// Marshal the complete cluster configuration to send to the joining node
	// Create an enhanced config that includes member status information
	type EnhancedConfig struct {
		*config.Config
		MemberStates map[string]membership.MemberStatus `json:"member_states"`
	}

	// Collect current member states
	memberStates := make(map[string]membership.MemberStatus)
	for id, member := range s.memberList.Members {
		memberStates[id] = member.Status
		s.logger.Debugf("Adding member state for %s: %s", id, membership.StatusToString(member.Status))
	}

	enhancedConfig := EnhancedConfig{
		Config:       s.config,
		MemberStates: memberStates,
	}

	configBytes, err := json.Marshal(enhancedConfig)
	if err != nil {
		s.logger.WithError(err).Error("Failed to marshal config for joining node")
		// Still return success but without config - node can sync later
		return &rpc.JoinResponse{
			Success: true,
			NodeId:  nodeID,
			Message: "Successfully joined cluster (config sync pending)",
		}, nil
	}

	s.logger.Debugf("Sending cluster configuration with member states to joining node %s", req.Address)
	return &rpc.JoinResponse{
		Success:       true,
		NodeId:        nodeID,
		Message:       "Successfully joined cluster",
		ClusterConfig: configBytes,
	}, nil
}

// HandleNodeLeave handles the node leave RPC call
func (s *Server) HandleNodeLeave(ctx context.Context, req *rpc.LeaveRequest) (*rpc.LeaveResponse, error) {
	s.Lock()
	defer s.Unlock()

	// Get node ID for the request
	var nodeID string

	if req.NodeId != "" {
		nodeID = req.NodeId
		s.logger.Debugf("Using provided node_id for leave: %s", nodeID)
	} else {
		// Neither node_id nor hostname provided
		return &rpc.LeaveResponse{
			Success: false,
			Message: "missing node_id",
		}, nil
	}

	// Get the member
	member := s.memberList.GetMemberByID(nodeID)
	if member == nil {
		return &rpc.LeaveResponse{
			Success: false,
			Message: fmt.Sprintf("node not found with ID %s", nodeID),
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
	if nodeID == localNodeID {
		s.logger.Info("Leaving cluster as local node")
		go func() {
			time.Sleep(1 * time.Second)
			s.Stop()
		}()
		return &rpc.LeaveResponse{
			Success: true,
			Message: "Successfully left the cluster",
		}, nil
	}

	// Remove the node from our member list
	if err := s.memberList.RemoveMember(nodeID); err != nil {
		s.logger.Errorf("Failed to remove member: %v", err)
		return &rpc.LeaveResponse{
			Success: false,
			Message: "Failed to remove member: " + err.Error(),
		}, nil
	}

	// Update our config to remove the node
	delete(s.config.Nodes, nodeID)

	// Success
	return &rpc.LeaveResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully removed node %s from the cluster", nodeID),
	}, nil
}

// GetClusterStatus returns the current status of all nodes
func (s *Server) GetClusterStatus(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	s.RLock()
	defer s.RUnlock()

	var members []*rpc.Member
	for _, member := range s.memberList.Members {
		health := member.GetHealthStatus()
		var st rpc.MemberStatusEnum
		switch health.Status {
		case membership.StatusActive:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE
		case membership.StatusPassive:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE
		case membership.StatusPartialActive:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_PARTIAL_ACTIVE
		default:
			st = rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN
		}
		members = append(members, &rpc.Member{
			Hostname:      health.Hostname,
			Status:        st,
			ActiveIps:     health.ActiveIPs,
			LastResponse:  health.LastResponse.String(),
			Latency:       health.Latency,
			PartialActive: health.PartialActive,
		})
	}

	return &rpc.StatusResponse{
		Members: members,
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
		return &rpc.PromoteResponse{
			Success: false,
			Message: fmt.Sprintf("node not found with ID %s", nodeID),
		}, nil
	}

	// Promote the member
	if err := member.MakeActive(req.Ips); err != nil {
		s.logger.Errorf("Failed to promote node: %v", err)
		return &rpc.PromoteResponse{
			Success: false,
			Message: fmt.Sprintf("failed to promote node: %v", err),
		}, nil
	}

	return &rpc.PromoteResponse{
		Success: true,
		Message: fmt.Sprintf("successfully promoted node %s", nodeID),
	}, nil
}

// Join handles the CLI Join RPC call
func (s *Server) Join(ctx context.Context, req *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"address": req.Address,
		"token":   req.Token != "",
	}).Info("Received CLI Join request")

	resp, err := s.HandleNodeJoin(ctx, req)
	if err != nil {
		s.logger.WithError(err).Error("CLI Join request failed")
	} else {
		s.logger.WithFields(logrus.Fields{
			"success": resp.Success,
			"message": resp.Message,
		}).Info("CLI Join request completed")
	}
	return resp, err
}

// Leave handles the CLI Leave RPC call
func (s *Server) Leave(ctx context.Context, req *rpc.LeaveRequest) (*rpc.LeaveResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"node_id": req.NodeId,
	}).Info("Received CLI Leave request")

	if req.NodeId == "" {
		return &rpc.LeaveResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
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

		// Broadcast removal of this node to all other peers before stopping
		for id, node := range s.config.Nodes {
			if id == localNodeID {
				continue
			}
			remoteClient, err := client.New()
			if err != nil {
				s.logger.WithError(err).Warnf("Failed to create client for peer %s (%s:%s)", id, node.IP, node.Port)
				continue
			}
			if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
				remoteClient.Close()
				s.logger.WithError(err).Warnf("Failed to connect to peer %s (%s:%s)", id, node.IP, node.Port)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err = remoteClient.Server().Remove(ctx, &rpc.RemoveRequest{NodeId: localNodeID})
			cancel()
			remoteClient.Close()
			if err != nil {
				s.logger.WithError(err).Warnf("Failed to propagate removal to peer %s", id)
			}
		}

		// Stop the server asynchronously to allow RPC response to complete
		go func() {
			time.Sleep(1 * time.Second)
			s.Stop()
		}()
		return &rpc.LeaveResponse{
			Success: true,
			Message: "Successfully left the cluster",
		}, nil
	}

	// Remove the node from our member list
	if err := s.memberList.RemoveMember(member.ID); err != nil {
		s.logger.Errorf("Failed to remove member: %v", err)
		return &rpc.LeaveResponse{
			Success: false,
			Message: "Failed to remove member: " + err.Error(),
		}, nil
	}

	// Update our config to remove the node
	delete(s.config.Nodes, member.ID)

	// Success
	return &rpc.LeaveResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully removed node %s from the cluster", member.ID),
	}, nil
}

// Promote handles the CLI Promote RPC call
func (s *Server) Promote(ctx context.Context, req *rpc.PromoteRequest) (*rpc.PromoteResponse, error) {
	s.logger.Infof("Received promote request for node ID: %s", req.NodeId)

	if req.NodeId == "" {
		return &rpc.PromoteResponse{
			Success: false,
			Message: "node_id is required",
		}, nil
	}

	// Get the member
	member := s.memberList.GetMemberByID(req.NodeId)
	if member == nil {
		return &rpc.PromoteResponse{
			Success: false,
			Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
		}, nil
	}

	// Promote the member
	if err := member.MakeActive(req.Ips); err != nil {
		s.logger.Errorf("Failed to promote member: %v", err)
		return &rpc.PromoteResponse{
			Success: false,
			Message: "Failed to promote member: " + err.Error(),
		}, nil
	}

	// Success
	return &rpc.PromoteResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully promoted node %s to active", req.NodeId),
	}, nil
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

	// Make the member passive by setting its status
	member.Status = membership.StatusPassive
	member.ActiveIPs = nil
	member.PartialActive = false

	// Success
	return &rpc.MakePassiveResponse{
		Success: true,
		Message: fmt.Sprintf("Successfully made node %s passive", req.NodeId),
	}, nil
}

// HealthCheck handles the health check RPC call
func (s *Server) HealthCheck(ctx context.Context, req *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	// Get the member
	var member *membership.Member

	if req.NodeId != "" {
		member = s.memberList.GetMemberByID(req.NodeId)
		if member == nil {
			return &rpc.HealthCheckResponse{
				Success: false,
				Message: fmt.Sprintf("Node not found with ID: %s", req.NodeId),
			}, nil
		}
		s.logger.Debugf("Found node by ID: %s (%s)", req.NodeId, member.Hostname)
	}

	if member == nil {
		return &rpc.HealthCheckResponse{
			Success: false,
			Message: "No node identifier provided",
		}, nil
	}

	// Update last response time
	member.LastHCResponse = time.Now()

	// Calculate and update latency
	latency := time.Since(member.LastHCResponse).String()
	member.Latency = latency
	s.logger.Debugf("Member %s latency: %s", member.Hostname, latency)

	// Return healthy response
	return &rpc.HealthCheckResponse{
		Success: true,
		Message: fmt.Sprintf("Node %s is healthy", member.Hostname),
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
		s.logger.Errorf("Failed to remove member: %v", err)
		return &rpc.RemoveResponse{
			Success: false,
			Message: "Failed to remove member: " + err.Error(),
		}, nil
	}

	// Update our config to remove the node
	delete(s.config.Nodes, member.ID)

	// Persist the updated configuration
	if err := s.config.Save(); err != nil {
		s.logger.Errorf("Failed to save config after removing node: %v", err)
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

// Reconfigure updates the server configuration in real-time
func (s *Server) Reconfigure() error {
	s.logger.Info("Reconfiguring PulseHA server...")

	s.Lock()
	defer s.Unlock()

	// Reload config
	s.logger.Debug("Reloading configuration...")
	if err := s.config.Reload(); err != nil {
		return fmt.Errorf("failed to reload config: %v", err)
	}

	// Get local node config
	s.logger.Debug("Getting updated local node configuration...")
	localNode, err := s.config.GetLocalNode()
	if err != nil {
		return fmt.Errorf("failed to get local node config: %v", err)
	}
	s.logger.Infof("Updated local node configuration: IP=%s, Port=%s", localNode.IP, localNode.Port)

	// Stop existing gRPC server if it exists
	if s.grpcServer != nil {
		s.logger.Debug("Stopping existing gRPC server...")
		s.grpcServer.GracefulStop()
	}

	// Create new gRPC server for cluster-only RPC and start listener
	s.logger.Debug("Creating new cluster gRPC server...")
	s.grpcServer = grpc.NewServer()
	rpc.RegisterServerServer(s.grpcServer, s)
	// Also register CLI service on the cluster listener for remote operations (e.g., join)
	rpc.RegisterCLIServer(s.grpcServer, s)

	s.logger.Debugf("Starting cluster listener on %s:%s...", localNode.IP, localNode.Port)
	if err := s.startClusterListener(localNode); err != nil {
		return fmt.Errorf("failed to start cluster listener: %v", err)
	}

	s.logger.Info("Server reconfiguration completed successfully")
	return nil
}

// GetMemberList returns the server's member list
func (s *Server) GetMemberList() *membership.MemberList {
	return s.memberList
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
		var allIPs []string
		for _, member := range s.memberList.Members {
			allIPs = append(allIPs, member.ActiveIPs...)
			member.ActiveIPs = nil // Clear current assignments
		}

		if err := s.memberList.RedistributeIPs(allIPs); err != nil {
			s.logger.Errorf("Failed to redistribute IPs: %v", err)
			// Continue anyway as the mode change is already saved
		}
	}

	// If switching to active-passive, move all IPs to the active node
	if req.Mode == "active-passive" {
		s.logger.Info("Moving all IPs to active node")
		var activeNode *membership.Member
		var allIPs []string

		// Find active node and collect all IPs
		for _, member := range s.memberList.Members {
			if member.Status == membership.StatusActive {
				activeNode = member
			}
			allIPs = append(allIPs, member.ActiveIPs...)
			member.ActiveIPs = nil // Clear current assignments
		}

		// Assign all IPs to active node
		if activeNode != nil {
			activeNode.ActiveIPs = allIPs
		}
	}

	s.logger.Infof("Successfully changed cluster mode to: %s", req.Mode)
	return &rpc.SetModeResponse{
		Success: true,
		Message: fmt.Sprintf("cluster mode changed to %s", req.Mode),
	}, nil
}

// CreateGroup implements the CLI.CreateGroup RPC method
func (s *Server) CreateGroup(ctx context.Context, req *rpc.CreateGroupRequest) (*rpc.CreateGroupResponse, error) {
	s.logger.Infof("Received CreateGroup request for group: %s", req.Name)
	s.Lock()
	defer s.Unlock()

	// Check if group already exists
	if _, exists := s.config.Groups[req.Name]; exists {
		return &rpc.CreateGroupResponse{
			Success: false,
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
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.CreateGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	s.logger.Infof("Successfully created group: %s", req.Name)
	return &rpc.CreateGroupResponse{
		Success: true,
		Message: fmt.Sprintf("group %s created successfully", req.Name),
	}, nil
}

// AddIPToGroup implements the CLI.AddIPToGroup RPC method
func (s *Server) AddIPToGroup(ctx context.Context, req *rpc.AddIPToGroupRequest) (*rpc.AddIPToGroupResponse, error) {
	s.logger.Infof("Received AddIPToGroup request for group: %s, IP: %s", req.GroupName, req.Ip)
	s.Lock()
	defer s.Unlock()

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

	// Check if IP already exists in any group
	for g, ips := range s.config.Groups {
		for _, existingIP := range ips {
			if existingIP == ipToUse {
				return &rpc.AddIPToGroupResponse{
					Success: false,
					Message: fmt.Sprintf("IP %s already exists in group %s", ipToUse, g),
				}, nil
			}
		}
	}

	// Find nodes that have this group assigned and try to bring up the IP
	ipBroughtUp := false
	for nodeID, node := range s.config.Nodes {
		for iface, groups := range node.IPGroups {
			for _, g := range groups {
				if g == req.GroupName {
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

						// Check if IP is already in use on another interface
						ipObj, _ := utils.GetCIDR(ipToUse)
						if ipObj != nil {
							exists, existingIface, err := network.CheckIfIPExists(ipObj.String())
							if err != nil {
								warnings = append(warnings, fmt.Sprintf("Failed to check if IP exists: %v", err))
								continue
							}
							if exists {
								warnings = append(warnings, fmt.Sprintf("IP %s is already in use on interface %s", ipToUse, existingIface))
								continue
							}
						}

						if err := network.BringIPup(iface, ipToUse); err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to bring up IP %s on interface %s: %v", ipToUse, iface, err))
							continue
						}
						ipBroughtUp = true
						s.logger.Infof("Successfully brought up IP %s on interface %s", ipToUse, iface)
					} else {
						// This is a remote node, send RPC to bring up the IP
						s.logger.Infof("Sending request to bring up IP %s on node %s", ipToUse, node.Hostname)
						remoteClient, err := client.New()
						if err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to create client for node %s: %v", node.Hostname, err))
							continue
						}

						// Connect to remote node
						if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
							remoteClient.Close()
							warnings = append(warnings, fmt.Sprintf("Failed to connect to node %s: %v", node.Hostname, err))
							continue
						}

						// Send request to bring up IP
						resp, err := remoteClient.Server().BringUpIP(ctx, &rpc.UpIpRequest{
							Iface: iface,
							Ips:   []string{ipToUse},
						})
						remoteClient.Close()

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

	// If we couldn't bring up the IP on any node, return the error
	if !ipBroughtUp && len(warnings) > 0 {
		return &rpc.AddIPToGroupResponse{
			Success:  false,
			Message:  "Failed to bring up IP on any node",
			Warnings: warnings,
		}, nil
	}

	// Add IP to group in config
	s.config.Groups[req.GroupName] = append(s.config.Groups[req.GroupName], ipToUse)

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.AddIPToGroupResponse{
			Success:  false,
			Message:  fmt.Sprintf("failed to save config: %v", err),
			Warnings: warnings,
		}, nil
	}

	s.logger.Infof("Successfully added IP %s to group %s", ipToUse, req.GroupName)
	return &rpc.AddIPToGroupResponse{
		Success:  true,
		Message:  fmt.Sprintf("successfully added IP %s to group %s", ipToUse, req.GroupName),
		Warnings: warnings,
	}, nil
}

// RemoveIPFromGroup implements the CLI.RemoveIPFromGroup RPC method
func (s *Server) RemoveIPFromGroup(ctx context.Context, req *rpc.RemoveIPFromGroupRequest) (*rpc.RemoveIPFromGroupResponse, error) {
	s.logger.Infof("Received RemoveIPFromGroup request for group: %s, IP: %s", req.GroupName, req.Ip)
	s.Lock()
	defer s.Unlock()

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
		return &rpc.RemoveIPFromGroupResponse{
			Success: false,
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
						}
					} else {
						// This is a remote node, send RPC to bring down the IP
						s.logger.Infof("Sending request to bring down IP %s on node %s", foundExactIP, node.Hostname)
						remoteClient, err := client.New()
						if err != nil {
							warnings = append(warnings, fmt.Sprintf("Failed to create client for node %s: %v", node.Hostname, err))
							continue
						}

						// Connect to remote node
						if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
							remoteClient.Close()
							warnings = append(warnings, fmt.Sprintf("Failed to connect to node %s: %v", node.Hostname, err))
							continue
						}

						// Send request to bring down IP
						resp, err := remoteClient.Server().BringDownIP(ctx, &rpc.DownIpRequest{
							Iface: iface,
							Ips:   []string{foundExactIP},
						})
						remoteClient.Close()

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
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.RemoveIPFromGroupResponse{
			Success:  false,
			Message:  fmt.Sprintf("failed to save config: %v", err),
			Warnings: warnings,
		}, nil
	}

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
	s.logger.Infof("Received AssignGroupToNode request for group: %s, node_id: %s, interface: %s", req.GroupName, req.NodeId, req.Interface)
	s.Lock()
	defer s.Unlock()

	// Check if group exists
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		return &rpc.AssignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s does not exist", req.GroupName),
		}, nil
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

	// Check if group is already assigned to this interface
	for _, g := range node.IPGroups[req.Interface] {
		if g == req.GroupName {
			return &rpc.AssignGroupResponse{
				Success: false,
				Message: fmt.Sprintf("group %s is already assigned to interface %s on node %s", req.GroupName, req.Interface, req.NodeId),
			}, nil
		}
	}

	// Add group to interface
	node.IPGroups[req.Interface] = append(node.IPGroups[req.Interface], req.GroupName)

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.AssignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
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
	s.logger.Infof("Received UnassignGroupFromNode request for group: %s, node: %s, interface: %s", req.GroupName, req.Hostname, req.Interface)
	s.Lock()
	defer s.Unlock()

	// Check if group exists
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		return &rpc.UnassignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s does not exist", req.GroupName),
		}, nil
	}

	// Resolve node: treat Hostname field as either node ID or hostname
	var (
		node      *config.Node
		nodeFound bool
		nodeRef   = req.Hostname
	)
	if n, ok := s.config.Nodes[nodeRef]; ok {
		node = n
		nodeFound = true
	} else {
		for _, n := range s.config.Nodes {
			if n.Hostname == nodeRef {
				node = n
				nodeFound = true
				break
			}
		}
	}

	if !nodeFound || node == nil {
		return &rpc.UnassignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("node %s not found", nodeRef),
		}, nil
	}

	// Check if group is assigned to this interface
	if node.IPGroups == nil {
		return &rpc.UnassignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s is not assigned to interface %s on node %s", req.GroupName, req.Interface, nodeRef),
		}, nil
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
		return &rpc.UnassignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s is not assigned to interface %s on node %s", req.GroupName, req.Interface, nodeRef),
		}, nil
	}

	// Remove group from slice
	node.IPGroups[req.Interface] = append(groups[:groupIndex], groups[groupIndex+1:]...)

	// If interface has no more groups, remove the entry
	if len(node.IPGroups[req.Interface]) == 0 {
		delete(node.IPGroups, req.Interface)
	}

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.UnassignGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	s.logger.Infof("Successfully unassigned group %s from interface %s on node %s", req.GroupName, req.Interface, nodeRef)
	return &rpc.UnassignGroupResponse{
		Success: true,
		Message: fmt.Sprintf("successfully unassigned group %s from interface %s on node %s", req.GroupName, req.Interface, nodeRef),
	}, nil
}

// DeleteGroup implements the CLI.DeleteGroup RPC method
func (s *Server) DeleteGroup(ctx context.Context, req *rpc.DeleteGroupRequest) (*rpc.DeleteGroupResponse, error) {
	s.logger.Infof("Received DeleteGroup request for group: %s, force: %v", req.GroupName, req.Force)
	s.Lock()
	defer s.Unlock()

	var warnings []string

	// Check if group exists
	if _, exists := s.config.Groups[req.GroupName]; !exists {
		return &rpc.DeleteGroupResponse{
			Success: false,
			Message: fmt.Sprintf("group %s does not exist", req.GroupName),
		}, nil
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
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.DeleteGroupResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

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
			s.logger.Errorf("Failed to marshal groups: %v", err)
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
							Hostname:  id, // temporarily set into Hostname field until proto updated
							Interface: iface,
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
	s.Lock()
	defer s.Unlock()

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
		s.logger.Errorf("Failed to get hostname: %v", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("failed to get hostname: %v", err),
		}, nil
	}

	// Generate certificates for mTLS
	if err := security.GenerateCertificates(hostname); err != nil {
		s.logger.Warnf("Failed to generate certificates: %v", err)
		// Continue without TLS for now
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
		s.logger.Warnf("Failed to get network interfaces: %v", err)
	} else {
		for _, iface := range interfaces {
			// Skip loopback, down interfaces, and interfaces without addresses
			if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
				continue
			}

			addrs, err := iface.Addrs()
			if err != nil {
				s.logger.Warnf("Failed to get addresses for interface %s: %v", iface.Name, err)
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
			}
		}
	}

	// Save config
	if err := s.config.Save(); err != nil {
		s.logger.Errorf("Failed to save config: %v", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("failed to save config: %v", err),
		}, nil
	}

	// Add the first member to the member list
	if err := s.memberList.AddMember(nodeID, hostname, req.BindIp, bindPort); err != nil {
		s.logger.Errorf("Failed to add first node to member list: %v", err)
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
		s.logger.Errorf("Failed to reconfigure server: %v", err)
		return &rpc.CreateClusterResponse{
			Success: false,
			Message: fmt.Sprintf("cluster created but failed to reconfigure server: %v", err),
		}, nil
	}

	// After successfully creating the cluster, start the health checker
	s.startHealthChecker()

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
		s.logger.Errorf("Failed to save configuration with new token: %v", err)
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
			remoteClient, err := client.New()
			if err != nil {
				continue
			}
			if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
				remoteClient.Close()
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: configBytes})
			cancel()
			remoteClient.Close()
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
	s.logger.Debug("Received configuration sync request")

	if req.Config == nil {
		return &rpc.ConfigSyncResponse{
			Success: false,
			Message: "no configuration data provided",
		}, nil
	}

	// Create a new config instance
	newConfig := &config.Config{}

	// Unmarshal the received configuration
	if err := json.Unmarshal(req.Config, newConfig); err != nil {
		s.logger.Errorf("Failed to unmarshal configuration: %v", err)
		return &rpc.ConfigSyncResponse{
			Success: false,
			Message: fmt.Sprintf("failed to unmarshal configuration: %v", err),
		}, nil
	}

	s.Lock()
	defer s.Unlock()

	// Update our configuration
	s.config = newConfig

	// Reconfigure the server with the new configuration
	if err := s.Reconfigure(); err != nil {
		s.logger.Errorf("Failed to reconfigure server: %v", err)
		return &rpc.ConfigSyncResponse{
			Success: false,
			Message: fmt.Sprintf("failed to reconfigure server: %v", err),
		}, nil
	}

	s.logger.Info("Configuration successfully synchronized")
	return &rpc.ConfigSyncResponse{
		Success: true,
		Message: "configuration successfully synchronized",
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
		if level, err := logrus.ParseLevel(req.Value); err == nil {
			s.logger.SetLevel(level)
		}
	}

	return &rpc.UpdateConfigResponse{Success: true, Message: "updated"}, nil
}

// ResyncNetwork implements CLI.ResyncNetwork RPC
func (s *Server) ResyncNetwork(ctx context.Context, req *rpc.ResyncNetworkRequest) (*rpc.ResyncNetworkResponse, error) {
	s.Lock()
	defer s.Unlock()

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
	}

	// Force immediate activation if cluster configuration exists
	if err := s.config.Reload(); err != nil {
		return &rpc.ResyncNetworkResponse{Success: false, Message: fmt.Sprintf("failed to reload config: %v", err)}, nil
	}

	if s.config.ClusterCheck() {
		// Sync member list with latest config and reload members
		s.memberList.UpdateConfig(s.config)
		if err := s.loadInitialMembers(); err != nil {
			s.logger.Errorf("Failed to load members during resync: %v", err)
			return &rpc.ResyncNetworkResponse{Success: false, Message: fmt.Sprintf("failed to load members: %v", err)}, nil
		}

		// Ensure cluster listener is bound and health checker running
		if err := s.Reconfigure(); err != nil {
			s.logger.Errorf("Failed to reconfigure server during resync: %v", err)
			return &rpc.ResyncNetworkResponse{Success: false, Message: fmt.Sprintf("failed to reconfigure server: %v", err)}, nil
		}

		s.startHealthChecker()

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
					s.logger.WithError(err).Warnf("Resync: failed to create client for peer %s (%s:%s)", id, node.IP, node.Port)
					continue
				}
				if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
					remoteClient.Close()
					s.logger.WithError(err).Warnf("Resync: failed to connect to peer %s (%s:%s)", id, node.IP, node.Port)
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				resp, err := remoteClient.CLI().Status(ctx, &rpc.StatusRequest{})
				cancel()
				remoteClient.Close()
				if err != nil || resp == nil {
					s.logger.WithError(err).Warnf("Resync: failed to get status from peer %s", id)
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
					if s.quorumManager == nil || !s.config.Pulse.QuorumEnabled {
						s.logger.Infof("Resync: member %s missing from majority but quorum disabled; skipping automatic removal", id)
						continue
					}
					subject := id
					description := fmt.Sprintf("Remove node %s due to absence from majority and unknown status", id)
					sessionID, err := s.quorumManager.StartVotingSession(quorum.VoteTypeConfigChange, subject, description, 30*time.Second)
					if err != nil {
						s.logger.WithError(err).Warnf("Resync: failed to start removal vote for %s", id)
						continue
					}

					// Poll for result (short window)
					passed := false
					for i := 0; i < 30; i++ {
						time.Sleep(1 * time.Second)
						session, err := s.quorumManager.GetVotingSession(sessionID)
						if err != nil {
							s.logger.WithError(err).Warnf("Resync: failed to get voting session %s", sessionID)
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
								remoteClient, err := client.New()
								if err != nil {
									continue
								}
								if err := remoteClient.Connect(node.IP, node.Port, false); err != nil {
									remoteClient.Close()
									continue
								}
								ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
								_, _ = remoteClient.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: configBytes})
								cancel()
								remoteClient.Close()
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
