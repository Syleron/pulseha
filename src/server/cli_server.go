/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
package server

import (
	"context"
	"encoding/json"
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/client"
	"github.com/Syleron/PulseHA/src/config"
	"github.com/Syleron/PulseHA/src/security"
	"github.com/Syleron/PulseHA/src/utils"
	"google.golang.org/grpc"
	"net"
	"os"
	"sync"
	"time"
)

/**
Server struct type
*/
type CLIServer struct {
	sync.Mutex
	Server     *Server
	Listener   net.Listener
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
	proto.RegisterCLIServer(grpcServer, s)
	grpcServer.Serve(lis)
}

/**
Attempt to join a configured cluster
Notes: We create a new client in attempt to communicate with our peer.
       If successful we acknowledge it and update our memberlist.
*/
func (s *CLIServer) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	//log.Debug("CLIServer:Join() Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		// Generate client server keys if tls is enabled
		if DB.Config.Pulse.TLS {
			security.GenTLSKeys(in.BindIp)
		}
		// Create a new client
		c := &client.Client{}
		// Attempt to connect
		err := c.Connect(in.Ip, in.Port, in.Hostname, DB.Config.Pulse.TLS)
		// Handle a client connection error
		if err != nil {
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		// Create new local node config to send
		newNode := &config.Node{
			IP:       in.BindIp,
			Port:     in.BindPort,
			IPGroups: make(map[string][]string, 0),
		}
		// Convert struct into byte array
		buf, err := json.Marshal(newNode)
		// Handle failure to marshal config
		if err != nil {
			log.Error("Join() Unable to marshal config: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: "Join failure. Please check the logs for more information",
			}, nil
		}
		// Send our join request
		hostname, err := utils.GetHostname()
		if err != nil {
			return nil, errors.New("cannot to join because unable to get hostname")
		}
		r, err := c.Send(client.SendJoin, &proto.PulseJoin{
			Config:   buf,
			Hostname: hostname,
		})
		// Handle a failed request
		if err != nil {
			log.Error("Join() Request error: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: "Join failure. Unable to connect to host.",
			}, nil
		}
		// Handle an unsuccessful request
		if !r.(*proto.PulseJoin).Success {
			log.Error("Join() Peer error: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: r.(*proto.PulseJoin).Message,
			}, nil
		}
		// Update our local config
		peerConfig := &config.Config{}
		err = json.Unmarshal(r.(*proto.PulseJoin).Config, peerConfig)
		// handle errors
		if err != nil {
			log.Error("Unable to unmarshal config node.")
			return &proto.PulseJoin{
				Success: false,
				Message: "Unable to unmarshal config node.",
			}, nil
		}
		// Set the config
		DB.SetConfig(peerConfig)
		// Save the config
		DB.Config.Save()
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
		return &proto.PulseJoin{
			Success: true,
			Message: "Successfully joined cluster",
		}, nil
	}
	return &proto.PulseJoin{
		Success: false,
		Message: "Unable to join as PulseHA is already in a cluster.",
	}, nil
}

/**
Break cluster / Leave from cluster
TODO: Remember to reassign active role on leave
*/
func (s *CLIServer) Leave(ctx context.Context, in *proto.PulseLeave) (*proto.PulseLeave, error) {
	log.Debug("CLIServer:Leave() - Leave Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !DB.Config.ClusterCheck() {
		return &proto.PulseLeave{
			Success: false,
			Message: "Unable to leave as no cluster was found",
		}, nil
	}
	// Check to see if we are not the only one in the "cluster"
	// Let everyone else know that we are leaving the cluster
	hostname := DB.Config.GetLocalNode()
	if DB.Config.NodeCount() > 1 {
		DB.MemberList.Broadcast(
			client.SendLeave,
			&proto.PulseLeave{
				Replicated: true,
				Hostname:   hostname,
			},
		)
	}
	MakeLocalPassive()
	// TODO: horrible way to do this but it will do for now.
	oldNode := DB.Config.Nodes[hostname]
	newNode := &config.Node{
		IPGroups: oldNode.IPGroups,
	}
	// ---
	nodesClearLocal()
	// ewww
	nodeAdd(hostname, newNode)
	// ---
	DB.MemberList.Reset()
	DB.Config.Save()
	s.Server.Shutdown()
	// bring down the ips
	log.Info("Successfully left configured cluster. PulseHA no longer listening..")
	if DB.Config.NodeCount() == 1 {
		return &proto.PulseLeave{
			Success: true,
			Message: "Successfully dismantled cluster",
		}, nil
	}
	return &proto.PulseLeave{
		Success: true,
		Message: "Successfully left from cluster",
	}, nil
}

/**
Create new PulseHA cluster
*/
func (s *CLIServer) Create(ctx context.Context, in *proto.PulseCreate) (*proto.PulseCreate, error) {
	s.Lock()
	defer s.Unlock()
	// Make sure we are not in a cluster before creating one.
	if !DB.Config.ClusterCheck() {
		hostname := DB.Config.GetLocalNode()
		//TODO: horrible way to do this but it will do for now.
		// Store the old node section
		oldNode := DB.Config.Nodes[hostname]
		// Create a new node section and populate it using new details
		// and old node details.
		newNode := &config.Node{
			IP:       in.BindIp,
			Port:     in.BindPort,
			IPGroups: oldNode.IPGroups,
		}
		// Remove the old instance
		nodeDelete(hostname)
		// Add the new node instance
		nodeAdd(hostname, newNode)
		// Save back to our config
		DB.Config.Save()
		// Cert stuff
		security.GenerateCACert(in.BindIp)
		// Generate client server keys if tls is enabled
		if DB.Config.Pulse.TLS {
			security.GenTLSKeys(in.BindIp)
		}
		go s.Server.Setup()
		return &proto.PulseCreate{
			Success: true,
			Message: "Pulse cluster successfully created!",
		}, nil
	} else {
		return &proto.PulseCreate{
			Success: false,
			Message: "Pulse daemon is already in a configured cluster",
		}, nil
	}
}

/**
Add a new floating IP group
*/
func (s *CLIServer) NewGroup(ctx context.Context, in *proto.PulseGroupNew) (*proto.PulseGroupNew, error) {
	s.Lock()
	defer s.Unlock()
	groupName, err := groupNew(in.Name)
	if err != nil {
		return &proto.PulseGroupNew{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.MemberList.SyncConfig()
	return &proto.PulseGroupNew{
		Success: true,
		Message: groupName + " successfully added.",
	}, nil
}

/**
Delete floating IP group
*/
func (s *CLIServer) DeleteGroup(ctx context.Context, in *proto.PulseGroupDelete) (*proto.PulseGroupDelete, error) {
	s.Lock()
	defer s.Unlock()
	err := groupDelete(in.Name)
	if err != nil {
		return &proto.PulseGroupDelete{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.MemberList.SyncConfig()
	return &proto.PulseGroupDelete{
		Success: true,
		Message: in.Name + " successfully deleted.",
	}, nil
}

/**
Add IP to group
*/
func (s *CLIServer) GroupIPAdd(ctx context.Context, in *proto.PulseGroupAdd) (*proto.PulseGroupAdd, error) {
	s.Lock()
	defer s.Unlock()
	err := groupIpAdd(in.Name, in.Ips)
	if err != nil {
		return &proto.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.MemberList.SyncConfig()
	// bring up the ip on the active appliance
	activeHostname, activeMember := DB.MemberList.GetActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	activeMember.Connect()
	iface := DB.Config.GetGroupIface(activeHostname, in.Name)
	activeMember.Send(client.SendBringUpIP, &proto.PulseBringIP{
		Iface: iface,
		Ips:   in.Ips,
	})
	// respond
	return &proto.PulseGroupAdd{
		Success: true,
		Message: "IP address(es) successfully added to " + in.Name,
	}, nil
}

/**
Remove IP from group
*/
func (s *CLIServer) GroupIPRemove(ctx context.Context, in *proto.PulseGroupRemove) (*proto.PulseGroupRemove, error) {
	s.Lock()
	defer s.Unlock()
	// TODO: Note: Validation! IMPORTANT otherwise someone could DOS by seg faulting.
	if in.Ips == nil || in.Name == "" {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: "Unable to process RPC call. Required parameters: Ips, Name",
		}, nil
	}
	_, activeMember := DB.MemberList.GetActiveMember()
	if activeMember == nil {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: "Unable to remove IP(s) to group as there no active node in the cluster.",
		}, nil
	}
	err := groupIpRemove(in.Name, in.Ips)
	if err != nil {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.MemberList.SyncConfig()
	// bring down the ip on the active appliance
	activeHostname, activeMember := DB.MemberList.GetActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	activeMember.Connect()
	iface := DB.Config.GetGroupIface(activeHostname, in.Name)
	activeMember.Send(client.SendBringDownIP, &proto.PulseBringIP{
		Iface: iface,
		Ips:   in.Ips,
	})
	return &proto.PulseGroupRemove{
		Success: true,
		Message: "IP address(es) successfully removed from " + in.Name,
	}, nil
}

/**
Assign group to interface
*/
func (s *CLIServer) GroupAssign(ctx context.Context, in *proto.PulseGroupAssign) (*proto.PulseGroupAssign, error) {
	s.Lock()
	defer s.Unlock()
	err := groupAssign(in.Group, in.Node, in.Interface)
	if err != nil {
		return &proto.PulseGroupAssign{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.MemberList.SyncConfig()
	return &proto.PulseGroupAssign{
		Success: true,
		Message: in.Group + " assigned to interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
Unassign group from interface
*/
func (s *CLIServer) GroupUnassign(ctx context.Context, in *proto.PulseGroupUnassign) (*proto.PulseGroupUnassign, error) {
	s.Lock()
	defer s.Unlock()
	err := groupUnassign(in.Group, in.Node, in.Interface)
	if err != nil {
		return &proto.PulseGroupUnassign{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.MemberList.SyncConfig()
	return &proto.PulseGroupUnassign{
		Success: true,
		Message: in.Group + " unassigned from interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
Show all groups
*/
func (s *CLIServer) GroupList(ctx context.Context, in *proto.GroupTable) (*proto.GroupTable, error) {
	s.Lock()
	defer s.Unlock()
	table := new(proto.GroupTable)
	for name, ips := range DB.Config.Groups {
		nodes, interfaces := getGroupNodes(name)
		row := &proto.GroupRow{Name: name, Ip: ips, Nodes: nodes, Interfaces: interfaces}
		table.Row = append(table.Row, row)
	}
	return table, nil
}

/**
Return the status for each node within the cluster
*/
func (s *CLIServer) Status(ctx context.Context, in *proto.PulseStatus) (*proto.PulseStatus, error) {
	s.Lock()
	defer s.Unlock()
	table := new(proto.PulseStatus)
	for _, member := range DB.MemberList.Members {
		details, _ := nodeGetByName(member.Hostname)
		tym := member.GetLastHCResponse()
		var tymFormat string
		if tym == (time.Time{}) {
			tymFormat = ""
		} else {
			tymFormat = tym.Format(time.RFC1123)
		}
		row := &proto.StatusRow{
			Hostname:     member.GetHostname(),
			Ip:           details.IP,
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
func (s *CLIServer) Promote(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote, error) {
	s.Lock()
	defer s.Unlock()
	err := DB.MemberList.PromoteMember(in.Member)
	if err != nil {
		return &proto.PulsePromote{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &proto.PulsePromote{
		Success: true,
		Message: "Successfully promoted member " + in.Member,
	}, nil
}

/**
Handle CLI promote request
*/
func (s *CLIServer) TLS(ctx context.Context, in *proto.PulseCert) (*proto.PulseCert, error) {
	s.Lock()
	defer s.Unlock()
	err := security.GenTLSKeys(in.BindIp)
	if err != nil {
		return &proto.PulseCert{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &proto.PulseCert{
		Success: true,
		Message: "Successfully generated new TLS certificates",
	}, nil
}
