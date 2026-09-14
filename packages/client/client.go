// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strings"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/rpc"
)

type Client struct {
	*client.Client
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
}

func (p ProtoFunction) String() string {
	return protoFunctions[p-1]
}

// Node represents a cluster node
type Node struct {
	Hostname string
	IP       string
	Port     string
}

// New creates a new instance of our Client
func New() (*Client, error) {
	internalClient, err := client.New()
	if err != nil {
		return nil, err
	}
	return &Client{Client: internalClient}, nil
}

// GetProtoFuncList defines the available RPC commands to send.
func (c *Client) GetProtoFuncList() map[string]interface{} {
	return map[string]interface{}{
		"ConfigSync": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().ConfigSync(ctx, data.(*rpc.ConfigSyncRequest))
		},
		"Join": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.CLI().Join(ctx, data.(*rpc.JoinRequest))
		},
		"Leave": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.CLI().Leave(ctx, data.(*rpc.LeaveRequest))
		},
		"MakePassive": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().MakePassive(ctx, data.(*rpc.MakePassiveRequest))
		},
		"BringUpIP": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().BringUpIP(ctx, data.(*rpc.UpIpRequest))
		},
		"BringDownIP": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().BringDownIP(ctx, data.(*rpc.DownIpRequest))
		},
		"HealthCheck": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().HealthCheck(ctx, data.(*rpc.HealthCheckRequest))
		},
		"Promote": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.CLI().Promote(ctx, data.(*rpc.PromoteRequest))
		},
		"Logs": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().Logs(ctx, data.(*rpc.LogsRequest))
		},
		"Remove": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Server().Remove(ctx, data.(*rpc.RemoveRequest))
		},
	}
}

// Connect creates a new client connection, over TLS when the caller supplies a
// configuration. See internal/client.Connect for why the decision is the
// caller's to make.
func (c *Client) Connect(ip string, port string, tlsConfig *tls.Config) error {
	return c.Client.Connect(ip, port, tlsConfig)
}

// GetLocalNode returns the local node configuration
func (c *Client) GetLocalNode() (*Node, error) {
	cfg, err := c.Client.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %v", err)
	}
	if !cfg.ClusterCheck() {
		return nil, fmt.Errorf("no cluster configured")
	}

	localNode, err := cfg.GetLocalNode()
	if err != nil {
		return nil, err
	}

	return &Node{
		Hostname: localNode.Hostname,
		IP:       localNode.IP,
		Port:     localNode.Port,
	}, nil
}

// Close terminates the client connection.
func (c *Client) Close() {
	c.Client.Close()
}

// Send sends an RPC command over the client connection
func (c *Client) Send(funcName ProtoFunction, data interface{}) (interface{}, error) {
	return c.Client.Send(client.ProtoFunction(funcName), data)
}

// Add method to expose CLI client
func (c *Client) CLI() rpc.CLIClient {
	return c.Client.CLI()
}

// CreateCluster creates a new cluster with the given bind IP and port
func (c *Client) CreateCluster(bindIP, bindPort, mode string) error {
	return c.Client.CreateCluster(bindIP, bindPort, mode)
}

// GetVotingSessions retrieves a list of voting sessions
func (c *Client) GetVotingSessions(includeCompleted bool) (*rpc.GetVotingSessionsResponse, error) {
	resp, err := c.CLI().GetVotingSessions(context.Background(), &rpc.GetVotingSessionsRequest{
		IncludeCompleted: includeCompleted,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetVotingSessionDetails retrieves details of a specific voting session
func (c *Client) GetVotingSessionDetails(sessionID string) (*rpc.GetVotingSessionDetailsResponse, error) {
	resp, err := c.CLI().GetVotingSessionDetails(context.Background(), &rpc.GetVotingSessionDetailsRequest{
		SessionId: sessionID,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CastVote casts a vote in a voting session
func (c *Client) CastVote(sessionID string, voterID string, decision rpc.VoteDecision) (*rpc.CastVoteResponse, error) {
	resp, err := c.Server().CastVote(context.Background(), &rpc.CastVoteRequest{
		SessionId: sessionID,
		VoterId:   voterID,
		Decision:  decision,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// JoinCluster joins an existing cluster
func (c *Client) JoinCluster(address, token, bindIP, bindPort string) error {
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
	if err := c.Connect(host, port, nil); err != nil {
		return fmt.Errorf("failed to connect to target node: %v", err)
	}

	// Get local hostname
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %v", err)
	}

	// Create join request (server handles all authoritative logic)
	joinReq := &rpc.JoinRequest{
		Hostname: hostname,
		Token:    token,
		BindIp:   bindIP,
		BindPort: bindPort,
	}

	_, err = c.CLI().Join(context.Background(), joinReq)
	return err
}
