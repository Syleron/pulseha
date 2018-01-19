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
	"encoding/json"
	"github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/utils"
	log "github.com/Sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"net"
	"strconv"
	"sync"
	"time"
	"runtime/debug"
	"crypto/tls"
	"io/ioutil"
	"crypto/x509"
)

/**
 * Server struct type
 */
type Server struct {
	sync.Mutex
	Server      *grpc.Server
	Listener    net.Listener
	Memberlist  *Memberlist
	HCScheduler func()
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
	var bindIP string
	bindIP = utils.FormatIPv6(config.LocalNode().IP)
	// Listen
	s.Listener, err = net.Listen("tcp", bindIP +":" + config.LocalNode().Port)
	if err != nil {
		debug.PrintStack()
		panic(err)
		//log.Errorf("Failed to listen: %s", err)
		// TODO: Note: Should we exit here?
		return
	}
	if config.Pulse.TLS {
		// load member cert/key
		peerCert, err := tls.LoadX509KeyPair(certDir+ utils.GetHostname() + ".server.crt", certDir + utils.GetHostname() + ".server.key")
		if err != nil {
			log.Error("load peer cert/key error:%v", err)
			return
		}
		// Load CA cert
		caCert, err := ioutil.ReadFile("ca.crt")
		if err != nil {
			log.Error("read ca cert file error:%v", err)
			return
		}
		// Define cert pool
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		creds := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{peerCert},
			ClientCAs:    caCertPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		})
		s.Server = grpc.NewServer(grpc.Creds(creds))
	} else {
		log.Warning("TLS Disabled! PulseHA server connection unsecured.")
		s.Server = grpc.NewServer()
	}
	proto.RegisterServerServer(s.Server, s)
	s.Memberlist.Setup()
	log.Info("PulseHA initialised on " + config.LocalNode().IP + ":" + config.LocalNode().Port)
	s.Server.Serve(s.Listener)
}

/**
 * Shutdown pulse server (not cli/cmd)
 */
func (s *Server) shutdown() {
	log.Debug("Shutting down server")
	if s.Server != nil {
		s.Server.GracefulStop()
	}
	if s.Listener != nil {
		s.Listener.Close()
	}
}

/**
Perform appr. health checks
*/
func (s *Server) HealthCheck(ctx context.Context, in *proto.PulseHealthCheck) (*proto.PulseHealthCheck, error) {
	log.Debug("Server:HealthCheck() Receiving health check")
	s.Lock()
	defer s.Unlock()
	activeHostname, _ := s.Memberlist.getActiveMember()
	if activeHostname != gconf.getLocalNode() {
		localMember := s.Memberlist.GetMemberByHostname(gconf.getLocalNode())
		// make passive to reset the networking
		if _, activeMember := s.Memberlist.getActiveMember(); activeMember == nil {
			log.Info("Local node is passive")
			localMember.makePassive()
		}
		localMember.setLastHCResponse(time.Now())
		s.Memberlist.update(in.Memberlist)
	} else {
		log.Warn("Active node mismatch")
		hostname := getFailOverCountWinner(in.Memberlist)
		log.Info("Member " + hostname + " has been determined as the correct active node.")
		if hostname != gconf.getLocalNode() {
			member, _ := s.Memberlist.getLocalMember()
			member.makePassive()
		} else {
			localMember, _ := pulse.getMemberlist().getLocalMember()
			localMember.setLastHCResponse(time.Time{})
		}
	}
	return &proto.PulseHealthCheck{
		Success: true,
	}, nil
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
		s.Memberlist.Reload()
		// Return with our new updated config
		buf, err := json.Marshal(gconf.GetConfig())
		// Handle failure to marshal config
		if err != nil {
			log.Fatal("Unable to marshal config: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		log.Info(in.Hostname + " has joined the cluster")
		return &proto.PulseJoin{
			Success: true,
			Message: "Successfully added ",
			Config:  buf,
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
		log.Fatal("Unable to marshal config: %s", err)
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
	s.Memberlist.Reload()
	// Let the logs know
	log.Info("Successfully r-synced local config")
	// Return with yay
	return &proto.PulseConfigSync{
		Success: true,
	}, nil
}

/**
Network action functions
*/
func (s *Server) Promote(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote, error) {
	log.Debug("Server:MakeActive() Making node active")
	s.Lock()
	defer s.Unlock()
	if in.Member != gconf.getLocalNode() {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	member := s.Memberlist.GetMemberByHostname(in.Member)
	if member == nil {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	success := member.makeActive()
	log.Info(in.Member + " has been promoted to active")
	return &proto.PulsePromote{
		Success: success,
	}, nil
}

/**
Make a member passive
 */
func (s *Server) MakePassive(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote, error) {
	log.Debug("Server:MakePassive() Making node passive")
	s.Lock()
	defer s.Unlock()
	if in.Member != gconf.getLocalNode() {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	member := s.Memberlist.GetMemberByHostname(in.Member)
	if member == nil {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	success := member.makePassive()
	log.Info(in.Member + " has been demoted to passive")
	return &proto.PulsePromote{
		Success: success,
	}, nil
}

/**

 */
func (s *Server) BringUpIP(ctx context.Context, in *proto.PulseBringIP) (*proto.PulseBringIP, error) {
	log.Debug("Server:BringUpIP() Bringing up IP(s)")
	s.Lock()
	defer s.Unlock()
	err := bringUpIPs(in.Iface, in.Ips)
	success := false
	msg := "success"
	if err != nil {
		success = true
		msg = err.Error()
	}
	return &proto.PulseBringIP{Success: success, Message: msg}, nil
}

/**

 */
func (s *Server) BringDownIP(ctx context.Context, in *proto.PulseBringIP) (*proto.PulseBringIP, error) {
	log.Debug("Server:BringDownIP() Bringing down IP(s)")
	s.Lock()
	defer s.Unlock()
	err := bringDownIPs(in.Iface, in.Ips)
	success := false
	msg := "success"
	if err != nil {
		success = true
		msg = err.Error()
	}
	return &proto.PulseBringIP{Success: success, Message: msg}, nil
}
