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
	"errors"
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/packages/config"
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

var (
	CLUSTER_REQUIRED_MESSAGE = "You must be in a configured cluster to complete this action."
)

/**
Server struct type
*/
type CLIServer struct {
	sync.Mutex
	Server   *Server
	Listener net.Listener
}

/**
Setup pulse cli type
*/
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

/**
Attempt to join a configured cluster
Notes: We create a new client in attempt to communicate with our peer.
       If successful we acknowledge it and update our memberlist.
*/
func (s *CLIServer) Join(ctx context.Context, in *rpc.PulseJoin) (*rpc.PulseJoin, error) {
	//log.Debug("CLIServer:Join() Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		// Validate our IP & Port
		i, _ := strconv.Atoi(in.BindPort)
		if i == 0 && i > 65535 {
			return &rpc.PulseJoin{
				Success: false,
				Message: "Invalid port range",
				ErrorCode: 9,
			}, nil
		}
		// Create a new client
		c := &client.Client{}
		// Attempt to connect
		err := c.Connect(in.Ip, in.Port, in.Hostname, false)
		// Handle a client connection error
		if err != nil {
			return &rpc.PulseJoin{
				Success: false,
				Message: err.Error(),
				ErrorCode: 0,
			}, nil
		}
		// Create new local node config to send
		uid, newNode, err := nodeCreateLocal(in.BindIp, in.BindPort, false)
		if err != nil {
			log.Errorf("Join() Unable to generate local node definition: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: "Join failure. Unable to generate local node definition",
				ErrorCode: 1,
			}, nil
		}
		// Convert struct into byte array
		buf, err := json.Marshal(newNode)
		// Handle failure to marshal config
		if err != nil {
			log.Errorf("Join() Unable to marshal config: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: "Join failure. Please check the logs for more information",
				ErrorCode: 2,
			}, nil
		}
		// Send our join request
		hostname, err := utils.GetHostname()
		if err != nil {
			return nil, errors.New("cannot to join because unable to get hostname")
		}
		r, err := c.Send(client.SendJoin, &rpc.PulseJoin{
			Config:   buf,
			Hostname: hostname,
			Uid: uid,
			Token: in.Token,
			ErrorCode: 3,
		})
		// Handle a failed request
		if err != nil {
			log.Errorf("Join() Request error: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: "Join failure. Unable to connect to host.",
				ErrorCode: 4,
			}, nil
		}
		// Handle an unsuccessful request
		if !r.(*rpc.PulseJoin).Success {
			log.Errorf("Join() Peer error: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: r.(*rpc.PulseJoin).Message,
				ErrorCode: 5,
			}, nil
		}
		// write CA keys
		utils.CreateFolder(security.CertDir)
		security.WriteCertFile("ca", []byte(r.(*rpc.PulseJoin).CaCrt))
		security.WriteKeyFile("ca", []byte(r.(*rpc.PulseJoin).CaKey))
		// Generate our new keys
		if err := security.GenTLSKeys(in.BindIp); err != nil {
			log.Errorf("Join() Unable to generate TLS keys: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: err.Error(),
				ErrorCode: 6,
			}, nil
		}
		// Update our local config
		peerConfig := &config.Config{}
		err = json.Unmarshal(r.(*rpc.PulseJoin).Config, peerConfig)
		// handle errors
		if err != nil {
			log.Error("Unable to unmarshal config node.")
			return &rpc.PulseJoin{
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
			return &rpc.PulseJoin{
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
		return &rpc.PulseJoin{
			Success: true,
			Message: "Successfully joined cluster",
		}, nil
	}
	return &rpc.PulseJoin{
		Success: false,
		Message: "Unable to join as PulseHA is already in a cluster.",
		ErrorCode: 9,
	}, nil
}

/**
Break cluster / Leave from cluster
TODO: Remember to reassign active role on leave
*/
func (s *CLIServer) Leave(ctx context.Context, in *rpc.PulseLeave) (*rpc.PulseLeave, error) {
	log.Debug("CLIServer:Leave() - Leave Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseLeave{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// Check to see if we are not the only one in the "cluster"
	// Let everyone else know that we are leaving the cluster
	node := DB.Config.GetLocalNode()
	if DB.Config.NodeCount() > 1 {
		DB.MemberList.Broadcast(
			client.SendLeave,
			&rpc.PulseLeave{
				Replicated: true,
				Hostname:   node.Hostname,
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
		return &rpc.PulseLeave{
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
		return &rpc.PulseLeave{
			Success: true,
			Message: "Successfully dismantled cluster",
		}, nil
	}
	return &rpc.PulseLeave{
		Success: true,
		Message: "Successfully left from cluster",
	}, nil
}

// Remove - Remove node from cluster by hostname
func (s *CLIServer) Remove(ctx context.Context, in *rpc.PulseRemove) (*rpc.PulseRemove, error) {
	log.Debug("CLIServer:Leave() - Remove node from Pulse cluster")
	s.Lock()
	defer s.Unlock()
	// make sure we are in a cluster
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseRemove{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// make sure we are not removing our active node
	activeHostname, _ := DB.MemberList.GetActiveMember()
	if in.Hostname == activeHostname {
		return &rpc.PulseRemove{
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
			&rpc.PulseRemove{
				Replicated: true,
				Hostname:   in.Hostname,
			},
		)
	}
	// Get our local node
	localNode := DB.Config.GetLocalNode()
	// Get our node we are removing
	uid, _, err := DB.Config.GetNodeByHostname(in.Hostname)
	if err != nil {
		return &rpc.PulseRemove{
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
			return &rpc.PulseRemove{
				Success: false,
				Message: err.Error(),
				ErrorCode: 4,
			}, nil
		}
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	return &rpc.PulseRemove{
		Success: true,
		Message: "Successfully left from cluster",
	}, nil
}

/**
Create new PulseHA cluster
*/
func (s *CLIServer) Create(ctx context.Context, in *rpc.PulseCreate) (*rpc.PulseCreate, error) {
	var token string
	s.Lock()
	defer s.Unlock()
	// Make sure we are not in a cluster before creating one.
	if !DB.Config.ClusterCheck() {
		// Validate our IP & Port
		i, _ := strconv.Atoi(in.BindPort)
		if i == 0 && i > 65535 {
			return &rpc.PulseCreate{
				Success: false,
				Message: "Invalid port range",
				Token: token,
				ErrorCode: 3,
			}, nil
		}
		// Get our local hostname
		hostname, err := utils.GetHostname()
		if err != nil {
			panic(err)
		}
		// Remove any hanging nodes
		nodesClearLocal()
		groupClearLocal()
		// Create a new local node config
		_, _, err = nodeCreateLocal(in.BindIp, in.BindPort, true)
		if err != nil {
			return &rpc.PulseCreate{
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
		return &rpc.PulseCreate{
			Success: true,
			Message: `Pulse cluster successfully created!
	
You can now join any number of machines by running the following on each node:

pulsectl join -bind-ip=<IP_ADDRESS> -bind-port=<PORT> -token=` + token + ` ` + in.BindIp + ` ` + in.BindPort + ` ` + hostname + `
			`,
		}, nil
	} else {
		return &rpc.PulseCreate{
			Success: false,
			Message: "Pulse daemon is already in a configured cluster",
			Token: token,
			ErrorCode: 2,
		}, nil
	}
}

/**
Add a new floating IP group
*/
func (s *CLIServer) NewGroup(ctx context.Context, in *rpc.PulseGroupNew) (*rpc.PulseGroupNew, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseGroupNew{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	groupName, err := groupNew(in.Name)
	if err != nil {
		return &rpc.PulseGroupNew{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.PulseGroupNew{
		Success: true,
		Message: groupName + " successfully added.",
	}, nil
}

/**
Delete floating IP group
*/
func (s *CLIServer) DeleteGroup(ctx context.Context, in *rpc.PulseGroupDelete) (*rpc.PulseGroupDelete, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseGroupDelete{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := groupDelete(in.Name)
	if err != nil {
		return &rpc.PulseGroupDelete{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.PulseGroupDelete{
		Success: true,
		Message: in.Name + " successfully deleted.",
	}, nil
}

/**
Add IP to group
*/
func (s *CLIServer) GroupIPAdd(ctx context.Context, in *rpc.PulseGroupAdd) (*rpc.PulseGroupAdd, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseGroupAdd{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := groupIpAdd(in.Name, in.Ips)
	if err != nil {
		return &rpc.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	if err := DB.MemberList.SyncConfig(); err != nil {
		return &rpc.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
			ErrorCode: 5,
		}, nil
	}
	// bring up the ip on the active appliance
	activeHostname, activeMember := DB.MemberList.GetActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	if err := activeMember.Connect(); err != nil {
		return &rpc.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
			ErrorCode: 3,
		}, nil
	}
	iface, err := DB.Config.GetGroupIface(activeHostname, in.Name)
	if err != nil {
		return &rpc.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
			ErrorCode: 6,
		}, nil
	}
	_, err = activeMember.Send(client.SendBringUpIP, &rpc.PulseBringIP{
		Iface: iface,
		Ips:   in.Ips,
	})
	if err != nil {
		return &rpc.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
			ErrorCode: 4,
		}, nil
	}
	// respond
	return &rpc.PulseGroupAdd{
		Success: true,
		Message: "IP address(es) successfully added to " + in.Name,
	}, nil
}

/**
Remove IP from group
*/
func (s *CLIServer) GroupIPRemove(ctx context.Context, in *rpc.PulseGroupRemove) (*rpc.PulseGroupRemove, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseGroupRemove{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	// TODO: Note: Validation! IMPORTANT otherwise someone could DOS by seg faulting.
	if in.Ips == nil || in.Name == "" {
		return &rpc.PulseGroupRemove{
			Success: false,
			Message: "Unable to process RPC call. Required parameters: Ips, Name",
			ErrorCode: 2,
		}, nil
	}
	_, activeMember := DB.MemberList.GetActiveMember()
	if activeMember == nil {
		return &rpc.PulseGroupRemove{
			Success: false,
			Message: "Unable to remove IP(s) to group as there no active node in the cluster.",
			ErrorCode: 3,
		}, nil
	}
	err := groupIpRemove(in.Name, in.Ips)
	if err != nil {
		return &rpc.PulseGroupRemove{
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
		return &rpc.PulseGroupRemove{
			Success: false,
			Message: err.Error(),
			ErrorCode: 5,
		}, nil
	}
	activeMember.Send(client.SendBringDownIP, &rpc.PulseBringIP{
		Iface: iface,
		Ips:   in.Ips,
	})
	return &rpc.PulseGroupRemove{
		Success: true,
		Message: "IP address(es) successfully removed from " + in.Name,
	}, nil
}

/**
Assign group to interface
*/
func (s *CLIServer) GroupAssign(ctx context.Context, in *rpc.PulseGroupAssign) (*rpc.PulseGroupAssign, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseGroupAssign{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	uid, _, err := nodeGetByHostname(in.Node)
	if err != nil {
		return &rpc.PulseGroupAssign{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := groupAssign(in.Group, uid, in.Interface); err != nil {
		return &rpc.PulseGroupAssign{
			Success: false,
			Message: err.Error(),
			ErrorCode: 3,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.PulseGroupAssign{
		Success: true,
		Message: in.Group + " assigned to interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
Unassign group from interface
*/
func (s *CLIServer) GroupUnassign(ctx context.Context, in *rpc.PulseGroupUnassign) (*rpc.PulseGroupUnassign, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseGroupUnassign{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	uid, _, err := nodeGetByHostname(in.Node)
	if err != nil {
		return &rpc.PulseGroupUnassign{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := groupUnassign(in.Group, uid, in.Interface); err != nil {
		return &rpc.PulseGroupUnassign{
			Success: false,
			Message: err.Error(),
			ErrorCode: 3,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	DB.MemberList.SyncConfig()
	return &rpc.PulseGroupUnassign{
		Success: true,
		Message: in.Group + " unassigned from interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
Show all groups
*/
func (s *CLIServer) GroupList(ctx context.Context, in *rpc.GroupTable) (*rpc.GroupTable, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.GroupTable{
			Success: false,
			Message: CLUSTER_REQUIRED_MESSAGE,
		}, nil
	}
	table := new(rpc.GroupTable)
	for name, ips := range DB.Config.Groups {
		nodes, interfaces := getGroupNodes(name)
		row := &rpc.GroupRow{Name: name, Ip: ips, Nodes: nodes, Interfaces: interfaces}
		table.Row = append(table.Row, row)
	}
	return table, nil
}

/**
Return the status for each node within the cluster
*/
func (s *CLIServer) Status(ctx context.Context, in *rpc.PulseStatus) (*rpc.PulseStatus, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseStatus{
			Success: false,
			Message: CLUSTER_REQUIRED_MESSAGE,
		}, nil
	}
	table := new(rpc.PulseStatus)
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
		}
		table.Row = append(table.Row, row)
	}
	table.Success = true
	return table, nil
}

/**
Handle CLI promote request
*/
func (s *CLIServer) Promote(ctx context.Context, in *rpc.PulsePromote) (*rpc.PulsePromote, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulsePromote{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := DB.MemberList.PromoteMember(in.Member)
	if err != nil {
		return &rpc.PulsePromote{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	return &rpc.PulsePromote{
		Success: true,
		Message: "Successfully promoted member " + in.Member,
	}, nil
}

/**
Handle CLI promote request
*/
func (s *CLIServer) TLS(ctx context.Context, in *rpc.PulseCert) (*rpc.PulseCert, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseCert{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 1,
		}, nil
	}
	err := security.GenTLSKeys(in.BindIp)
	if err != nil {
		return &rpc.PulseCert{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	return &rpc.PulseCert{
		Success: true,
		Message: "Successfully generated new TLS certificates",
	}, nil
}

// Config - Update any key's value in the pulsectl section of the config
func (s *CLIServer) Config(ctx context.Context, in *rpc.PulseConfig) (*rpc.PulseConfig, error) {
	s.Lock()
	defer s.Unlock()
	// Validation
	if in.Key == "local_node" ||
		in.Key == "cluster_token" {
		return &rpc.PulseConfig{
			Success: false,
			Message: "You are unable to use the config command to change the value of " + in.Key,
			ErrorCode: 4,
		}, nil
	}
	// If the value is hostname, update our node in our nodes section as well
	if in.Key == "hostname" {
		if err := nodeUpdateLocalHostname(in.Value); err != nil {
			return &rpc.PulseConfig{
				Success: false,
				Message: err.Error(),
				ErrorCode: 3,
			}, nil
		}
		return &rpc.PulseConfig{
			Success: true,
			Message: "Successfully updated PulseHA config",
		}, nil
	}
	// Update our key value
	if err := DB.Config.UpdateValue(in.Key, in.Value); err != nil {
		return &rpc.PulseConfig{
			Success: false,
			Message: err.Error(),
			ErrorCode: 2,
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
		return &rpc.PulseConfig{
			Success: false,
			Message: err.Error(),
			ErrorCode: 5,
		}, nil
	}
	// Reload our config
	DB.Config.Reload()
	// Sync it with our peers
	if err := DB.MemberList.SyncConfig(); err != nil {
		return &rpc.PulseConfig{
			Success: false,
			Message: err.Error(),
			ErrorCode: 6,
		}, nil
	}
	return &rpc.PulseConfig{
		Success: true,
		Message: "Successfully updated PulseHA config",
	}, nil
}

// Token - Generate a new cluster token
func (s *CLIServer) Token(ctx context.Context, in *rpc.PulseToken) (*rpc.PulseToken, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseToken{
			Success: false,
			Message: CLUSTER_REQUIRED_MESSAGE,
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
		return &rpc.PulseToken{
			Success:   false,
			Message:   CLUSTER_REQUIRED_MESSAGE,
			ErrorCode: 2,
		}, nil
	}
	// Save back to our config
	if err := DB.Config.Save(); err != nil {
		log.Error("Unable to save local config. This likely means the local config is now out of date.")
	}
	// report back with success
	return &rpc.PulseToken{
		Success: true,
		Message: "Success! Your new cluster token is " + token,
		Token: token,
	}, nil
}

// Network -
func (s *CLIServer) Network(ctx context.Context, in *rpc.PulseNetwork) (*rpc.PulseNetwork, error) {
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &rpc.PulseNetwork{
			Success: false,
			Message: CLUSTER_REQUIRED_MESSAGE,
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

func (s *CLIServer) Describe(ctx context.Context, in *rpc.PulseDescribe) (*rpc.PulseDescribe, error) {
	return &rpc.PulseDescribe{}, nil
}
