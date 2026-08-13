package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

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
	SendReadConfig
	SendSetMaintenance
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
	"ReadConfig",
	"SetMaintenance",
}

func (p ProtoFunction) String() string {
	return protoFunctions[p-1]
}

// New creates a new client connected to the daemon's CLI Unix socket.
func New() (*Client, error) {
	conn, err := grpc.NewClient(
		"unix://"+config.CLI_SOCKET_PATH,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to CLI socket %s: %v", config.CLI_SOCKET_PATH, err)
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
			return c.cliClient.UnassignGroupFromNode(ctx, data.(*rpc.UnassignGroupRequest))
		},
		"DeleteGroup": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.DeleteGroup(ctx, data.(*rpc.DeleteGroupRequest))
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
		"ReadConfig": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.ReadConfig(ctx, data.(*rpc.ReadConfigRequest))
		},
		"SetMaintenance": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.cliClient.SetMaintenance(ctx, data.(*rpc.SetMaintenanceRequest))
		},
	}
}

// Connect creates a new client connection with TLS support
func (c *Client) Connect(ip string, port string, tlsEnabled bool) error {
	var err error
	ip = utils.FormatIPv6(ip)
	previousConn := c.Connection

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
		c.Connection, err = grpc.NewClient(ip+":"+port, grpc.WithTransportCredentials(creds))
	} else {
		// Use insecure connection for non-TLS
		c.Connection, err = grpc.NewClient(ip+":"+port, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if err != nil {
		log.Error("GRPC client connection error", "error", err)
		return fmt.Errorf("could not connect to host: %v", err)
	}

	// Close previous connection after successfully dialing new target to avoid leaking sockets.
	if previousConn != nil {
		previousConn.Close()
	}

	c.server = rpc.NewServerClient(c.Connection)
	c.cliClient = rpc.NewCLIClient(c.Connection)
	log.Debug("Client:Connect() Connection made", "address", ip+":"+port)
	return nil
}

// Close terminates the client connection
func (c *Client) Close() {
	log.Debug("Client:Close() Connection closed")
	if c.Connection != nil {
		c.Connection.Close()
	}
}

// Send sends an RPC command over the client connection
func (c *Client) Send(funcName ProtoFunction, data interface{}) (interface{}, error) {
	log.Debug("Client:Send() Sending", "function", funcName.String())
	funcList := c.GetProtoFuncList()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	Members       []Member    `json:"members"`
	Groups        []GroupInfo `json:"groups"`
	Mode          string      `json:"mode"`
	ClusterHealth string      `json:"cluster_health"`
}

type Member struct {
	Hostname     string   `json:"hostname"`
	NodeID       string   `json:"node_id"`
	IP           string   `json:"ip"`
	Port         string   `json:"port"`
	Status       string   `json:"status"`
	IPs          []string `json:"ips"`
	ActiveIPs    []string `json:"active_ips"`
	LastResponse string   `json:"last_response"`
	Latency      string   `json:"latency"`
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

	// Get local hostname
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %v", err)
	}
	_ = hostname // server validates/records

	// Determine node ID (server expects a node_id); generate if not provided
	nodeID := customNodeID
	if nodeID == "" {
		nodeID = uuid.New().String()
	}

	// Default bind port if not provided
	if bindPort == "" {
		bindPort = "8080"
	}

	// Ask local daemon to initiate join with target via dedicated RPC
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	respJoin, err := c.CLI().InitiateJoin(ctx, &rpc.InitiateJoinRequest{
		TargetHost: host,
		TargetPort: port,
		Token:      token,
		BindIp:     bindIP,
		BindPort:   bindPort,
		NodeId:     nodeID,
	})
	if err != nil {
		return fmt.Errorf("join initiate failed: %v", err)
	}
	if respJoin != nil && !respJoin.Success {
		if respJoin.Message != "" {
			return errors.New(respJoin.Message)
		}
		return fmt.Errorf("join initiate failed")
	}

	// Bounded synchronous confirmation: wait up to 20s for the node to appear locally; also confirm via remote if needed
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		resp, sErr := c.CLI().Status(ctx2, &rpc.StatusRequest{})
		cancel2()
		if sErr == nil && resp != nil {
			for _, m := range resp.Members {
				if m.NodeId == nodeID {
					fmt.Printf("Successfully joined cluster: %s (%s:%s) [id=%s]\n", m.Hostname, utils.FormatIPv6(m.Ip),
						m.Port, m.NodeId)
					return nil
				}
			}
		}
		// If local daemon hasn't reflected yet, confirm membership directly from the target node
		ctx3, cancel3 := context.WithTimeout(context.Background(), 2*time.Second)
		remoteConn, dErr := grpc.Dial(host+":"+port, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if dErr == nil {
			defer remoteConn.Close()
			remoteCLI := rpc.NewCLIClient(remoteConn)
			rResp, rErr := remoteCLI.Status(ctx3, &rpc.StatusRequest{})
			if rErr == nil && rResp != nil {
				for _, m := range rResp.Members {
					if m.NodeId == nodeID {
						fmt.Printf("Joined cluster (confirmed by %s); local daemon will reflect shortly. Node ID: %s\n",
							host, nodeID)
						cancel3()
						return nil
					}
				}
			}
		}
		cancel3()
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("join did not complete within 20s; run 'pulsectl status' or check pulseha logs")
}

// LeaveCluster removes this node from the cluster
func (c *Client) LeaveCluster() error {
	// Empty NodeId indicates local node; server will resolve and handle
	_, err := c.CLI().Leave(context.Background(), &rpc.LeaveRequest{})
	if err != nil {
		return err
	}
	return nil
}

// GetClusterStatus returns the current cluster status
func (c *Client) GetClusterStatus() (*ClusterStatus, error) {
	resp, err := c.CLI().Status(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		return nil, err
	}

	status := &ClusterStatus{
		Members: make([]Member, len(resp.Members)),
		Groups:  make([]GroupInfo, 0),
		Mode:    "active-passive",
	}

	for i, m := range resp.Members {
		// Map enum to string
		statusStr := "Unknown"
		switch m.Status {
		case rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE:
			statusStr = "Active"
		case rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE:
			statusStr = "Passive"
		case rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE:
			statusStr = "Maintenance"
		case rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY:
			statusStr = "Standby"
		case rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN:
			statusStr = "Unknown"
		}
		status.Members[i] = Member{
			Hostname:     m.Hostname,
			Status:       statusStr,
			IPs:          m.ActiveIps,
			ActiveIPs:    m.ActiveIps,
			LastResponse: m.LastResponse,
			Latency:      m.Latency,
			IP:           m.Ip,
			Port:         m.Port,
		}

		// Log the latency for debugging
		log.Debug("Member latency from RPC", "hostname", m.Hostname, "latency", m.Latency)
	}

	// Populate groups from server response (authoritative)
	for _, g := range resp.Groups {
		group := GroupInfo{
			Name:        g.Name,
			IPs:         g.Ips,
			Assignments: make([]GroupAssignment, 0, len(g.Assignments)),
		}
		for _, a := range g.Assignments {
			group.Assignments = append(group.Assignments, GroupAssignment{NodeID: a.NodeId, Interface: a.Interface})
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
	// Resolve node_id via authoritative Status RPC rather than local file
	resp, err := c.CLI().Status(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		return fmt.Errorf("failed to get cluster status: %v", err)
	}
	var nodeID string
	for _, m := range resp.Members {
		if m.Hostname == hostname {
			nodeID = m.NodeId
			break
		}
	}
	if nodeID == "" {
		return fmt.Errorf("failed to resolve node_id for hostname %s", hostname)
	}
	_, err = c.CLI().Promote(context.Background(), &rpc.PromoteRequest{
		NodeId: nodeID,
		Ips:    ips,
	})
	return err
}

// GetConfig returns the daemon's live configuration via gRPC.
func (c *Client) GetConfig() (*config.Config, error) {
	r, err := c.Send(SendReadConfig, &rpc.ReadConfigRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %v", err)
	}
	resp := r.(*rpc.ReadConfigResponse)
	if !resp.Success {
		return nil, fmt.Errorf("failed to read config: %s", resp.Message)
	}
	var cfg config.Config
	if err := json.Unmarshal(resp.Config, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %v", err)
	}
	return &cfg, nil
}

// SetClusterMode changes the cluster operation mode
func (c *Client) SetClusterMode(mode string) error {
	resp, err := c.CLI().SetMode(context.Background(), &rpc.SetModeRequest{Mode: mode})
	if err != nil {
		return fmt.Errorf("failed to set cluster mode: %v", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to set cluster mode: %s", resp.Message)
	}
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

// UnassignGroupFromNode removes a group assignment from a node's interface
func (c *Client) UnassignGroupFromNode(groupName, nodeID, iface string) error {
	_, err := c.CLI().UnassignGroupFromNode(context.Background(), &rpc.UnassignGroupRequest{
		GroupName: groupName,
		NodeId:    nodeID,
		Interface: iface,
	})
	return err
}

// DeleteGroup removes a group and optionally its assignments
func (c *Client) DeleteGroup(groupName string, force bool) error {
	resp, err := c.CLI().DeleteGroup(context.Background(), &rpc.DeleteGroupRequest{
		GroupName: groupName,
		Force:     force,
	})
	if err != nil {
		return err
	}

	for _, w := range resp.Warnings {
		fmt.Printf("Warning: %s\n", w)
	}

	if !resp.Success {
		return errors.New(resp.Message)
	}

	fmt.Printf("%s\n", resp.Message)
	return nil
}
