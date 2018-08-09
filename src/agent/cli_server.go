/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

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
package agent

import (
	"context"
	"encoding/json"
	"github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/utils"
	log "github.com/Sirupsen/logrus"
	"google.golang.org/grpc"
	"net"
	"sync"
	"time"
	"os"
	"errors"
)

/**
Server struct type
*/
type CLIServer struct {
	sync.Mutex
	Server     *Server
	Listener   net.Listener
	Memberlist *Memberlist
}

/**
Attempt to join a configured cluster
Notes: We create a new client in attempt to communicate with our peer.
       If successful we acknowledge it and update our memberlist.
*/
func (s *CLIServer) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	log.Debug("CLIServer:Join() Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !gconf.clusterCheck() {
		// Generate client server keys if tls is enabled
		if gconf.Pulse.TLS {
			genTLSKeys(in.BindIp)
		}
		// Create a new client
		client := &Client{}
		// Attempt to connect
		err := client.Connect(in.Ip, in.Port, in.Hostname)
		// Handle a client connection error
		if err != nil {
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		// Create new local node config to send
		newNode := &Node{
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
		hostname, err := GetHostname()
		if err != nil {
			return nil, errors.New("cannot to join because unable to get hostname")
		}
		r, err := client.Send(SendJoin, &proto.PulseJoin{
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
		peerConfig := &Config{}
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
		gconf.SetConfig(*peerConfig)
		// Save the config
		gconf.save()
		// Reload config in memory
		gconf.reload()
		// Setup our daemon server
		go s.Server.Setup()
		// reset our HC last received time
		localMember, _ := s.Memberlist.getLocalMember()
		localMember.setLastHCResponse(time.Now())
		// Close the connection
		client.Close()
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
	if !gconf.clusterCheck() {
		return &proto.PulseLeave{
			Success: false,
			Message: "Unable to leave as no cluster was found",
		}, nil
	}
	// Check to see if we are not the only one in the "cluster"
	// Let everyone else know that we are leaving the cluster
	hostname := gconf.getLocalNode()
	if gconf.clusterTotal() > 1 {
		s.Memberlist.Broadcast(
			SendLeave,
			&proto.PulseLeave{
				Replicated: true,
				Hostname:   hostname,
			},
		)
	}
	makeMemberPassive()

	// TODO: horrible way to do this but it will do for now.
	oldNode := gconf.Nodes[hostname]
	newNode := &Node{
		IPGroups: oldNode.IPGroups,
	}
	// ---
	nodesClearLocal()
	// ewww
	nodeAdd(hostname, newNode)
	// ---
	s.Memberlist.reset()
	gconf.save()
	s.Server.shutdown()
	// bring down the ips
	log.Info("Successfully left configured cluster. PulseHA no longer listening..")
	if gconf.clusterTotal() == 1 {
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
	log.Debug("CLIServer:Create() - Create Pulse cluster")
	s.Lock()
	defer s.Unlock()
	// Make sure we are not in a cluster before creating one.
	if !gconf.clusterCheck() {
		hostname := gconf.getLocalNode()
		//TODO: horrible way to do this but it will do for now.
		oldNode := gconf.Nodes[hostname]
		newNode := &Node{
			IP: in.BindIp,
			Port: in.BindPort,
			IPGroups: oldNode.IPGroups,
		}
		nodeDelete(hostname)
		nodeAdd(hostname, newNode)
		// Save back to our config
		gconf.save()
		// Cert stuff
		GenerateCACert(in.BindIp)
		// Generate client server keys if tls is enabled
		if gconf.Pulse.TLS {
			genTLSKeys(in.BindIp)
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
	log.Debug("CLIServer:NewGroup() - Create floating IP group")
	s.Lock()
	defer s.Unlock()
	groupName, err := GroupNew(in.Name)
	if err != nil {
		return &proto.PulseGroupNew{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.save()
	s.Memberlist.SyncConfig()
	return &proto.PulseGroupNew{
		Success: true,
		Message: groupName + " successfully added.",
	}, nil
}

/**
Delete floating IP group
*/
func (s *CLIServer) DeleteGroup(ctx context.Context, in *proto.PulseGroupDelete) (*proto.PulseGroupDelete, error) {
	log.Debug("CLIServer:DeleteGroup() - Delete floating IP group")
	s.Lock()
	defer s.Unlock()
	err := GroupDelete(in.Name)
	if err != nil {
		return &proto.PulseGroupDelete{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.save()
	s.Memberlist.SyncConfig()
	return &proto.PulseGroupDelete{
		Success: true,
		Message: in.Name + " successfully deleted.",
	}, nil
}

/**
Add IP to group
*/
func (s *CLIServer) GroupIPAdd(ctx context.Context, in *proto.PulseGroupAdd) (*proto.PulseGroupAdd, error) {
	log.Debug("CLIServer:GroupIPAdd() - Add IP addresses to group " + in.Name)
	s.Lock()
	defer s.Unlock()
	log.Info("test")
	err := GroupIpAdd(in.Name, in.Ips)
	if err != nil {
		return &proto.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.save()
	s.Memberlist.SyncConfig()
	// bring up the ip on the active appliance
	activeHostname, activeMember := s.Memberlist.getActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	activeMember.Connect()
	configCopy := gconf.GetConfig()
	iface := configCopy.getGroupIface(activeHostname, in.Name)
	activeMember.Send(SendBringUpIP, &proto.PulseBringIP{
		Iface: iface,
		Ips: in.Ips,
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
	log.Debug("CLIServer:GroupIPRemove() - Removing IPs from group " + in.Name)
	s.Lock()
	defer s.Unlock()
	// TODO: Note: Validation! IMPORTANT otherwise someone could DOS by seg faulting.
	if in.Ips == nil || in.Name == "" {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: "Unable to process RPC call. Required parameters: Ips, Name",
		}, nil
	}
	_, activeMember := s.Memberlist.getActiveMember()
	if activeMember == nil {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: "Unable to remove IP(s) to group as there no active node in the cluster.",
		}, nil
	}
	err := GroupIpRemove(in.Name, in.Ips)
	if err != nil {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.save()
	s.Memberlist.SyncConfig()
	// bring down the ip on the active appliance
	activeHostname, activeMember := s.Memberlist.getActiveMember()
	// Connect first just in case.. otherwise we could seg fault
	activeMember.Connect()
	configCopy := gconf.GetConfig()
	iface := configCopy.getGroupIface(activeHostname, in.Name)
	activeMember.Send(SendBringDownIP, &proto.PulseBringIP{
		Iface: iface,
		Ips: in.Ips,
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
	log.Debug("CLIServer:GroupAssign() - Assigning group " + in.Group + " to interface " + in.Interface + " on node " + in.Node)
	s.Lock()
	defer s.Unlock()
	err := GroupAssign(in.Group, in.Node, in.Interface)
	if err != nil {
		return &proto.PulseGroupAssign{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.save()
	s.Memberlist.SyncConfig()
	return &proto.PulseGroupAssign{
		Success: true,
		Message: in.Group + " assigned to interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
Unassign group from interface
*/
func (s *CLIServer) GroupUnassign(ctx context.Context, in *proto.PulseGroupUnassign) (*proto.PulseGroupUnassign, error) {
	log.Debug("CLIServer:GroupUnassign() - Unassigning group " + in.Group + " from interface " + in.Interface + " on node " + in.Node)
	s.Lock()
	defer s.Unlock()
	err := GroupUnassign(in.Group, in.Node, in.Interface)
	if err != nil {
		return &proto.PulseGroupUnassign{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.save()
	s.Memberlist.SyncConfig()
	return &proto.PulseGroupUnassign{
		Success: true,
		Message: in.Group + " unassigned from interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
Show all groups
 */
func (s *CLIServer) GroupList(ctx context.Context, in *proto.GroupTable) (*proto.GroupTable, error) {
	log.Debug("CLIServer:GroupList() - Getting groups and their IPs")
	s.Lock()
	defer s.Unlock()
	table := new(proto.GroupTable)
	config := gconf.GetConfig()
	for name, ips := range config.Groups {
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
	log.Debug("CLIServer:Status() - Getting cluster node statuses")
	s.Lock()
	defer s.Unlock()
	table := new(proto.PulseStatus)
	for _, member := range s.Memberlist.Members {
		details, _ := nodeGetByName(member.Hostname)
		tym := member.getLastHCResponse()
		var tymFormat string
		if tym == (time.Time{}) {
			tymFormat = ""
		} else {
			tymFormat = tym.Format(time.RFC1123)
		}
		row := &proto.StatusRow{
			Hostname: member.getHostname(),
			Ip:       details.IP,
			Latency:     member.getLatency(),
			Status:   member.getStatus(),
			LastReceived: tymFormat,
		}
		table.Row = append(table.Row, row)
	}
	return table, nil
}

/**
Handle CLI promote request
 */
func (s *CLIServer) Promote(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote, error) {
	log.Debug("CLIServer:Promote() - Promote a new member")
	s.Lock()
	defer s.Unlock()
	err := s.Memberlist.PromoteMember(in.Member)
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
	log.Debug("CLIServer:Promote() - Promote a new member")
	s.Lock()
	defer s.Unlock()
	err := genTLSKeys(in.BindIp)
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
