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
package main

import (
	"context"
	"github.com/Syleron/PulseHA/proto"
	"github.com/coreos/go-log/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
	"encoding/json"
	"strconv"
	"github.com/Syleron/PulseHA/src/netUtils"
	"github.com/Syleron/PulseHA/src/utils"
)

/**
 * Server struct type
 */
type Server struct {
	sync.Mutex
	Status        proto.HealthCheckResponse_ServingStatus
	Last_response time.Time
	Log log.Logger
	Server *grpc.Server
	Listener net.Listener
	Memberlist *Memberlist
}

/**
 *
 */
func (s *Server) Check(ctx context.Context, in *proto.HealthCheckRequest) (*proto.HealthCheckResponse, error) {
	s.Lock()
	defer s.Unlock()
	switch in.Request {
	case proto.HealthCheckRequest_SETUP:
		log.Debug("Server:Check() - HealthCheckRequest Setup")
	case proto.HealthCheckRequest_STATUS:
		log.Debug("Server:Check() - HealthCheckRequest Status")
		return &proto.HealthCheckResponse{
			Status: proto.HealthCheckResponse_CONFIGURED,
		}, nil
	default:
	}
	return nil, nil
}

/**
 * Attempt to join a configured cluster
 * Notes: We create a new client in attempt to communicate with our peer.
 *        If successful we acknowledge it and update our memberlist.
 */
func (s *Server) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	log.Debug("Server:Join() " + strconv.FormatBool(in.Replicated) + " - Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if in.Replicated {
		return s.JoinReplicated(in)
	}
	if !clusterCheck() {
		client := &Client{}
		err := client.Connect(in.Ip, in.Port, in.Hostname)
		if err != nil {
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		newNode := &Node{
			IP:       in.Ip,
			Port:     in.Port,
			IPGroups: make(map[string][]string, 0),
		}
		buf, err := json.Marshal(newNode)
		if err != nil {
			log.Emergency("Unable to marshal config: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		r, err := client.SendJoin(&proto.PulseJoin{
			Replicated: true,
			Config: buf,
			Hostname: utils.GetHostname(),
		})
		if err != nil {
			log.Emergency("Response error: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		if !r.Success {
			log.Emergency("Peer error: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: r.Message,
			}, nil
		}
		client.Close()
		return &proto.PulseJoin{
			Success: true,
		}, nil
	}
	return &proto.PulseJoin{
		Success: false,
		Message: "Unable to join as PulseHA is already in a cluster.",
	}, nil
}

/**
 * Join replicated logic. This is only performed when sent by another peer/node.
 */
func (s *Server) JoinReplicated(in *proto.PulseJoin) (*proto.PulseJoin, error) {
	if clusterCheck() {
		originNode := &Node{}
		err := json.Unmarshal(in.Config, originNode)
		if err != nil {
			log.Error("Unable to unmarshal config node.")
			return &proto.PulseJoin{
				Success: false,
				Message: "Unable to unmarshal config node.",
			}, nil
		}
		// TODO: Node validation?
		NodeAdd(in.Hostname, originNode)
		gconf.Save()
		// we need to return the entire conf
		return &proto.PulseJoin{
			Success: true,
			Message: "Successfully added ",
		}, nil
	}
	 return &proto.PulseJoin{
	 	Success: false,
	 	Message: "This node is not in a configured cluster.",
	 }, nil
}

/**
 * Break cluster / Leave from cluster
 * TODO: Leave from cluster. At the moment it will only break if we are the sole member.
 */
func (s *Server) Leave(ctx context.Context, in *proto.PulseLeave) (*proto.PulseLeave, error) {
	log.Debug("Server:Leave() - Leave Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !clusterCheck() {
		return &proto.PulseLeave{
			Success: false,
			Message: "Unable to leave as no cluster was found",
		}, nil
	}
	GroupClearLocal()
	NodesClearLocal()
	gconf.Save()
	s.shutdown()
	if clusterTotal() == 1 {
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
 * Note: This will probably need to be replicated..
 */
func (s *Server) Create(ctx context.Context, in *proto.PulseCreate) (*proto.PulseCreate, error) {
	log.Debug("Server:Create() - Create Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if !clusterCheck() {
		newNode := &Node{
			IP:       in.BindIp,
			Port:     in.BindPort,
			IPGroups: make(map[string][]string, 0),
		}
		NodeAdd(utils.GetHostname(), newNode)
		for _, ifaceName := range netUtils.GetInterfaceNames() {
			if ifaceName != "lo" {
				newNode.IPGroups[ifaceName] = make([]string, 0)
				groupName := GenGroupName()
				gconf.Groups[groupName] = []string{}
				GroupAssign(groupName, utils.GetHostname(), ifaceName)
			}
		}
		gconf.Save()
		go s.Setup()
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
 * Add a new floating IP group
 * Note: This will probably need to be replicated..
 */
func (s *Server) NewGroup(ctx context.Context, in *proto.PulseGroupNew) (*proto.PulseGroupNew, error) {
	log.Debug("Server:NewGroup() - Create floating IP group")
	s.Lock()
	defer s.Unlock()
	groupName, err := GroupNew()
	if err != nil {
		return &proto.PulseGroupNew{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseGroupNew{
		Success: true,
		Message: groupName + " successfully added.",
	}, nil
}

/**
 * Delete floating IP group
 */
func (s *Server) DeleteGroup(ctx context.Context, in *proto.PulseGroupDelete) (*proto.PulseGroupDelete, error) {
	log.Debug("Server:DeleteGroup() - Delete floating IP group")
	s.Lock()
	defer s.Unlock()
	err := GroupDelete(in.Name)
	if err != nil {
		return &proto.PulseGroupDelete{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseGroupDelete{
		Success: true,
		Message: in.Name + " successfully deleted.",
	}, nil
}

/**
 *
 * Note: This will probably need to be replicated..
 */
func (s *Server) GroupIPAdd(ctx context.Context, in *proto.PulseGroupAdd) (*proto.PulseGroupAdd, error) {
	log.Debug("Server:GroupIPAdd() - Add IP addresses to group " + in.Name)
	s.Lock()
	defer s.Unlock()
	err := GroupIpAdd(in.Name, in.Ips)
	if err != nil {
		return &proto.PulseGroupAdd{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseGroupAdd{
		Success: true,
		Message: "IP address(es) successfully added to " + in.Name,
	}, nil
}

/**
 *
 * Note: This will probably need to be replicated..
 */
func (s *Server) GroupIPRemove(ctx context.Context, in *proto.PulseGroupRemove) (*proto.PulseGroupRemove, error) {
	log.Debug("Server:GroupIPRemove() - Removing IPs from group " + in.Name)
	s.Lock()
	defer s.Unlock()
	err := GroupIpRemove(in.Name, in.Ips)
	if err != nil {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseGroupRemove{
		Success: true,
		Message: "IP address(es) successfully removed from " + in.Name,
	}, nil
}

/**
 *
 * Note: This will probably need to be replicated..
 */
func (s *Server) GroupAssign(ctx context.Context, in *proto.PulseGroupAssign) (*proto.PulseGroupAssign, error) {
	log.Debug("Server:GroupAssign() - Assigning group " + in.Group + " to interface " + in.Interface + " on node " + in.Node)
	s.Lock()
	defer s.Unlock()
	err := GroupAssign(in.Group, in.Node, in.Interface)
	if err != nil {
		return &proto.PulseGroupAssign{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseGroupAssign{
		Success: true,
		Message: in.Group + " assigned to interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
 *
 * Note: This will probably need to be replicated..
 */
func (s *Server) GroupUnassign(ctx context.Context, in *proto.PulseGroupUnassign) (*proto.PulseGroupUnassign, error) {
	log.Debug("Server:GroupUnassign() - Unassigning group " + in.Group + " from interface " + in.Interface + " on node " + in.Node)
	s.Lock()
	defer s.Unlock()
	err := GroupUnassign(in.Group, in.Node, in.Interface)
	if err != nil {
		return &proto.PulseGroupUnassign{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseGroupUnassign{
		Success: true,
		Message: in.Group + " unassigned from interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
 *
 */
func (s *Server) GroupList(ctx context.Context, in *proto.GroupTable) (*proto.GroupTable, error) {
	log.Debug("Server:GroupList() - Getting groups and their IPs")
	s.Lock()
	defer s.Unlock()
	table := new(proto.GroupTable)
	config := gconf.GetConfig()
	for name, ips := range config.Groups {
		nodes, interfaces := getGroupNodes(name)
		row := &proto.GroupRow{Name:name, Ip:ips, Nodes:nodes, Interfaces:interfaces }
		table.Row = append(table.Row, row)
	}
	return table, nil
}

/**
 *function to get the nodes and interfaces that relate to the specified node
 */
func getGroupNodes(group string)([]string, []string) {
	var hosts []string
	var interfaces []string
	var found = false
	config := gconf.GetConfig()
	for name, node := range config.Nodes {
		for iface, groupNameSlice := range node.IPGroups {
			for _, groupName := range groupNameSlice{
				if group == groupName{
				hosts = append(hosts, name)
				interfaces = append(interfaces, iface)
				found = true
				}
			}
		}
	}
	if found {
		return hosts, interfaces
	}
	return nil, nil
}

/**
 * Setup pulse cli type
 */
func (s *Server) SetupCLI() {
	log.Info("CLI initialised on 127.0.0.1:9443")
	lis, err := net.Listen("tcp", "127.0.0.1:9443")
	if err != nil {
		log.Errorf("Failed to listen: %s", err)
	}
	grpcServer := grpc.NewServer()
	proto.RegisterRequesterServer(grpcServer, s)
	grpcServer.Serve(lis)
}

/**
 * Setup pulse server type
 */
func (s *Server) Setup() {
	configCopy := gconf.GetConfig()
	if !clusterCheck() {
		log.Info("PulseHA is currently un-configured.")
		return
	}
	var err error
	s.Listener, err = net.Listen("tcp", configCopy.LocalNode().IP+":"+configCopy.LocalNode().Port)
	if err != nil {
		log.Errorf("Failed to listen: %s", err)
		os.Exit(1)
	}
	if configCopy.Pulse.TLS {
		// Get project directory location
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			log.Emergency(err)
		}
		if utils.CreateFolder(dir + "/certs") {
			log.Warning("TLS keys are missing! Generating..")
			GenOpenSSL()
		}
		creds, err := credentials.NewServerTLSFromFile(dir+"/certs/server.crt", dir+"/certs/server.key")
		if err != nil {
			log.Error("Could not load TLS keys.")
			os.Exit(1)
		}
		s.Server = grpc.NewServer(grpc.Creds(creds))
	} else {
		log.Warning("TLS Disabled! Pulse server connection unsecured.")
		s.Server = grpc.NewServer()
	}
	proto.RegisterRequesterServer(s.Server, s)
	s.Memberlist.Setup()
	log.Info("Pulse initialised on " + configCopy.LocalNode().IP + ":" + configCopy.LocalNode().Port)
	s.Server.Serve(s.Listener)
}

/**
 * Shutdown pulse server (not cli/cmd)
 */
func (s *Server) shutdown() {
	log.Debug("Shutting down server")
	s.Server.GracefulStop()
	s.Listener.Close()
}
