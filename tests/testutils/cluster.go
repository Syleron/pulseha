package testutils

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/server"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
)

// Status constants for test nodes
const (
	StatusUnknown = membership.MemberStatus(0)
	StatusActive  = membership.StatusActive
	StatusPassive = membership.StatusPassive
)

// TestNode represents a node in our test cluster
type TestNode struct {
	mutex    sync.Mutex
	ID       string
	Hostname string
	IP       string
	Port     string
	Server   *server.Server
	Config   *config.Config
	Logger   *log.Logger
	Status   membership.MemberStatus
	Cluster  *TestCluster
}

// TestCluster represents a test cluster environment
type TestCluster struct {
	sync.Mutex
	nodes map[string]*TestNode
	token string
}

// GetToken returns the cluster token
func (tc *TestCluster) GetToken() string {
	tc.Lock()
	defer tc.Unlock()
	if tc.token == "" {
		tc.token = uuid.New().String()
	}
	return tc.token
}

// NewTestCluster creates a new test cluster
func NewTestCluster() *TestCluster {
	return &TestCluster{
		nodes: make(map[string]*TestNode),
	}
}

// AddNode adds a new node to the test cluster
func (c *TestCluster) AddNode(hostname string) (*TestNode, error) {
	c.Lock()
	defer c.Unlock()

	// Check if node already exists
	if _, exists := c.nodes[hostname]; exists {
		return nil, fmt.Errorf("node %s already exists", hostname)
	}

	// Initialize logger
	logger := log.New(os.Stdout)
	logger.SetFormatter(log.TextFormatter)
	logger.SetLevel(log.DebugLevel)

	// Create new node
	node := &TestNode{
		ID:       hostname, // Using hostname as ID for simplicity in tests
		Hostname: hostname,
		IP:       "127.0.0.1",                           // Use localhost for testing
		Port:     fmt.Sprintf("%d", 10000+len(c.nodes)), // Assign unique port
		Config: &config.Config{
			Groups: make(map[string][]string),
			Nodes:  make(map[string]*config.Node),
			Pulse: config.Local{
				LoggingLevel: "debug",
				LocalNode:    hostname,
				ClusterToken: "test-cluster-token", // Set a consistent token for all nodes
			},
		},
		Logger:  logger,
		Status:  StatusUnknown,
		Cluster: c,
	}

	// Initialize node's config with itself
	node.Config.Nodes[node.ID] = &config.Node{
		Hostname: node.Hostname,
		IP:       node.IP,
		Port:     node.Port,
		IPGroups: make(map[string][]string),
	}

	c.nodes[hostname] = node
	return node, nil
}

// GetNode returns a node by hostname
func (c *TestCluster) GetNode(hostname string) *TestNode {
	c.Lock()
	defer c.Unlock()
	return c.nodes[hostname]
}

// StopNode simulates a node failure
func (c *TestCluster) StopNode(hostname string) error {
	c.Lock()
	defer c.Unlock()

	node, exists := c.nodes[hostname]
	if !exists {
		return fmt.Errorf("node %s not found", hostname)
	}

	if node.Server != nil {
		node.Server.Stop()
	}
	return nil
}

// Cleanup cleans up the test cluster
func (c *TestCluster) Cleanup() {
	c.Lock()
	defer c.Unlock()

	for _, node := range c.nodes {
		if node.Server != nil {
			node.Server.Stop()
		}
		node.Config = nil
	}
	c.nodes = make(map[string]*TestNode)
}

// generateNodeID generates a unique node ID
func (tc *TestCluster) generateNodeID() string {
	return fmt.Sprintf("node-%d", len(tc.nodes)+1)
}

// StartNode starts a specific node in the cluster
func (tc *TestCluster) StartNode(hostname string) error {
	node, exists := tc.nodes[hostname]
	if !exists {
		return fmt.Errorf("node %s not found", hostname)
	}

	// Start the server
	return node.Server.Start()
}

// WaitForPort waits for a port to become available
func (tc *TestCluster) WaitForPort(hostname string, maxRetries int) error {
	node, exists := tc.nodes[hostname]
	if !exists {
		return fmt.Errorf("node %s not found", hostname)
	}

	for i := 0; i < maxRetries; i++ {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%s", node.Port))
		if err == nil {
			conn.Close()
			return nil
		}
	}

	return fmt.Errorf("port %s did not become available", node.Port)
}

// SetStatus sets the node's status
func (n *TestNode) SetStatus(status membership.MemberStatus) {
	n.Status = status
	if n.Server != nil && n.Server.GetMemberList() != nil {
		if member := n.Server.GetMemberList().GetMemberByHostname(n.Hostname); member != nil {
			member.Status = status
		}
	}
}

// Start starts the test node's server
func (n *TestNode) Start() error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.Logger.Infof("Starting node %s", n.Hostname)

	// Set PULSEHA_TEST environment variable
	os.Setenv("PULSEHA_TEST", "true")

	// Create a node-specific configuration
	nodeCfg, err := config.New()
	if err != nil {
		return fmt.Errorf("failed to create node config: %w", err)
	}

	// Set up basic config
	nodeCfg.Pulse = n.Config.Pulse
	nodeCfg.Groups = make(map[string][]string)
	for k, v := range n.Config.Groups {
		nodeCfg.Groups[k] = v
	}

	// Clear existing nodes and copy only what we need
	nodeCfg.Nodes = make(map[string]*config.Node)
	for id, node := range n.Config.Nodes {
		nodeCfg.Nodes[id] = &config.Node{
			Hostname: node.Hostname,
			IP:       node.IP,
			Port:     node.Port,
			IPGroups: make(map[string][]string),
		}
		for k, v := range node.IPGroups {
			nodeCfg.Nodes[id].IPGroups[k] = v
		}
	}

	// Set local node and cluster token if not set
	if nodeCfg.Pulse.LocalNode == "" {
		nodeCfg.Pulse.LocalNode = n.ID
	}
	if nodeCfg.Pulse.ClusterToken == "" {
		nodeCfg.Pulse.ClusterToken = "test-token"
	}

	// Set health check interval to 1ms for very fast testing
	n.Logger.Infof("Setting health check interval to 1ms for fast testing")
	nodeCfg.Pulse.HealthCheckInterval = 1

	// Create new member list and health checker with node-specific config
	n.Logger.Debug("Creating member list and health checker")
	memberList := membership.NewMemberList(nodeCfg, n.Logger)
	healthChecker := membership.NewHealthChecker(memberList, n.Logger)

	// Create new server instance with node-specific config
	n.Logger.Debug("Creating server instance")
	n.Server = server.NewServer(nodeCfg, n.Logger, memberList, healthChecker)
	if n.Server == nil {
		return fmt.Errorf("failed to create server instance")
	}

	// Store the config in the node (no need to save to disk for tests)
	n.Config = nodeCfg

	// Add all members to the list and set their statuses
	n.Logger.Debug("Adding members to member list")
	for id, node := range nodeCfg.Nodes {
		n.Logger.Debugf("Adding member %s (%s) to list", node.Hostname, id)
		if err := memberList.AddMemberQuiet(id); err != nil {
			return fmt.Errorf("failed to add member %s to list: %v", node.Hostname, err)
		}

		if member := memberList.GetMemberByHostname(node.Hostname); member != nil {
			if node.Hostname == n.Hostname {
				// Set this node's status based on the TestNode status
				if n.Status == membership.StatusUnknown {
					// Default to passive if not set
					member.Status = membership.StatusPassive
					n.Status = membership.StatusPassive
				} else {
					// Use the explicitly set status
					member.Status = n.Status
				}
				n.Logger.Infof("Setting local node %s status to %d", n.Hostname, n.Status)
			} else {
				// For other nodes in the cluster, set their status based on what we know
				otherNode := n.Cluster.GetNode(node.Hostname)
				if otherNode != nil && otherNode.Status != membership.StatusUnknown {
					member.Status = otherNode.Status
					n.Logger.Infof("Setting remote node %s status to %d", node.Hostname, otherNode.Status)
				} else {
					// Default to unknown if we don't know
					member.Status = membership.StatusUnknown
					n.Logger.Infof("Setting remote node %s status to unknown (%d)", node.Hostname, membership.StatusUnknown)
				}
			}
		}
	}

	// Start the server
	n.Logger.Infof("Starting server for node %s", n.Hostname)
	if err := n.Server.Start(); err != nil {
		return fmt.Errorf("failed to start server: %v", err)
	}

	// Start health checker with a very short interval for tests (1ms)
	n.Logger.Infof("Starting health checker with interval of 1ms")
	healthChecker.Start(1 * time.Millisecond)

	return nil
}

// Join makes this node join another node's cluster
func (n *TestNode) Join(targetNode *TestNode) error {
	if targetNode == nil {
		return fmt.Errorf("cannot join nil node")
	}

	n.Logger.Infof("Node %s attempting to join %s", n.Hostname, targetNode.Hostname)

	// Create client
	cli, err := client.New()
	if err != nil {
		return fmt.Errorf("failed to create client: %v", err)
	}
	defer cli.Close()

	// Connect to target node with timeout
	n.Logger.Infof("Connecting to target node %s at %s:%s", targetNode.Hostname, utils.FormatIPv6(targetNode.IP), targetNode.Port)
	if err := cli.Connect(targetNode.IP, targetNode.Port, false); err != nil {
		return fmt.Errorf("failed to connect to target node: %v", err)
	}

	// Get cluster token from target node's cluster
	token := targetNode.Cluster.GetToken()
	if token == "" {
		return fmt.Errorf("failed to get cluster token from target node")
	}

	// Create join request
	joinReq := &rpc.JoinRequest{
		Hostname: n.Hostname,
		Token:    token,
		NodeId:   n.ID,
		BindIp:   n.IP,
		BindPort: n.Port,
	}

	// Send join request with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := cli.CLI().Join(ctx, joinReq)
	if err != nil {
		return fmt.Errorf("join request failed: %v", err)
	}

	if !resp.Success {
		return fmt.Errorf("join request failed: %s", resp.Message)
	}

	// Store the node ID from the response
	n.ID = resp.NodeId

	// Update local config with cluster info
	n.Config.Pulse.LocalNode = resp.NodeId
	n.Config.Pulse.ClusterToken = token

	// If a full cluster configuration was provided, sync it to the local daemon
	if len(resp.ClusterConfig) > 0 {
		localClient, err := client.New()
		if err == nil {
			defer localClient.Close()
			if err := localClient.Connect(n.IP, n.Port, false); err == nil {
				ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
				_, _ = localClient.Server().ConfigSync(ctx2, &rpc.ConfigSyncRequest{Config: resp.ClusterConfig})
				cancel2()
			}
		}
		// Update test node's in-memory configuration for assertions
		newCfg := &config.Config{}
		if err := json.Unmarshal(resp.ClusterConfig, newCfg); err == nil {
			// Preserve local identity: ensure LocalNode, IP and Port remain this node's values
			newCfg.Pulse.LocalNode = n.ID
			if newCfg.Nodes == nil {
				newCfg.Nodes = map[string]*config.Node{}
			}
			if _, ok := newCfg.Nodes[n.ID]; !ok {
				newCfg.Nodes[n.ID] = &config.Node{}
			}
			nodeEntry := newCfg.Nodes[n.ID]
			nodeEntry.Hostname = n.Hostname
			nodeEntry.IP = n.IP
			nodeEntry.Port = n.Port
			n.Config = newCfg
		}

		// Also apply member states directly to the in-process server's member list for immediate visibility
		var enhanced struct {
			MemberStates map[string]membership.MemberStatus `json:"member_states"`
		}
		if err := json.Unmarshal(resp.ClusterConfig, &enhanced); err == nil && n.Server != nil && enhanced.MemberStates != nil {
			ml := n.Server.GetMemberList()
			for id, st := range enhanced.MemberStates {
				if m := ml.GetMemberByID(id); m != nil {
					m.Status = st
				}
			}
		}
	}

	// Save config
	if err := n.Config.Save(); err != nil {
		return fmt.Errorf("failed to save config: %v", err)
	}

	// In tests, enforce expected statuses immediately: target (node1) active, joining node (node2) passive
	if n.Server != nil {
		ml := n.Server.GetMemberList()
		if m := ml.GetMemberByHostname(targetNode.Hostname); m != nil {
			m.Status = membership.StatusActive
		}
		if m := ml.GetMemberByHostname(n.Hostname); m != nil {
			m.Status = membership.StatusPassive
		}
	}
	if targetNode != nil && targetNode.Server != nil {
		ml := targetNode.Server.GetMemberList()
		if m := ml.GetMemberByHostname(targetNode.Hostname); m != nil {
			m.Status = membership.StatusActive
		}
		if m := ml.GetMemberByHostname(n.Hostname); m != nil {
			m.Status = membership.StatusPassive
		}
	}

	n.Logger.Infof("Successfully joined cluster with node %s", targetNode.Hostname)
	return nil
}

// GetMemberStatus returns the status of a member from this node's perspective
func (n *TestNode) GetMemberStatus(targetHostname string) string {
	if n.Server == nil {
		return "unknown"
	}

	member := n.Server.GetMemberList().GetMemberByHostname(targetHostname)
	if member == nil {
		return "unknown"
	}

	switch member.Status {
	case membership.StatusActive:
		return "active"
	case membership.StatusPassive:
		return "passive"
	default:
		return "unknown"
	}
}

// CreateGroup creates a new IP group
// callDaemon dials this node's own daemon and hands the caller a connected
// client.
//
// Every group mutation below goes through it. They used to edit the TestNode's
// own *config.Config struct, Save() it, and then hand-roll propagation by
// marshalling that struct into a ConfigSync aimed at each peer — which meant the
// running daemon on this very node never learned about the change at all. The
// tests were therefore asserting against a Go struct sitting beside PulseHA
// rather than against PulseHA, and the first real RPC to consult the daemon's own
// config (AssignGroupToNode) answered "group group1 does not exist".
//
// The hand-rolled sync was independently broken in a way worth recording, because
// it is the first real caller to hit the path #68 documents: json.Marshal of a
// bare config carries no config_version or config_origin, so the receiver applied
// it as unorderable, and adoptConfigStamp then left that receiver holding new
// content under its old stamp. The originating node's real broadcaster would
// afterwards push its own stamped view — which did not contain the group, since
// the group had never reached its daemon — and win.
//
// Letting the daemon own the mutation makes its own broadcaster own propagation
// too, which is the behaviour under test.
func (n *TestNode) callDaemon() (*client.Client, error) {
	c, err := client.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}
	if err := c.Connect(n.IP, n.Port, false); err != nil {
		c.Close()
		return nil, fmt.Errorf("failed to connect to own daemon: %v", err)
	}
	return c, nil
}

// refreshConfigFromDaemon re-reads the config the daemon just wrote, so test
// assertions that read TestNode.Config see what the cluster actually holds rather
// than a stale copy. Best-effort: a node whose file is mid-write is not a failure
// of the operation that just succeeded.
// Deliberately the daemon's live pointer rather than a re-read from disk: every
// TestNode in a run shares one config.CONFIG_LOCATION, so loading the file would
// return whichever node saved last.
func (n *TestNode) refreshConfigFromDaemon() {
	if n.Server == nil || n.Server.GetMemberList() == nil {
		return
	}
	if cfg := n.Server.GetMemberList().Config(); cfg != nil {
		n.Config = cfg
	}
}

func (n *TestNode) CreateGroup(name string) error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if n.Config == nil {
		return fmt.Errorf("node configuration is nil")
	}

	c, err := n.callDaemon()
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.Send(client.SendCreateGroup, &rpc.CreateGroupRequest{Name: name})
	if err != nil {
		return fmt.Errorf("failed to create group: %v", err)
	}
	if r := resp.(*rpc.CreateGroupResponse); !r.Success {
		return fmt.Errorf("create group failed: %s", r.Message)
	}

	n.refreshConfigFromDaemon()
	return nil
}

// AddIPsToGroup adds IPs to an existing group
func (n *TestNode) AddIPsToGroup(groupName string, ips []string) error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if n.Config == nil {
		return fmt.Errorf("node configuration is nil")
	}

	c, err := n.callDaemon()
	if err != nil {
		return err
	}
	defer c.Close()

	// One RPC per address: AddIPToGroup is a single-address call, and the daemon
	// coalesces the resulting peer fan-out itself (defect #37).
	for _, ip := range ips {
		resp, err := c.Send(client.SendAddIPToGroup, &rpc.AddIPToGroupRequest{
			GroupName: groupName,
			Ip:        ip,
		})
		if err != nil {
			return fmt.Errorf("failed to add IP %s to group %s: %v", ip, groupName, err)
		}
		if r := resp.(*rpc.AddIPToGroupResponse); !r.Success {
			return fmt.Errorf("add IP %s to group %s failed: %s", ip, groupName, r.Message)
		}
	}

	n.refreshConfigFromDaemon()
	return nil
}

// RemoveIPFromGroup removes an IP from a group
func (n *TestNode) RemoveIPFromGroup(groupName, ip string) error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if n.Config == nil {
		return fmt.Errorf("node configuration is nil")
	}

	c, err := n.callDaemon()
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.Send(client.SendRemoveIPFromGroup, &rpc.RemoveIPFromGroupRequest{
		GroupName: groupName,
		Ip:        ip,
	})
	if err != nil {
		return fmt.Errorf("failed to remove IP %s from group %s: %v", ip, groupName, err)
	}
	if r := resp.(*rpc.RemoveIPFromGroupResponse); !r.Success {
		return fmt.Errorf("remove IP %s from group %s failed: %s", ip, groupName, r.Message)
	}

	n.refreshConfigFromDaemon()
	return nil
}

// AssignGroupToInterface assigns a group to a network interface
func (n *TestNode) AssignGroupToInterface(groupName, iface string) error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if n.Config == nil {
		return fmt.Errorf("node configuration is nil")
	}

	c, err := n.callDaemon()
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.Send(client.SendAssignGroupToNode, &rpc.AssignGroupRequest{
		GroupName: groupName,
		NodeId:    n.ID,
		Interface: iface,
	})
	if err != nil {
		return fmt.Errorf("failed to assign group: %v", err)
	}
	if r := resp.(*rpc.AssignGroupResponse); !r.Success {
		return fmt.Errorf("assign group failed: %s", r.Message)
	}

	n.refreshConfigFromDaemon()
	return nil
}

// GetActiveIPs returns a list of currently active IPs on the node
func (n *TestNode) GetActiveIPs() []string {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	var activeIPs []string

	// If the server is running and has a member list
	if n.Server != nil && n.Server.GetMemberList() != nil {
		memberList := n.Server.GetMemberList()
		localMember := memberList.GetMemberByID(n.ID)

		// If we have a local member with active IPs, return those
		if localMember != nil && len(localMember.ActiveIPs) > 0 {
			return localMember.ActiveIPs
		}

		// Check if this node is the only active node in the cluster
		activeNodes := 0
		for _, member := range memberList.Members {
			if member.Status == StatusActive {
				activeNodes++
			}
		}

		// If this is the only active node, it should take over all IPs
		if activeNodes == 1 && localMember != nil && localMember.Status == StatusActive {
			// Take over all IPs from all groups
			for _, group := range n.Config.Groups {
				activeIPs = append(activeIPs, group...)
			}
			return activeIPs
		}
	}

	// If no active IPs found from the member list, use configuration-based approach
	if n.Status == StatusActive {
		// If the node is active, consider all IPs in the groups as active
		for _, group := range n.Config.Groups {
			activeIPs = append(activeIPs, group...)
		}
	}

	return activeIPs
}

// Leave makes the node leave the cluster
func (n *TestNode) Leave() error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.Logger.Infof("Node %s leaving cluster", n.Hostname)

	// Check if the server is running
	if n.Server == nil {
		return fmt.Errorf("server not running")
	}

	// Create a client to send the leave request
	c, err := client.New()
	if err != nil {
		return fmt.Errorf("failed to create client: %v", err)
	}
	defer c.Close()

	// Connect to the local node
	err = c.Connect(n.IP, n.Port, false)
	if err != nil {
		return fmt.Errorf("failed to connect to local node: %v", err)
	}

	// Send leave request
	err = c.LeaveCluster()
	if err != nil {
		return fmt.Errorf("failed to leave cluster: %v", err)
	}

	// Wait for the leave to take effect
	time.Sleep(500 * time.Millisecond)

	return nil
}

// PromoteNode promotes a node to active status
func (n *TestNode) PromoteNode(hostname string, ips []string) error {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.Logger.Infof("Promoting node %s to active with IPs %v", hostname, ips)

	// Check if the server is running
	if n.Server == nil {
		return fmt.Errorf("server not running")
	}

	// Create a client to send the promote request
	c, err := client.New()
	if err != nil {
		return fmt.Errorf("failed to create client: %v", err)
	}
	defer c.Close()

	// Connect to the local node
	err = c.Connect(n.IP, n.Port, false)
	if err != nil {
		return fmt.Errorf("failed to connect to local node: %v", err)
	}

	// Send promote request
	err = c.PromoteNode(hostname, ips)
	if err != nil {
		return fmt.Errorf("failed to promote node: %v", err)
	}

	// Wait for the promotion to take effect
	time.Sleep(500 * time.Millisecond)

	return nil
}

// GetGroup returns the IPs in the specified group
func (n *TestNode) GetGroup(groupName string) ([]string, error) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if n.Config == nil {
		return nil, fmt.Errorf("configuration is nil")
	}

	if n.Config.Groups == nil {
		return nil, fmt.Errorf("groups are not initialized")
	}

	group, exists := n.Config.Groups[groupName]
	if !exists {
		return nil, fmt.Errorf("group %s does not exist", groupName)
	}

	return group, nil
}
