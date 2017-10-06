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
	"github.com/Syleron/PulseHA/src/utils"
	"encoding/json"
	"strconv"
)

/**
 * Server struct type
 */
type Server struct {
	sync.Mutex
	Status        proto.HealthCheckResponse_ServingStatus
	//Last_response time.Time
	//Log log.Logger
	Server *grpc.Server
	Listener net.Listener
	Memberlist *Memberlist
}

/**
	Perform appr. health checks
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
	Join request for a configured cluster
 */
func (s *Server) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	log.Debug("Server:Join() " + strconv.FormatBool(in.Replicated) + " - Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if gconf.ClusterCheck() {
		// Define new node
		originNode := &Node{}
		// unmarshal byte data to new node
		err := json.Unmarshal(in.Config, originNode)
		// handle errors
		if err != nil {
			log.Error("Unable to unmarshal config node.")
			return &proto.PulseJoin{
				Success: false,
				Message: "Unable to unmarshal config node.",
			}, nil
		}
		// TODO: Node validation?
		// Add node to config
		NodeAdd(in.Hostname, originNode)
		// Save our new config to file
		gconf.Save()
		// Update the cluster config
		s.Memberlist.SyncConfig()
		// Add node to the memberlist
		// TODO: Reconsider how this is done. Perhaps a member reload
		s.Memberlist.ReloadMembers()
		// Return with our new updated config
		buf, err := json.Marshal(gconf.GetConfig())
		// Handle failure to marshal config
		if err != nil {
			log.Emergency("Unable to marshal config: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		return &proto.PulseJoin{
			Success: true,
			Message: "Successfully added ",
			Config: buf,
		}, nil
	}
	return &proto.PulseJoin{
		Success: false,
		Message: "This node is not in a configured cluster.",
	}, nil
}

/**
	Update our local config from a Resync request
 */
func (s *Server) Leave(ctx context.Context, in *proto.PulseLeave) (*proto.PulseLeave, error) {
	log.Debug("Server:Leave() " + strconv.FormatBool(in.Replicated) + " - Node leave cluster")
	s.Lock()
	defer s.Unlock()
	// Remove from our memberlist
	s.Memberlist.MemberRemoveByName(in.Hostname)
	// Remove from our config
	err := NodeDelete(in.Hostname)
	if err != nil {
		return &proto.PulseLeave{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	gconf.Save()
	return &proto.PulseLeave{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

/**
	Update our local config from a Resync request
 */
func (s *Server) ConfigSync(ctx context.Context, in *proto.PulseConfigSync) (*proto.PulseConfigSync, error) {
	log.Debug("Server:ConfigSync() " + strconv.FormatBool(in.Replicated) + " - Sync cluster config")
	s.Lock()
	defer s.Unlock()
	// Define new node
	newConfig := &Config{}
	// unmarshal byte data to new node
	err := json.Unmarshal(in.Config, newConfig)
	// Handle failure to marshal config
	if err != nil {
		log.Emergency("Unable to marshal config: %s", err)
		return &proto.PulseConfigSync{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	// Set our new config in memory
	gconf.SetConfig(*newConfig)
	// Save our config to file
	gconf.Save()
	// Update our member list
	s.Memberlist.ReloadMembers()
	// Let the logs know
	log.Info("Successfully r-synced local config")
	// Return with yay
	return &proto.PulseConfigSync{
		Success: true,
	}, nil
}
/**
 * Setup pulse server type
 */
func (s *Server) Setup() {
	config := gconf.GetConfig()
	if !gconf.ClusterCheck() {
		log.Info("PulseHA is currently un-configured.")
		return
	}
	var err error
	s.Listener, err = net.Listen("tcp", config.LocalNode().IP+":"+config.LocalNode().Port)
	if err != nil {
		log.Errorf("Failed to listen: %s", err)
		os.Exit(1)
	}
	if config.Pulse.TLS {
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
	proto.RegisterServerServer(s.Server, s)
	s.Memberlist.Setup()
	log.Info("Pulse initialised on " + config.LocalNode().IP + ":" + config.LocalNode().Port)
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


// network action functions

func (s *Server)RpcMakeActive(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote,error){
	if in.Member != gconf.getLocalNode() {
		return &proto.PulsePromote{Success:false,
		Message:"cannot promote a node other than ourself by rpcMakeActive",
		Member:""}, nil
	}
	err := makeMemberActive()
	success := false
	if err != nil {
		success = true
	}
	return &proto.PulsePromote{Success:success,Message:err.Error(),Member:""}, nil
}