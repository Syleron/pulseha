package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// isLocalIP checks whether the given IP address is assigned to a local interface
func isLocalIP(ip string) bool {
    if ip == "127.0.0.1" || ip == "::1" {
        return true
    }
    ifaces, err := net.Interfaces()
    if err != nil {
        return false
    }
    for _, iface := range ifaces {
        addrs, err := iface.Addrs()
        if err != nil {
            continue
        }
        for _, addr := range addrs {
            switch v := addr.(type) {
            case *net.IPNet:
                if v.IP.String() == ip {
                    return true
                }
            case *net.IPAddr:
                if v.IP.String() == ip {
                    return true
                }
            }
        }
    }
    return false
}

type Client struct {
	Connection *grpc.ClientConn
	server     rpc.ServerClient
	cliClient  rpc.CLIClient
}

// ProtoFunction represents available RPC functions
type ProtoFunction int

const (
	SendConfigSync ProtoFunction = 1 + iota
	SendJoin
	SendLeave
	SendMakePassive
	SendBringUpIP
	SendBringDownIP
	SendHealthCheck
	SendPromote
	SendLogs
	SendRemove
	SendCreateGroup
	SendAddIPToGroup
	SendRemoveIPFromGroup
	SendAssignGroupToNode
	SendUnassignGroupFromNode
	SendDeleteGroup
	SendListGroups
	SendCreateCluster
	SendToken
)

var protoFunctions = []string{
	"ConfigSync",
	"Join",
	"Leave",
	"MakePassive",
	"BringUpIP",
	"BringDownIP",
	"HealthCheck",
	"Promote",
	"Logs",
	"Remove",
	"CreateGroup",
	"AddIPToGroup",
	"RemoveIPFromGroup",
	"AssignGroupToNode",
	"UnassignGroupFromNode",
	"DeleteGroup",
	"ListGroups",
	"CreateCluster",
	"Token",
}

func (p ProtoFunction) String() string {
	return protoFunctions[p-1]
}

// New creates a new client with default local connection
func New() (*Client, error) {
	// Always connect CLI to localhost
	conn, err := grpc.Dial("127.0.0.1:8080", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to CLI server: %v", err)
	}

	return &Client{
		Connection: conn,
		server:     rpc.NewServerClient(conn),
		cliClient:  rpc.NewCLIClient(conn),
	}, nil
}

// GetProtoFuncList defines the available RPC commands
func (c *Client) GetProtoFuncList() map[string]interface{} {
	return map[string]interface{}{
		"ConfigSync": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.ConfigSync(ctx, data.(*rpc.ConfigSyncRequest))
		},
		"Join": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.Join(ctx, data.(*rpc.JoinRequest))
		},
		"Leave": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.Leave(ctx, data.(*rpc.LeaveRequest))
		},
		"MakePassive": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.MakePassive(ctx, data.(*rpc.MakePassiveRequest))
		},
		"BringUpIP": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.BringUpIP(ctx, data.(*rpc.UpIpRequest))
		},
		"BringDownIP": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.BringDownIP(ctx, data.(*rpc.DownIpRequest))
		},
		"HealthCheck": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.HealthCheck(ctx, data.(*rpc.HealthCheckRequest))
		},
		"Promote": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.Promote(ctx, data.(*rpc.PromoteRequest))
		},
		"Logs": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.Logs(ctx, data.(*rpc.LogsRequest))
		},
		"Remove": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.server.Remove(ctx, data.(*rpc.RemoveRequest))
		},
		"CreateGroup": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.CreateGroup(ctx, data.(*rpc.CreateGroupRequest))
		},
		"AddIPToGroup": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.AddIPToGroup(ctx, data.(*rpc.AddIPToGroupRequest))
		},
		"RemoveIPFromGroup": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.RemoveIPFromGroup(ctx, data.(*rpc.RemoveIPFromGroupRequest))
		},
		"AssignGroupToNode": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.AssignGroupToNode(ctx, data.(*rpc.AssignGroupRequest))
		},
		"UnassignGroupFromNode": func(ctx context.Context, data interface{}) (interface{}, error) {
			// Direct call to server method for now
			return c.callUnassignGroup(ctx, data)
		},
		"DeleteGroup": func(ctx context.Context, data interface{}) (interface{}, error) {
			// Direct call to server method for now
			return c.callDeleteGroup(ctx, data)
		},
		"ListGroups": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.ListGroups(ctx, data.(*rpc.ListGroupsRequest))
		},
		"CreateCluster": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.CreateCluster(ctx, data.(*rpc.CreateClusterRequest))
		},
		"Token": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.Token(ctx, data.(*rpc.TokenRequest))
		},
	}
}

// Connect creates a new client connection with TLS support
func (c *Client) Connect(ip string, port string, tlsEnabled bool) error {
	var err error
	if tlsEnabled {
		// Load member cert/key
		peerCert, err := tls.LoadX509KeyPair(
			security.CertDir+"pulseha.crt",
			security.CertDir+"pulseha.key",
		)
		if err != nil {
			return fmt.Errorf("could not connect to host: %v", err)
		}
		// Load CA
		caCert, err := ioutil.ReadFile(security.CertDir + "ca.crt")
		if err != nil {
			return fmt.Errorf("could not connect to host: %v", err)
		}
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return errors.New("failed to append ca certs")
		}
		creds := credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{peerCert},
			RootCAs:            caCertPool,
		})
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(creds))
	} else {
		// Use insecure connection for non-TLS
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if err != nil {
		log.Errorf("GRPC client connection error: %s", err.Error())
		return fmt.Errorf("could not connect to host: %v", err)
	}
	c.server = rpc.NewServerClient(c.Connection)
	c.cliClient = rpc.NewCLIClient(c.Connection)
	log.Debug("Client:Connect() Connection made to " + ip + ":" + port)
	return nil
}

// Close terminates the client connection
func (c *Client) Close() {
	log.Debug("Client:Close() Connection closed")
	if c.Connection != nil {
		c.Connection.Close()
	}
}

// GetNodeIDByHostname translates a hostname to a node_id
func (c *Client) GetNodeIDByHostname(hostname string) (string, error) {
	cfg := config.New()
	uuid, _, err := cfg.GetNodeByHostname(hostname)
	if err != nil {
		return "", fmt.Errorf("failed to get node ID for hostname %s: %v", hostname, err)
	}
	return uuid, nil
}

// GetHostnameByNodeID translates a node_id to a hostname
func (c *Client) GetHostnameByNodeID(nodeID string) (string, error) {
	cfg := config.New()
	if node, ok := cfg.Nodes[nodeID]; ok {
		return node.Hostname, nil
	}
	return "", fmt.Errorf("failed to get hostname for node ID %s", nodeID)
}

// Send sends an RPC command over the client connection
func (c *Client) Send(funcName ProtoFunction, data interface{}) (interface{}, error) {
	log.Debug("Client:Send() Sending " + funcName.String())
	funcList := c.GetProtoFuncList()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return funcList[funcName.String()].(func(context.Context, interface{}) (interface{}, error))(
		ctx, data,
	)
}

// Add this method to expose the server client
func (c *Client) Server() rpc.ServerClient {
	return c.server
}

// Add this method to expose the CLI client
func (c *Client) CLI() rpc.CLIClient {
	return c.cliClient
}

// ClusterStatus represents the current state of the cluster
type ClusterStatus struct {
	Members []Member    `json:"members"`
	Groups  []GroupInfo `json:"groups"`
	Mode    string      `json:"mode"`
}

type Member struct {
	Hostname      string   `json:"hostname"`
	IP            string   `json:"ip"`
	Port          string   `json:"port"`
	Status        string   `json:"status"`
	IPs           []string `json:"ips"`
	ActiveIPs     []string `json:"active_ips"`
	LastResponse  string   `json:"last_response"`
	Latency       string   `json:"latency"`
	PartialActive bool     `json:"partial_active"`
}

// GroupInfo represents a floating IP group
type GroupInfo struct {
	Name        string            `json:"name"`
	IPs         []string          `json:"ips"`
	Assignments []GroupAssignment `json:"assignments"`
}

// GroupAssignment represents a group assignment to a node interface
type GroupAssignment struct {
	NodeID    string `json:"node_id"`
	Interface string `json:"interface"`
}

// CreateCluster initializes a new cluster
func (c *Client) CreateCluster(bindIP, bindPort, mode string) error {
	r, err := c.Send(
		SendCreateCluster,
		&rpc.CreateClusterRequest{
			BindIp:   bindIP,
			BindPort: bindPort,
			Mode:     mode,
		},
	)
	if err != nil {
		return err
	}
	response := r.(*rpc.CreateClusterResponse)
	if !response.Success {
		return errors.New(response.Message)
	}
	fmt.Println(response.Message)
	return nil
}

// Token gets the current cluster token or generates a new one
func (c *Client) Token(regenerate bool) (*rpc.TokenResponse, error) {
	r, err := c.Send(
		SendToken,
		&rpc.TokenRequest{
			Regenerate: regenerate,
		},
	)
	if err != nil {
		return nil, err
	}
	response := r.(*rpc.TokenResponse)
	return response, nil
}

// JoinCluster joins an existing cluster
func (c *Client) JoinCluster(address, token, bindIP, bindPort string) error {
	return c.JoinClusterWithNodeID(address, token, bindIP, bindPort, "")
}

// JoinClusterWithNodeID allows specifying a custom node ID
func (c *Client) JoinClusterWithNodeID(address, token, bindIP, bindPort, customNodeID string) error {
	// Split address into host and port if port is specified
	host, port := address, "8080"
	if strings.Contains(address, ":") {
		parts := strings.Split(address, ":")
		if len(parts) == 2 {
			host = parts[0]
			port = parts[1]
		}
	}

	// Connect to the target node
	if err := c.Connect(host, port, false); err != nil {
		return fmt.Errorf("failed to connect to target node: %v", err)
	}

	// Get local hostname
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %v", err)
	}

	// Determine node ID
	cfg := c.GetConfig()
	nodeID := customNodeID
	if nodeID == "" {
		nodeID = cfg.GenerateNodeID()
	}

	// Default bind port if not provided
	if bindPort == "" {
		bindPort = "8080"
	}

	// Create join request
	joinReq := &rpc.JoinRequest{
		Address:  hostname,
		Token:    token,
		NodeId:   nodeID,
		BindIp:   bindIP,
		BindPort: bindPort,
	}

	resp, err := c.CLI().Join(context.Background(), joinReq)
	if err != nil {
		return fmt.Errorf("join request failed: %v", err)
	}

	if !resp.Success {
		return fmt.Errorf("join failed: %s", resp.Message)
	}

	// If cluster configuration is provided, use it
	if resp.ClusterConfig != nil && len(resp.ClusterConfig) > 0 {
		// Enhanced config structure that includes member states
		type EnhancedConfig struct {
			*config.Config
			MemberStates map[string]int `json:"member_states"`
		}

		// Unmarshal the enhanced cluster configuration
		enhancedConfig := &EnhancedConfig{}
		if err := json.Unmarshal(resp.ClusterConfig, enhancedConfig); err != nil {
			log.Warnf("Failed to unmarshal enhanced cluster config: %v", err)
			// Try plain config as fallback
			clusterConfig := &config.Config{}
			if err := json.Unmarshal(resp.ClusterConfig, clusterConfig); err != nil {
				log.Warnf("Failed to unmarshal plain cluster config: %v", err)
				// Fall back to minimal config
			} else {
				enhancedConfig = &EnhancedConfig{Config: clusterConfig}
			}
		}

		if enhancedConfig != nil && enhancedConfig.Config != nil {
			// Preserve local-specific settings
			loggingLevel := cfg.Pulse.LoggingLevel
			logToFile := cfg.Pulse.LogToFile
			logFileLocation := cfg.Pulse.LogFileLocation

			// Merge cluster config while preserving local settings
			cfg.Nodes = enhancedConfig.Nodes
			cfg.Groups = enhancedConfig.Groups
			cfg.Pulse.ClusterToken = enhancedConfig.Pulse.ClusterToken
			cfg.Pulse.Mode = enhancedConfig.Pulse.Mode
			cfg.Pulse.QuorumEnabled = enhancedConfig.Pulse.QuorumEnabled
			cfg.Pulse.QuorumMinNodes = enhancedConfig.Pulse.QuorumMinNodes
			cfg.Pulse.HealthCheckInterval = enhancedConfig.Pulse.HealthCheckInterval
			cfg.Pulse.FailOverInterval = enhancedConfig.Pulse.FailOverInterval
			cfg.Pulse.FailOverLimit = enhancedConfig.Pulse.FailOverLimit
			cfg.Pulse.AutoFailback = enhancedConfig.Pulse.AutoFailback

			// Restore local-specific settings but use the new node ID
			cfg.Pulse.LocalNode = resp.NodeId // Use the new UUID assigned by the cluster
			cfg.Pulse.LoggingLevel = loggingLevel
			cfg.Pulse.LogToFile = logToFile
			cfg.Pulse.LogFileLocation = logFileLocation

			// Log member states if available
			if enhancedConfig.MemberStates != nil {
				log.Info("Received member states from cluster:")
				for id, status := range enhancedConfig.MemberStates {
					log.Infof("  Member %s: status=%d", id, status)
				}
			}

			log.Info("Successfully received and merged cluster configuration")
		}
	} else {
		// Fall back to minimal configuration if no cluster config provided
		log.Warn("No cluster configuration received, using minimal config")

		// Update local config with the cluster information
		cfg.Pulse.LocalNode = resp.NodeId
		cfg.Pulse.ClusterToken = token

		// Add this node to the nodes map
		if cfg.Nodes == nil {
			cfg.Nodes = make(map[string]*config.Node)
		}

		cfg.Nodes[resp.NodeId] = &config.Node{
			Hostname: hostname,
			IP:       bindIP,
			Port:     bindPort,
			IPGroups: make(map[string][]string),
		}
	}

	// Save the updated config
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("successfully joined cluster but failed to save local config: %v", err)
	}

	// Post-join validation and health checks
	// 1) Validate bind-ip is local (if provided)
	var warnings []string
	if bindIP != "" {
		if !isLocalIP(bindIP) {
			warnings = append(warnings, fmt.Sprintf("bind-ip %s does not match any local interface", bindIP))
		}
	}

	// 2) Detect duplicate bind tuple (IP:Port) with existing nodes
	if localNode, ok := cfg.Nodes[resp.NodeId]; ok {
		for id, node := range cfg.Nodes {
			if id == resp.NodeId {
				continue
			}
			if node.IP == localNode.IP && node.Port == localNode.Port && node.IP != "" && node.Port != "" {
				warnings = append(warnings, fmt.Sprintf("bind %s:%s conflicts with existing node %s", node.IP, node.Port, id))
			}
		}
	}

	// 3) Wait briefly for local daemon to load new config and report full membership
	// Poll Status up to ~8s
	expectedMembers := len(cfg.Nodes)
	membershipEstablished := false
	for i := 0; i < 8; i++ {
		statusResp, statusErr := c.CLI().Status(context.Background(), &rpc.StatusRequest{})
		if statusErr == nil && statusResp != nil {
			if len(statusResp.Members) >= expectedMembers {
				membershipEstablished = true
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	if !membershipEstablished {
		warnings = append(warnings, "node membership not established within timeout")
	}

	// If we detected any critical issues, provide actionable guidance and return an error
	if len(warnings) > 0 {
		for _, w := range warnings {
			fmt.Printf("WARN  %s\n", w)
		}
		// Suggest corrective action when bind-ip seems wrong and a conflict exists
		if bindIP != "" && !isLocalIP(bindIP) {
			fmt.Printf("HINT  Re-run with a local --bind-ip or update /etc/pulseha/config.json and restart pulseha.\n")
		}
		return fmt.Errorf("join configuration applied, but cluster membership not healthy")
	}

	fmt.Printf("Successfully joined cluster with node ID: %s\n", resp.NodeId)
	return nil
}

// LeaveCluster removes this node from the cluster
func (c *Client) LeaveCluster() error {
	// Get current config to read the local node ID
	cfg := c.GetConfig()
	localNodeID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return fmt.Errorf("failed to get local node ID: %v", err)
	}

	resp, err := c.CLI().Leave(context.Background(), &rpc.LeaveRequest{NodeId: localNodeID})
	if err != nil {
		return fmt.Errorf("leave request failed: %v\nHINT  If the local daemon is unreachable, try: pulsectl cluster leave --address <leader-ip:port>", err)
	}
	if resp == nil || !resp.Success {
		msg := "unknown error"
		if resp != nil && resp.Message != "" {
			msg = resp.Message
		}
		return fmt.Errorf("leave failed: %s", msg)
	}
	fmt.Println(resp.Message)
	return nil
}

// LeaveClusterVia attempts to issue a leave against a specific cluster member (leader/peer)
func (c *Client) LeaveClusterVia(address string, nodeID string) error {
    host, port := address, "8080"
    if strings.Contains(address, ":") {
        parts := strings.Split(address, ":")
        if len(parts) == 2 {
            host = parts[0]
            port = parts[1]
        }
    }
    if err := c.Connect(host, port, false); err != nil {
        return fmt.Errorf("failed to connect to %s:%s: %v", host, port, err)
    }
    if nodeID == "" {
        cfg := c.GetConfig()
        var err error
        nodeID, err = cfg.GetLocalNodeUUID()
        if err != nil {
            return fmt.Errorf("failed to get local node ID: %v", err)
        }
    }
    resp, err := c.CLI().Leave(context.Background(), &rpc.LeaveRequest{NodeId: nodeID})
    if err != nil {
        return fmt.Errorf("leave request to %s:%s failed: %v", host, port, err)
    }
    if resp == nil || !resp.Success {
        msg := "unknown error"
        if resp != nil && resp.Message != "" {
            msg = resp.Message
        }
        return fmt.Errorf("leave failed via %s:%s: %s", host, port, msg)
    }
    fmt.Println(resp.Message)
    return nil
}

// GetClusterStatus returns the current cluster status
func (c *Client) GetClusterStatus() (*ClusterStatus, error) {
	resp, err := c.CLI().Status(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		return nil, err
	}

	// Get current config to read the mode
	cfg := c.GetConfig()

	status := &ClusterStatus{
		Members: make([]Member, len(resp.Members)),
		Groups:  make([]GroupInfo, 0), // Initialize empty groups slice
		Mode:    cfg.Pulse.Mode,       // Read mode from config instead of hardcoding
	}

	for i, m := range resp.Members {
		// Map enum to string
		statusStr := "Unknown"
		switch m.Status {
		case rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE:
			statusStr = "Active"
		case rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE:
			statusStr = "Passive"
		case rpc.MemberStatusEnum_MEMBER_STATUS_PARTIAL_ACTIVE:
			statusStr = "PartialActive"
		case rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN:
			statusStr = "Unknown"
		}
		status.Members[i] = Member{
			Hostname:      m.Hostname,
			Status:        statusStr,
			IPs:           m.ActiveIps,
			ActiveIPs:     m.ActiveIps,
			LastResponse:  m.LastResponse,
			Latency:       m.Latency,
			PartialActive: m.PartialActive,
			IP:            m.Ip,
			Port:          m.Port,
		}

		// Log the latency for debugging
		log.Debugf("Member %s latency from RPC: %s", m.Hostname, m.Latency)
	}

	// Get group information from the config since we can't rely on the proto update yet
	for groupName, ips := range cfg.Groups {
		group := GroupInfo{
			Name:        groupName,
			IPs:         ips,
			Assignments: make([]GroupAssignment, 0),
		}

		// Find assignments for this group
		for id, node := range cfg.Nodes {
			for iface, assignedGroups := range node.IPGroups {
				for _, g := range assignedGroups {
					if g == groupName {
						group.Assignments = append(group.Assignments, GroupAssignment{
							NodeID:    id,
							Interface: iface,
						})
					}
				}
			}
		}

		status.Groups = append(status.Groups, group)
	}

	return status, nil
}

// CreateGroup creates a new IP group
func (c *Client) CreateGroup(name string) error {
	r, err := c.Send(
		SendCreateGroup,
		&rpc.CreateGroupRequest{
			Name: name,
		},
	)
	if err != nil {
		return err
	}
	response := r.(*rpc.CreateGroupResponse)
	if !response.Success {
		return errors.New(response.Message)
	}
	fmt.Println(response.Message)
	return nil
}

// PromoteNode promotes a node to active state
func (c *Client) PromoteNode(hostname string, ips []string) error {
	// Look up node_id from hostname
	cfg := c.GetConfig()
	nodeID, _, err := cfg.GetNodeByHostname(hostname)
	if err != nil {
		return fmt.Errorf("failed to get node_id for hostname %s: %v", hostname, err)
	}

	// Send request with only node_id
	_, err = c.CLI().Promote(context.Background(), &rpc.PromoteRequest{
		NodeId: nodeID,
		Ips:    ips,
	})
	return err
}

// GetConfig returns the current configuration
func (c *Client) GetConfig() *config.Config {
	return config.New()
}

// SetClusterMode changes the cluster operation mode
func (c *Client) SetClusterMode(mode string) error {
	// Get current config
	cfg := c.GetConfig()
	if !cfg.ClusterCheck() {
		return fmt.Errorf("no cluster configured")
	}

	// Validate mode
	if mode != "active-passive" && mode != "active-active" {
		return fmt.Errorf("invalid mode %q: must be either 'active-passive' or 'active-active'", mode)
	}

	// Send RPC request to update mode
	resp, err := c.CLI().SetMode(context.Background(), &rpc.SetModeRequest{
		Mode: mode,
	})
	if err != nil {
		return fmt.Errorf("failed to set cluster mode: %v", err)
	}

	if !resp.Success {
		return fmt.Errorf("failed to set cluster mode: %s", resp.Message)
	}

	// The server will handle updating the configuration and syncing with other nodes
	return nil
}

// AddIPToGroup adds an IP to a group
func (c *Client) AddIPToGroup(groupName, ip string) error {
	r, err := c.Send(
		SendAddIPToGroup,
		&rpc.AddIPToGroupRequest{
			GroupName: groupName,
			Ip:        ip,
		},
	)
	if err != nil {
		return err
	}
	response := r.(*rpc.AddIPToGroupResponse)

	// Display any warnings
	for _, warning := range response.Warnings {
		fmt.Printf("Warning: %s\n", warning)
	}

	if !response.Success {
		return errors.New(response.Message)
	}
	fmt.Println(response.Message)
	return nil
}

// RemoveIPFromGroup removes an IP from a group
func (c *Client) RemoveIPFromGroup(groupName, ip string) error {
	r, err := c.Send(
		SendRemoveIPFromGroup,
		&rpc.RemoveIPFromGroupRequest{
			GroupName: groupName,
			Ip:        ip,
		},
	)
	if err != nil {
		return err
	}
	response := r.(*rpc.RemoveIPFromGroupResponse)

	// Display any warnings
	for _, warning := range response.Warnings {
		fmt.Printf("Warning: %s\n", warning)
	}

	if !response.Success {
		return errors.New(response.Message)
	}
	fmt.Println(response.Message)
	return nil
}

// AssignGroupToNode assigns a group to a node's interface
func (c *Client) AssignGroupToNode(groupName, nodeID, iface string) error {
	r, err := c.Send(
		SendAssignGroupToNode,
		&rpc.AssignGroupRequest{
			GroupName: groupName,
			NodeId:    nodeID,
			Interface: iface,
		},
	)
	if err != nil {
		return err
	}
	response := r.(*rpc.AssignGroupResponse)
	if !response.Success {
		return errors.New(response.Message)
	}
	fmt.Println(response.Message)
	return nil
}

// ListGroups lists all IP groups
func (c *Client) ListGroups(jsonOutput bool) (string, []*rpc.GroupInfo, error) {
	r, err := c.Send(
		SendListGroups,
		&rpc.ListGroupsRequest{
			JsonOutput: jsonOutput,
		},
	)
	if err != nil {
		return "", nil, err
	}
	response := r.(*rpc.ListGroupsResponse)
	if !response.Success {
		return "", nil, errors.New(response.Message)
	}
	return response.JsonData, response.Groups, nil
}

// Temporary struct definitions to match server-side (until protobuf is regenerated)
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

// Helper methods for direct server calls
func (c *Client) callUnassignGroup(ctx context.Context, data interface{}) (interface{}, error) {
	// This is a simplified direct call - in practice, this would use gRPC
	// For now, returning an error to indicate not implemented via RPC
	return nil, fmt.Errorf("unassign group RPC not yet implemented - use direct method")
}

func (c *Client) callDeleteGroup(ctx context.Context, data interface{}) (interface{}, error) {
	// This is a simplified direct call - in practice, this would use gRPC
	// For now, returning an error to indicate not implemented via RPC
	return nil, fmt.Errorf("delete group RPC not yet implemented - use direct method")
}

// UnassignGroupFromNode removes a group assignment from a node's interface
func (c *Client) UnassignGroupFromNode(groupName, nodeID, iface string) error {
	// For now, implement this by directly reading and modifying the config
	// This is a temporary solution until RPC is properly implemented
	cfg := c.GetConfig()

	// Check if group exists
	if _, exists := cfg.Groups[groupName]; !exists {
		return fmt.Errorf("group %s does not exist", groupName)
	}

	// Resolve node by ID (canonical)
	node, ok := cfg.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("node_id %s not found", nodeID)
	}

	if node.IPGroups == nil {
		return fmt.Errorf("group %s is not assigned to interface %s on node %s", groupName, iface, nodeID)
	}

	// Find and remove the group from interface
	groups := node.IPGroups[iface]
	groupIndex := -1
	for i, g := range groups {
		if g == groupName {
			groupIndex = i
			break
		}
	}

	if groupIndex == -1 {
		return fmt.Errorf("group %s is not assigned to interface %s on node %s", groupName, iface, nodeID)
	}

	// Remove group from slice
	node.IPGroups[iface] = append(groups[:groupIndex], groups[groupIndex+1:]...)

	// If interface has no more groups, remove the entry
	if len(node.IPGroups[iface]) == 0 {
		delete(node.IPGroups, iface)
	}

	// Save config
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("failed to save config: %v", err)
	}

	fmt.Printf("Successfully unassigned group %s from interface %s on node %s\n", groupName, iface, nodeID)
	return nil
}

// DeleteGroup removes a group and optionally its assignments
func (c *Client) DeleteGroup(groupName string, force bool) error {
	// For now, implement this by directly reading and modifying the config
	// This is a temporary solution until RPC is properly implemented
	cfg := c.GetConfig()

	var warnings []string

	// Check if group exists
	if _, exists := cfg.Groups[groupName]; !exists {
		return fmt.Errorf("group %s does not exist", groupName)
	}

	// Check if group is assigned to any nodes (unless force is true)
	var assignedNodes []string
	for _, node := range cfg.Nodes {
		for iface, groups := range node.IPGroups {
			for _, group := range groups {
				if group == groupName {
					assignedNodes = append(assignedNodes, fmt.Sprintf("%s:%s", node.Hostname, iface))
				}
			}
		}
	}

	if len(assignedNodes) > 0 && !force {
		return fmt.Errorf("group %s is assigned to nodes: %v. Use --force to delete anyway", groupName, assignedNodes)
	}

	// If force is true and group is assigned, remove assignments and add warnings
	if len(assignedNodes) > 0 && force {
		for _, node := range cfg.Nodes {
			for iface := range node.IPGroups {
				groups := node.IPGroups[iface]
				for i := len(groups) - 1; i >= 0; i-- {
					if groups[i] == groupName {
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
	delete(cfg.Groups, groupName)

	// Save config
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("failed to save config: %v", err)
	}

	// Display warnings
	for _, warning := range warnings {
		fmt.Printf("Warning: %s\n", warning)
	}

	fmt.Printf("Successfully deleted group %s\n", groupName)
	return nil
}
