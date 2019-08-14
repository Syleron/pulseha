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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/config"
	"github.com/Syleron/PulseHA/src/security"
	"github.com/Syleron/PulseHA/src/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io/ioutil"
	"net"
	"strconv"
	"sync"
	"time"
)

var (
	DB *Database
)

/**
 * Server struct type
 */
type Server struct {
	sync.Mutex
	Server      *grpc.Server
	Listener    net.Listener
	HCScheduler func()
}

func (s *Server) Init(db *Database) {
	// Set our config
	DB = db
	// Setup/Load plugins
	DB.Plugins.Setup()
	// Setup the server
	s.Setup()
}

/**
 * Setup pulse server type
 */
func (s *Server) Setup() {
	// Get our hostname
	hostname, err := utils.GetHostname()
	if err != nil {
		log.Error("cannot setup server because unable to get local hostname")
		return
	}
	// Check to make sure that our hostname matches with the one in the config
	if DB.Config.Pulse.LocalNode != hostname {
		log.Fatal(errors.New("pulse config 'localnode' does not match system hostname"))
	}

	// Make sure our local node is setup and available
	if exists := nodeExists(hostname); !exists {
		log.Fatal(errors.New("cannot find local hostname in pulse cluster config"))

		if len(DB.Config.Nodes) == 0 {
			// Create local node in config
			nodecreateLocal()
		}
	}

	if !DB.Config.ClusterCheck() {
		log.Info("PulseHA is currently un-configured.")
		return
	}
	var bindIP string
	bindIP = utils.FormatIPv6(DB.Config.LocalNode().IP)
	// Listen
	s.Listener, err = net.Listen("tcp", bindIP+":"+DB.Config.LocalNode().Port)
	if err != nil {
		panic(err)
		// TODO: Note: Should we exit here?
		return
	}
	if DB.Config.Pulse.TLS {
		// load member cert/key
		peerCert, err := tls.LoadX509KeyPair(security.CertDir+hostname+".crt", security.CertDir+hostname+".key")
		if err != nil {
			log.Error("load peer cert/key error:%v", err)
			return
		}
		// Load CA cert
		caCert, err := ioutil.ReadFile(security.CertDir + "ca.crt")
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
	// Set our start delay
	DB.StartDelay = true
	// Setup our members
	DB.MemberList.Setup()
	// Start PulseHA daemon server
	log.Info("PulseHA initialised on " + DB.Config.LocalNode().IP + ":" + DB.Config.LocalNode().Port)
	s.Server.Serve(s.Listener)
}

/**
 * Shutdown pulse server (not cli/cmd)
 */
func (s *Server) Shutdown() {
	log.Info("Shutting down PulseHA daemon")
	// Make passive
	MakeLocalPassive()
	// Clear our
	DB.MemberList.Reset()
	// Shutdown our RPC server
	if s.Server != nil {
		s.Server.Stop()
	}
	if s.Listener != nil {
		s.Listener.Close()
	}
}

/**
Perform appr. health checks
*/
func (s *Server) HealthCheck(ctx context.Context, in *proto.PulseHealthCheck) (*proto.PulseHealthCheck, error) {
	DB.Logging.Debug("Server:HealthCheck() Receiving health check")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	activeHostname, _ := DB.MemberList.GetActiveMember()
	if activeHostname != DB.Config.GetLocalNode() {
		localMember := DB.MemberList.GetMemberByHostname(DB.Config.GetLocalNode())
		// make passive to reset the networking
		if _, activeMember := DB.MemberList.GetActiveMember(); activeMember == nil {
			DB.Logging.Info("Local node is passive")
			localMember.MakePassive()
		}
		localMember.SetLastHCResponse(time.Now())
		DB.MemberList.Update(in.Memberlist)
	} else {
		DB.Logging.Warn("Active node mismatch")
		hostname := GetFailOverCountWinner(in.Memberlist)
		DB.Logging.Info("Member " + hostname + " has been determined as the correct active node.")
		if hostname != DB.Config.GetLocalNode() {
			member, _ := DB.MemberList.GetLocalMember()
			member.MakePassive()
		} else {
			localMember, _ := DB.MemberList.GetLocalMember()
			localMember.SetLastHCResponse(time.Time{})
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
	DB.Logging.Debug("Server:Join() " + strconv.FormatBool(in.Replicated) + " - Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	if DB.Config.ClusterCheck() {
		// Define new node
		originNode := &config.Node{}
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
		nodeAdd(in.Hostname, originNode)
		// Save our new config to file
		DB.Config.Save()
		// Update the cluster config
		DB.MemberList.SyncConfig()
		// Add node to the memberlist
		DB.MemberList.Reload()
		// Return with our new updated config
		buf, err := json.Marshal(DB.Config)
		// Handle failure to marshal config
		if err != nil {
			log.Fatal("Unable to marshal config: %s", err)
			return &proto.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		DB.Logging.Info(in.Hostname + " has joined the cluster")
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
	DB.Logging.Debug("Server:Leave() " + strconv.FormatBool(in.Replicated) + " - Node leave cluster")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	// Remove from our memberlist
	DB.MemberList.MemberRemoveByName(in.Hostname)
	// Remove from our config
	err := nodeDelete(in.Hostname)
	if err != nil {
		return &proto.PulseLeave{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.Logging.Info("Successfully removed " + in.Hostname + " from the cluster")
	return &proto.PulseLeave{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

// Remove - Remove node from cluster by hostname
func (s *Server) Remove(ctx context.Context, in *proto.PulseRemove) (*proto.PulseRemove, error) {
	DB.Logging.Debug("Server:Remove() " + strconv.FormatBool(in.Replicated) + " - Remove " + in.Hostname + "from cluster")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	// Make sure we can get our own hostname
	localHostname, err := utils.GetHostname()
	if err != nil {
		DB.Logging.Debug("Server:Remove() Fail. Unable to get local hostname to remove node from cluster")
		return &proto.PulseRemove{
			Success: false,
			Message: "Unable to perform remove as unable to get local hostname",
		}, nil
	}
	// Set our member status
	member := DB.MemberList.GetMemberByHostname(in.Hostname)
	member.SetStatus(proto.MemberStatus_LEAVING)
	if in.Hostname == localHostname {
		nodesClearLocal()
		s.Shutdown()
		log.Info("Successfully removed " + in.Hostname + " from cluster. PulseHA no longer listening..")
	} else {
		// Remove from our memberlist
		DB.MemberList.MemberRemoveByName(in.Hostname)
		// Remove from our config
		err := nodeDelete(in.Hostname)
		if err != nil {
			return &proto.PulseRemove{
				Success: false,
				Message: err.Error(),
			}, nil
		}
	}
	DB.Config.Save()
	DB.Logging.Info("Successfully removed node " + in.Hostname + " from the cluster")
	return &proto.PulseRemove{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

/**
Update our local config from a Resync request
*/
func (s *Server) ConfigSync(ctx context.Context, in *proto.PulseConfigSync) (*proto.PulseConfigSync, error) {
	DB.Logging.Debug("Server:ConfigSync() " + strconv.FormatBool(in.Replicated) + " - Sync cluster config")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	// Define new node
	newConfig := &config.Config{}
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
	// !!!IMPORTANT!!!: Do not replace our local config
	newConfig.Pulse = DB.Config.Pulse
	// Set our new config in memory
	DB.SetConfig(newConfig)
	// Save our config to file
	DB.Config.Save()
	// Update our member list
	DB.MemberList.Reload()
	// Let the logs know
	DB.Logging.Info("Successfully r-synced local config")
	// Return with yay
	return &proto.PulseConfigSync{
		Success: true,
	}, nil
}

/**
Network action functions
*/
func (s *Server) Promote(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote, error) {
	DB.Logging.Debug("Server:MakeActive() Making node active")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	if in.Member != DB.Config.GetLocalNode() {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	member := DB.MemberList.GetMemberByHostname(in.Member)
	if member == nil {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	success := member.MakeActive()
	DB.Logging.Info(in.Member + " has been promoted to active")
	return &proto.PulsePromote{
		Success: success,
	}, nil
}

/**
Make a member passive
*/
func (s *Server) MakePassive(ctx context.Context, in *proto.PulsePromote) (*proto.PulsePromote, error) {
	DB.Logging.Debug("Server:MakePassive() Making node passive")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	if in.Member != DB.Config.GetLocalNode() {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	member := DB.MemberList.GetMemberByHostname(in.Member)
	if member == nil {
		return &proto.PulsePromote{
			Success: false,
		}, nil
	}
	success := member.MakePassive()
	DB.Logging.Info(in.Member + " has been demoted to passive")
	return &proto.PulsePromote{
		Success: success,
	}, nil
}

/**

 */
func (s *Server) BringUpIP(ctx context.Context, in *proto.PulseBringIP) (*proto.PulseBringIP, error) {
	DB.Logging.Debug("Server:BringUpIP() Bringing up IP(s)")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	err := BringUpIPs(in.Iface, in.Ips)
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
	DB.Logging.Debug("Server:BringDownIP() Bringing down IP(s)")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	err := BringDownIPs(in.Iface, in.Ips)
	success := false
	msg := "success"
	if err != nil {
		success = true
		msg = err.Error()
	}
	return &proto.PulseBringIP{Success: success, Message: msg}, nil
}

// Logs Listens for new log entries and displays them in journal
func (s *Server) Logs(ctx context.Context, in *proto.PulseLogs) (*proto.PulseLogs, error) {
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	// Log the incoming errors
	switch in.Level {
	case proto.PulseLogs_DEBUG:
		log.Debugf("[%s] %s", in.Node, in.Message)
		break;
	case proto.PulseLogs_INFO:
		log.Infof("[%s] %s", in.Node, in.Message)
		break;
	case proto.PulseLogs_ERROR:
		log.Errorf("[%s] %s", in.Node, in.Message)
		break;
	case proto.PulseLogs_WARNING:
		log.Warnf("[%s] %s", in.Node, in.Message)
		break;
	}
	return &proto.PulseLogs{}, nil
}
