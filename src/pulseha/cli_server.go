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

package pulseha

import (
	"context"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/language"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// CLIServer CLI server object
type CLIServer struct {
	sync.Mutex
	Server   *Server
	Listener net.Listener
}

// Setup is used to bootstrap the cli server.
func (s *CLIServer) Setup() {
	log.Info("CLI server initialised on 127.0.0.1:49152")
	lis, err := net.Listen("tcp", "127.0.0.1:49152")
	if err != nil {
		log.Errorf("Failed to listen: %s", err)
		// TODO: Note: We exit because the service is useless without the CLI server running
		os.Exit(0)
	}
	grpcServer := grpc.NewServer()
	rpc.RegisterCLIServer(grpcServer, s)
	grpcServer.Serve(lis)
}

// Join command is used to join an already established PulseHA cluster.
func (s *CLIServer) Join(ctx context.Context, in *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	log.Debug("CLIServer:Join() Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		// Validate our IP & Port
		i, _ := strconv.Atoi(in.BindPort)
		if i == 0 && i > 65535 {
			return &rpc.JoinResponse{
				Success: false,
				Message: "Invalid port range",
				ErrorCode: 9,
			}, nil
		}
		// Create a new client
		c := &client.Client{}
		// Attempt to connect
		err := c.Connect(in.Ip, in.Port, false)
		// Handle a client connection error
		if err != nil {
			return &rpc.JoinResponse{
				Success: false,
				Message: err.Error(),
				ErrorCode: 0,
			}, nil
		}
		// Check to see if we have a local node definition
		localHostname, err := utils.GetHostname()
		if err != nil {
			return &rpc.JoinResponse{
				Success: false,
				Message: err.Error(),
				ErrorCode: 10,
			}, nil
		}
		uid, node, err := nodeGetByHostname(localHostname)
		var newNode *config.Node
		// If our local node doesn't exist.. create a new local definition.
		if err != nil {
			// Create new local node config to send
			uid, newNode, err = nodeCreateLocal(in.BindIp, in.BindPort, false)
			if err != nil {
				log.Errorf("Join() Unable to generate local node definition: %s", err)
				return &rpc.JoinResponse{
					Success: false,
					Message: "Join failure. Unable to generate local node definition",
					ErrorCode: 1,
				}, nil
			}
		} else {
			// Set our
			newNode = &node
		}
		// Convert struct into byte array
		buf, err := json.Marshal(newNode)
		// Handle failure to marshal config
		if err != nil {
			log.Errorf("Join() Unable to marshal config: %s", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: "Join failure. Please check the logs for more information",
				ErrorCode: 2,
			}, nil
		}
		r, err := c.Send(client.SendJoin, &rpc.JoinRequest{
			Config:   buf,
			Uid: uid,
			Token: in.Token,
			ErrorCode: 3,
		})
		// Handle a failed request
		if err != nil {
			log.Errorf("Join() Request error: %s", err)
			// Return
			return &rpc.JoinResponse{
				Success: false,
				Message: "Join failure. Unable to connect to host.",
				ErrorCode: 4,
			}, nil
		}
		// Handle an unsuccessful request
		if !r.(*rpc.JoinResponse).Success {
			log.Errorf("Join() Peer error: %s", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: r.(*rpc.JoinResponse).Message,
				ErrorCode: 5,
			}, nil
		}
		// write CA keys
		utils.CreateFolder(security.CertDir)
		security.WriteCertFile("ca", []byte(r.(*rpc.JoinResponse).CaCrt))
		security.WriteKeyFile("ca", []byte(r.(*rpc.JoinResponse).CaKey))
		// Generate our new keys
		if err := security.GenTLSKeys(in.BindIp); err != nil {
			log.Errorf("Join() Unable to generate TLS keys: %s", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: err.Error(),
				ErrorCode: 6,
			}, nil
		}
		// Update our local config
		peerConfig := &config.Config{}
		err = json.Unmarshal(r.(*rpc.JoinResponse).Config, peerConfig)
		// handle errors
		if err != nil {
			log.Error("Unable to unmarshal config node.")
			return &rpc.JoinResponse{
				Success: false,
				Message: "Unable to unmarshal config node.",
				ErrorCode: 7,
			}, nil
		}
		// !!!IMPORTANT!!!: Do not replace our local config
		peerConfig.Pulse = DB.Config.Pulse
		// Set the config
		DB.SetConfig(peerConfig)
		// Save the config
		if err := DB.Config.Save(); err != nil {
			return &rpc.JoinResponse{
				Success: false,
				Message: "Failed to write config. Joined failed.",
				ErrorCode: 8,
			}, nil
		}
		// Reload config in memory
		DB.Config.Reload()
		// Setup our daemon server
		go s.Server.Setup()
		// reset our HC last received time
		localMember, _ := DB.MemberList.GetLocalMember()
		localMember.SetLastHCResponse(time.Now())
		// Close the connection
		c.Close()
		log.Info("Successfully joined cluster with " + in.Ip)
		return &rpc.JoinResponse{
			Success: true,
			Message: "Successfully joined cluster",
		}, nil
	}
	return &rpc.JoinResponse{
		Success: false,
		Message: "Unable to join as PulseHA is already in a cluster.",
		ErrorCode: 9,
	}, nil
}

// Leave command is used to leave from the configured PulseHA cluster.
// TODO: Remember to reassign active role on leave.
func (s *CLIServer) Leave(ctx context.Context, in *rpc.LeaveRequest) (*rpc.LeaveResponse, error) {
	log.Debug("CLIServer:Leave() - Leave Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.LeaveResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// Check to see if we are not the only one in the "cluster"
	// Let everyone else know that we are leaving the cluster
	node, err := DB.Config.GetLocalNode()
	if err != nil {
		return &rpc.LeaveResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 3,
		}, nil
	}
	if DB.Config.NodeCount() > 1 {
		DB.MemberList.Broadcast(
			client.SendLeave,
			&rpc.LeaveRequest{
				Replicated: true,
				Hostname:   node.Hostname, // TODO: Change this to UUID
			},
		)
	}
	// Shutdown daemon server
	// Note: Shutdown makes ourselves passive and reset our memberlist
	s.Server.Shutdown()
	// Clear our config
	nodesClearLocal()
	groupClearLocal()
	// save
	if err := DB.Config.Save(); err != nil {
		return &rpc.LeaveResponse{
			Success: false,
			Message: "PulseHA successfully removed from cluster but could not update local config",
			ErrorCode: 2,
		}, nil
	}
	// Remove our generated keys
	if !utils.DeleteFolder(security.CertDir) {
		log.Warn("Failed to remove certs directory. Please manually remove hanging certs.")
	}
	// yay?
	log.Info("Successfully left configured cluster. PulseHA no longer listening..")
	if DB.Config.NodeCount() == 1 {
		return &rpc.LeaveResponse{
			Success: true,
			Message: "Successfully dismantled cluster",
		}, nil
	}
	return &rpc.LeaveResponse{
		Success: true,
		Message: "Successfully left from cluster",
	}, nil
}

// Remove command is used to remove a node from a PulseHA cluster.
func (s *CLIServer) Remove(ctx context.Context, in *rpc.RemoveRequest) (*rpc.RemoveResponse, error) {
	log.Debug("CLIServer:Leave() - Remove node from Pulse cluster")
	s.Lock()
	defer s.Unlock()
	// make sure we are in a cluster
	if !DB.Config.ClusterCheck() {
		return &rpc.RemoveResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// make sure we are not removing our active node
	activeHostname, _ := DB.MemberList.GetActiveMember()
	if in.Hostname == activeHostname {
		return &rpc.RemoveResponse{
			Success: false,
			Message: "Unable to remove active node. Please promote another node and try again",
			ErrorCode: 2,
		}, nil
	}
	// Tell everyone else to do the same
	// Note: This must come first otherwise the node we remove later wont be included in the request
	if DB.Config.NodeCount() > 1 {
		DB.MemberList.Broadcast(
			client.SendRemove,
			&rpc.RemoveRequest{
				Replicated: true,
				Hostname:   in.Hostname,
			},
		)
	}
	// Get our local node
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		return &rpc.RemoveResponse{
			Success: false,
			Message: "Unable to retrieve local node from configuration",
			ErrorCode: 5,
		}, nil
	}
	// Get our node we are removing
	uid, _, err := DB.Config.GetNodeByHostname(in.Hostname)
	if err != nil {
		return &rpc.RemoveResponse{
			Success: false,
			Message: "Unable to retrieve " + in.Hostname + " from local configuration",
			ErrorCode: 3,
		}, nil
	}
	// Set our member status
	member := DB.MemberList.GetMemberByHostname(in.Hostname)
	member.SetStatus(rpc.MemberStatus_LEAVING)
	// Check if I am the node being removed
	if in.Hostname == localNode.Hostname {
		s.Server.Shutdown()
		nodesClearLocal()
		groupClearLocal()
		log.Info("Successfully removed " + in.Hostname + " from cluster. PulseHA no longer listening..")
	} else {
		// Remove from our memberlist
		DB.MemberList.MemberRemoveByHostname(in.Hostname)
		// Remove from our config
		err := nodeDelete(uid)
		if err != nil {
			return &rpc.RemoveResponse{
				Success: false,
				Message: err.Error(),
				ErrorCode: 4,
			}, nil
		}
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	return &rpc.RemoveResponse{
		Success: true,
		Message: "Successfully left from cluster",
	}, nil
}

// Create command is used to create a new PulseHA cluster.
func (s *CLIServer) Create(ctx context.Context, in *rpc.CreateRequest) (*rpc.CreateResponse, error) {
	var token string
	s.Lock()
	defer s.Unlock()
	// Make sure we are not in a cluster before creating one.
	if !DB.Config.ClusterCheck() {
		// Validate our IP & Port
		i, _ := strconv.Atoi(in.BindPort)
		if i == 0 && i > 65535 {
			return &rpc.CreateResponse{
				Success: false,
				Message: "Invalid port range",
				Token: token,
				ErrorCode: 3,
			}, nil
		}
		// Remove any hanging nodes
		nodesClearLocal()
		groupClearLocal()
		// Create a new local node config
		_, _, err := nodeCreateLocal(in.BindIp, in.BindPort, true)
		if err != nil {
			return &rpc.CreateResponse{
				Success: false,
				Message: "Failed write local not to config",
				Token: token,
				ErrorCode: 1,
			}, nil
		}
		// Generate new token
		token = generateRandomString(20)
		// Create a new hasher for sha 256
		token_hash := security.GenerateSHA256Hash(token)
		// Set our token in our config
		DB.Config.Pulse.ClusterToken = token_hash
		// Save back to our config
		if err := DB.Config.Save(); err != nil {
			panic(err)
		}
		// Cert stuff
		security.GenerateCACert(in.BindIp)
		// Generate client server keys if tls is enabled
		if err := security.GenTLSKeys(in.BindIp); err != nil {
			panic(err)
		}
		// Setup our pulse server
		go s.Server.Setup()
		// Save our newly generated token
		return &rpc.CreateResponse{
			Success: true,
			Message: `Pulse cluster successfully created!
	
You can now join any number of machines by running the following on each node:

pulsectl join -bind-ip=<IP_ADDRESS> -bind-port=<PORT> -token=` + token + ` ` + in.BindIp + ` ` + in.BindPort + `
			`,
		}, nil
	} else {
		return &rpc.CreateResponse{
			Success: false,
			Message: "Pulse daemon is already in a configured cluster",
			Token: token,
			ErrorCode: 2,
		}, nil
	}
}

// NewGroup command is used to create a new floating IP group.
func (s *CLIServer) NewGroup(ctx context.Context, in *rpc.GroupNewRequest) (*rpc.GroupNewResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupNewResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	groupName, err := groupNew(in.Name)
	if err != nil {
		return &rpc.GroupNewResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.GroupNewResponse{
		Success: true,
		Message: groupName + " successfully added.",
	}, nil
}

// DeleteGroup command is used to delete a floating IP group.
func (s *CLIServer) DeleteGroup(ctx context.Context, in *rpc.GroupDeleteRequest) (*rpc.GroupDeleteResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupDeleteResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := groupDelete(in.Name)
	if err != nil {
		return &rpc.GroupDeleteResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.GroupDeleteResponse{
		Success: true,
		Message: in.Name + " successfully deleted.",
	}, nil
}

// GroupIPAdd command is used to add a floating ip to an ip group.
func (s *CLIServer) GroupIPAdd(ctx context.Context, in *rpc.GroupAddRequest) (*rpc.GroupAddResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupAddResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := groupIpAdd(in.Name, in.Ips)
	if err != nil {
		return &rpc.GroupAddResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	if err := DB.MemberList.SyncConfig(); err != nil {
		return &rpc.GroupAddResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 5,
		}, nil
	}
	// bring up the ip on the active appliance
	activeHostname, activeMember := DB.MemberList.GetActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	if err := activeMember.Connect(); err != nil {
		return &rpc.GroupAddResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 3,
		}, nil
	}
	iface, err := DB.Config.GetGroupIface(activeHostname, in.Name)
	if err != nil {
		return &rpc.GroupAddResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 6,
		}, nil
	}
	_, err = activeMember.Send(client.SendBringUpIP, &rpc.UpIpRequest{
		Iface: iface,
		Ips:   in.Ips,
	})
	if err != nil {
		return &rpc.GroupAddResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 4,
		}, nil
	}
	// respond
	return &rpc.GroupAddResponse{
		Success: true,
		Message: "IP address(es) successfully added to " + in.Name,
	}, nil
}

// GroupIPRemove command is used to remove a floating ip from a ip group.
func (s *CLIServer) GroupIPRemove(ctx context.Context, in *rpc.GroupRemoveRequest) (*rpc.GroupRemoveResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupRemoveResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// TODO: Note: Validation! IMPORTANT otherwise someone could DOS by seg faulting.
	if in.Ips == nil || in.Name == "" {
		return &rpc.GroupRemoveResponse{
			Success: false,
			Message: "Unable to process RPC call. Required parameters: Ips, Name",
			ErrorCode: 2,
		}, nil
	}
	_, activeMember := DB.MemberList.GetActiveMember()
	if activeMember == nil {
		return &rpc.GroupRemoveResponse{
			Success: false,
			Message: "Unable to remove IP(s) to group as there no active node in the cluster.",
			ErrorCode: 3,
		}, nil
	}
	err := groupIpRemove(in.Name, in.Ips)
	if err != nil {
		return &rpc.GroupRemoveResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 4,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	// bring down the ip on the active appliance
	activeHostname, activeMember := DB.MemberList.GetActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	activeMember.Connect()
	iface, err := DB.Config.GetGroupIface(activeHostname, in.Name)
	if err != nil {
		return &rpc.GroupRemoveResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 5,
		}, nil
	}
	activeMember.Send(client.SendBringDownIP, &rpc.DownIpRequest{
		Iface: iface,
		Ips:   in.Ips,
	})
	return &rpc.GroupRemoveResponse{
		Success: true,
		Message: "IP address(es) successfully removed from " + in.Name,
	}, nil
}

// GroupAssign command is used to assign a floating ip group to a network interface
// on the current node.
func (s *CLIServer) GroupAssign(ctx context.Context, in *rpc.GroupAssignRequest) (*rpc.GroupAssignResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupAssignResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	uid, _, err := nodeGetByHostname(in.Node)
	if err != nil {
		return &rpc.GroupAssignResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := groupAssign(in.Group, uid, in.Interface); err != nil {
		return &rpc.GroupAssignResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 3,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.GroupAssignResponse{
		Success: true,
		Message: in.Group + " assigned to interface " + in.Interface + " on node " + in.Node,
	}, nil
}

// GroupUnassign command is used to unassign a floating ip group from a network interface.
// on the current node.
func (s *CLIServer) GroupUnassign(ctx context.Context, in *rpc.GroupUnassignRequest) (*rpc.GroupUnassignResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupUnassignResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	uid, _, err := nodeGetByHostname(in.Node)
	if err != nil {
		return &rpc.GroupUnassignResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := groupUnassign(in.Group, uid, in.Interface); err != nil {
		return &rpc.GroupUnassignResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 3,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.GroupUnassignResponse{
		Success: true,
		Message: in.Group + " unassigned from interface " + in.Interface + " on node " + in.Node,
	}, nil
}

// GroupList command is used to list the available floating ip groups on the current node.
func (s *CLIServer) GroupList(ctx context.Context, in *rpc.GroupTableRequest) (*rpc.GroupTableResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupTableResponse{
			Success: false,
			Message: language.CLUSTER_REQUIRED_MESSAGE,
		}, nil
	}
	table := new(rpc.GroupTableResponse)
	for name, ips := range DB.Config.Groups {
		nodes, interfaces := getGroupNodes(name)
		row := &rpc.GroupRow{Name: name, Ip: ips, Nodes: nodes, Interfaces: interfaces}
		table.Row = append(table.Row, row)
	}
	return table, nil
}

// Status command is used to return an object of node statuses
func (s *CLIServer) Status(ctx context.Context, in *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.StatusResponse{
			Success: false,
			Message: language.CLUSTER_REQUIRED_MESSAGE,
		}, nil
	}
	table := new(rpc.StatusResponse)
	for _, member := range DB.MemberList.Members {
		_, node, _ := nodeGetByHostname(member.Hostname)
		tym := member.GetLastHCResponse()
		var tymFormat string
		if tym == (time.Time{}) {
			tymFormat = ""
		} else {
			tymFormat = tym.Format(time.RFC1123)
		}
		row := &rpc.StatusRow{
			Hostname:     member.GetHostname(),
			Ip:           node.IP,
			Latency:      member.GetLatency(),
			Status:       member.GetStatus(),
			LastReceived: tymFormat,
			Score: int32(member.GetScore()),
		}
		table.Row = append(table.Row, row)
	}
	table.Success = true
	return table, nil
}

// Promote command is used to make a particular node active
func (s *CLIServer) Promote(ctx context.Context, in *rpc.PromoteRequest) (*rpc.PromoteResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PromoteResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := DB.MemberList.PromoteMember(in.Member)
	if err != nil {
		return &rpc.PromoteResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	return &rpc.PromoteResponse{
		Success: true,
		Message: "Successfully promoted member " + in.Member,
	}, nil
}

// TLS command is used to generate and manage tls keys.
func (s *CLIServer) TLS(ctx context.Context, in *rpc.CertRequest) (*rpc.CertResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.CertResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := security.GenTLSKeys(in.BindIp)
	if err != nil {
		return &rpc.CertResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	return &rpc.CertResponse{
		Success: true,
		Message: "Successfully generated new TLS certificates",
	}, nil
}

// Config command is used to update any key value in the pulseha config.
func (s *CLIServer) Config(ctx context.Context, in *rpc.ConfigRequest) (*rpc.ConfigResponse, error) {
	s.Lock()
	defer s.Unlock()
	// Validation
	if in.Key == "local_node" ||
		in.Key == "cluster_token" {
		return &rpc.ConfigResponse{
			Success: false,
			Message: "You are unable to use the config command to change the value of " + in.Key,
			ErrorCode: 4,
		}, nil
	}
	// If the value is hostname, update our node in our nodes section as well
	if in.Key == "hostname" {
		if err := nodeUpdateLocalHostname(in.Value); err != nil {
			return &rpc.ConfigResponse{
				Success: false,
				Message: err.Error(),
				ErrorCode: 3,
			}, nil
		}
		return &rpc.ConfigResponse{
			Success: true,
			Message: "Successfully updated PulseHA config",
		}, nil
	}
	// Update our key value
	if err := DB.Config.UpdateValue(in.Key, in.Value); err != nil {
		return &rpc.ConfigResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
		return &rpc.ConfigResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 5,
		}, nil
	}
	// Reload our config
	DB.Config.Reload()
	// Sync it with our peers
	if err := DB.MemberList.SyncConfig(); err != nil {
		return &rpc.ConfigResponse{
			Success: false,
			Message: err.Error(),
			ErrorCode: 6,
		}, nil
	}
	return &rpc.ConfigResponse{
		Success: true,
		Message: "Successfully updated PulseHA config",
	}, nil
}

// Token - Generate a new cluster token
func (s *CLIServer) Token(ctx context.Context, in *rpc.TokenRequest) (*rpc.TokenResponse, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.TokenResponse{
			Success: false,
			Message: language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// Generate new token
	token := generateRandomString(20)
	// Create a new hasher for sha 256
	token_hash := security.GenerateSHA256Hash(token)
	// Set our token in our config
	DB.Config.Pulse.ClusterToken = token_hash
	// Sync our config with the cluster
	if err := DB.MemberList.SyncConfig(); err != nil {
		return &rpc.TokenResponse{
			Success:   false,
			Message:   language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 2,
		}, nil
	}
	// Save back to our config
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	// report back with success
	return &rpc.TokenResponse{
		Success: true,
		Message: "Success! Your new cluster token is " + token,
		Token: token,
	}, nil
}

// Network command to make changes to the node networking.
func (s *CLIServer) Network(ctx context.Context, in *rpc.PulseNetwork) (*rpc.PulseNetwork, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseNetwork{
			Success: false,
			Message: language.CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	switch(in.Action) {
	case "resync":
		if err := nodeUpdateLocalInterfaces(); err != nil {
			return &rpc.PulseNetwork{
				Success: false,
				Message: err.Error(),
				ErrorCode: 2,
			}, nil
		}
		break
	default:
		break
	}
	return &rpc.PulseNetwork{
		Success: true,
		Message: "Success! PulseHA config has been sync'd with local network interfaces",
	}, nil
}

// Describe command to return current node details.
func (s *CLIServer) Describe(ctx context.Context, in *rpc.DescribeRequest) (*rpc.DescribeResponse, error) {
	return &rpc.DescribeResponse{}, nil
}
