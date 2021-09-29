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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"io/ioutil"
	"net"
	"os"
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
	if !DB.Config.ClusterCheck() {
		log.Info("PulseHA is currently un-configured.")
		return
	}
	// Get our hostname
	hostname, err := utils.GetHostname()
	if err != nil {
		log.Error("cannot setup server because unable to get local hostname")
		return
	}
	// Make sure our local node is setup and available
	if exists := nodeExistsByHostname(hostname); !exists {
		log.Error("cannot find local hostname in pulse cluster config")

		if len(DB.Config.Nodes) > 0 {
			// We have other members but our self... something's wrong
			log.Error("Hanging config detected. Other nodes are defined but not the local member.")
		}
		os.Exit(1)
	}
	bindIP := utils.FormatIPv6(DB.Config.LocalNode().IP)
	// Listen
	s.Listener, err = net.Listen("tcp", bindIP+":"+DB.Config.LocalNode().Port)
	if err != nil {
		panic(err)
		return
	}
	// load member cert/key
	peerCert, err := tls.LoadX509KeyPair(security.CertDir+"server.crt", security.CertDir+"server.key")
	if err != nil {
		log.Fatalf("load peer cert/key error:%v", err)
		return
	}
	// Load CA cert
	caCert, err := ioutil.ReadFile(security.CertDir + "ca.crt")
	if err != nil {
		log.Fatalf("read ca cert file error:%v", err)
		return
	}
	// Define cert pool
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
		log.Fatal("failed to append certs")
		return
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{peerCert},
		ClientCAs:    caCertPool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
	})
	s.Server = grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(s.serverInterceptor),
	)
	// Register proto server handlers
	rpc.RegisterServerServer(s.Server, s)
	// Set our start delay
	DB.StartDelay = true
	// Setup our members
	DB.MemberList.Setup()
	// Start PulseHA daemon server
	log.Info("PulseHA initialised on " + DB.Config.LocalNode().IP + ":" + DB.Config.LocalNode().Port)
	if err := s.Server.Serve(s.Listener); err != nil {
		log.Fatalf("grpc serve error: %s", err)
	}
}

// serverInterceptor - Intercept each request to ensure only Join can be used without a verified cert
func (s *Server) serverInterceptor(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (interface{}, error) {

	log.Debugf("Request - Method:%s", info.FullMethod)

	// Skip authorize when join is requested
	if info.FullMethod != "/proto.Server/Join" {
		peer, ok := peer.FromContext(ctx)
		if ok {
			tlsInfo := peer.AuthInfo.(credentials.TLSInfo)
			v := tlsInfo.State.PeerCertificates
			if len(v) <= 0 {
				return nil, errors.New("invalid permissions")
			}
		}
	}

	// Calls the handler
	h, err := handler(ctx, req)

	return h, err
}

/**
 * Shutdown pulse server (not cli/cmd)
 */
func (s *Server) Shutdown() {
	log.Info("Shutting down PulseHA daemon")
	// Make passive
	if DB.Config.ClusterCheck() {
		MakeLocalPassive()
	}
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
func (s *Server) HealthCheck(ctx context.Context, in *rpc.PulseHealthCheck) (*rpc.PulseHealthCheck, error) {
	DB.Logging.Debug("Server:HealthCheck() Receiving health check")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	activeHostname, _ := DB.MemberList.GetActiveMember()
	localNode := DB.Config.GetLocalNode()
	if activeHostname != localNode.Hostname {
		localMember := DB.MemberList.GetMemberByHostname(localNode.Hostname)
		// make passive to reset the networking
		// TODO: Figure out why I do this here
		//if _, activeMember := DB.MemberList.GetActiveMember(); activeMember == nil {
		//	DB.Logging.Info("Local node is passive")
		//	//localMember.MakePassive()
		//}
		localMember.SetLastHCResponse(time.Now())
		DB.MemberList.Update(in.Memberlist)
	} else {
		DB.Logging.Warn("Active node mismatch")
		hostname := GetFailOverCountWinner(in.Memberlist)
		DB.Logging.Info("Member " + hostname + " has been determined as the correct active node.")
		if hostname != localNode.Hostname {
			member, _ := DB.MemberList.GetLocalMember()
			member.MakePassive()
		} else {
			localMember, _ := DB.MemberList.GetLocalMember()
			localMember.SetLastHCResponse(time.Time{})
		}
	}
	return &rpc.PulseHealthCheck{
		Success: true,
	}, nil
}

/**
Join request for a configured cluster
*/
func (s *Server) Join(ctx context.Context, in *rpc.PulseJoin) (*rpc.PulseJoin, error) {
	DB.Logging.Debug("Server:Join() " + strconv.FormatBool(in.Replicated) + " - Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	// Make sure we are in a cluster
	if DB.Config.ClusterCheck() {
		// Validate our cluster token
		if !security.SHA256StringValidation(in.Token, DB.Config.Pulse.ClusterToken) {
			DB.Logging.Warn(in.Hostname + " attempted to join with an invalid cluster token")
			return &rpc.PulseJoin{
				Success: false,
				Message: "Invalid cluster token",
			}, nil
		}
		// Define new node
		originNode := &config.Node{}
		// unmarshal byte data to new node
		err := json.Unmarshal(in.Config, originNode)
		// handle errors
		if err != nil {
			log.Error("Unable to unmarshal config node.")
			return &rpc.PulseJoin{
				Success: false,
				Message: "Unable to unmarshal config node.",
			}, nil
		}
		// Make sure the node doesnt already exist
		if nodeExistsByHostname(in.Hostname) {
			return &rpc.PulseJoin{
				Success: false,
				Message: "unable to join cluster as a node with hostname " + in.Hostname + " already exists!",
			}, nil
		}
		// TODO: Node validation?
		// Add node to config
		if err = nodeAdd(in.Uid, in.Hostname, originNode); err != nil {
			return &rpc.PulseJoin{
				Success: false,
				Message: "Failed to add new node to membership list",
			}, nil
		}
		// Save our new config to file
		if err := DB.Config.Save(); err != nil {
			if nodeExistsByHostname(in.Hostname) {
				if err := nodeDelete(in.Hostname); err != nil {
					return &rpc.PulseJoin{
						Success: false,
						Message: "Failed to clean up after a failed join attempt",
					}, nil
				}
			}
		}
		// Update the cluster config
		if err := DB.MemberList.SyncConfig(); err != nil {
			DB.Logging.Warn("Unable to sync config when a join attempt was made")
		}
		// Add node to the memberlist
		DB.MemberList.Reload()
		// Return with our new updated config
		buf, err := json.Marshal(DB.Config)
		// Handle failure to marshal config
		if err != nil {
			log.Fatalf("Unable to marshal config: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		// Attempt to read our CA details
		caCert, err := ioutil.ReadFile(security.CertDir + "ca.crt")
		if err != nil {
			log.Fatalf("Unable to load ca.crt: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		// Make sure our CA cert files exist
		if !utils.CheckFileExists(security.CertDir+ "ca.crt") ||
			!utils.CheckFileExists(security.CertDir+ "ca.key") {
			log.Fatal("ca.crt and or ca.key does not exists")
			return &rpc.PulseJoin{
				Success: false,
				Message: "Unable to gather TLS details to join the cluster",
			}, nil
		}
		// Load our cert details
		caKey, err := ioutil.ReadFile(security.CertDir + "ca.key")
		if err != nil {
			log.Fatalf("Unable to load ca.key: %s", err)
			return &rpc.PulseJoin{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		DB.Logging.Info(in.Hostname + " has joined the cluster")
		return &rpc.PulseJoin{
			Success: true,
			Message: "Successfully added ",
			Config:  buf,
			CaCrt:   string(caCert),
			CaKey:   string(caKey),
		}, nil
	}
	return &rpc.PulseJoin{
		Success: false,
		Message: "This node is not in a configured cluster.",
	}, nil
}

/**
Update our local config from a Resync request
*/
func (s *Server) Leave(ctx context.Context, in *rpc.PulseLeave) (*rpc.PulseLeave, error) {
	DB.Logging.Debug("Server:Leave() " + strconv.FormatBool(in.Replicated) + " - Node leave cluster")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	// Remove from our memberlist
	DB.MemberList.MemberRemoveByHostname(in.Hostname)
	// Remove from our config
	err := nodeDelete(in.Hostname)
	if err != nil {
		return &rpc.PulseLeave{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	DB.Config.Save()
	DB.Logging.Info("Successfully removed " + in.Hostname + " from the cluster")
	return &rpc.PulseLeave{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

// Remove - Remove node from cluster by hostname
func (s *Server) Remove(ctx context.Context, in *rpc.PulseRemove) (*rpc.PulseRemove, error) {
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
		return &rpc.PulseRemove{
			Success: false,
			Message: "Unable to perform remove as unable to get local hostname",
		}, nil
	}
	// Set our member status
	member := DB.MemberList.GetMemberByHostname(in.Hostname)
	member.SetStatus(rpc.MemberStatus_LEAVING)
	if in.Hostname == localHostname {
		s.Shutdown()
		nodesClearLocal()
		log.Info("Successfully removed " + in.Hostname + " from cluster. PulseHA no longer listening..")
	} else {
		// Remove from our memberlist
		DB.MemberList.MemberRemoveByHostname(in.Hostname)
		// Remove from our config
		err := nodeDelete(in.Hostname)
		if err != nil {
			return &rpc.PulseRemove{
				Success: false,
				Message: err.Error(),
			}, nil
		}
	}
	DB.Config.Save()
	DB.Logging.Info("Successfully removed node " + in.Hostname + " from the cluster")
	return &rpc.PulseRemove{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

/**
Update our local config from a Resync request
*/
func (s *Server) ConfigSync(ctx context.Context, in *rpc.PulseConfigSync) (*rpc.PulseConfigSync, error) {
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
		log.Fatalf("Unable to marshal config: %s", err)
		return &rpc.PulseConfigSync{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	// !!!IMPORTANT!!!: Do not replace our local config
	newConfig.Pulse.ClusterToken = DB.Config.Pulse.ClusterToken
	newConfig.Pulse.LocalNode = DB.Config.Pulse.LocalNode
	newConfig.Pulse.LoggingLevel = DB.Config.Pulse.LoggingLevel
	// Set our new config in memory
	DB.SetConfig(newConfig)
	// Save our config to file
	DB.Config.Save()
	// Update our member list
	DB.MemberList.Reload()
	// Let the logs know
	DB.Logging.Debug("Successfully r-synced local config")
	// Return with yay
	return &rpc.PulseConfigSync{
		Success: true,
	}, nil
}

/**
Network action functions
*/
func (s *Server) Promote(ctx context.Context, in *rpc.PulsePromote) (*rpc.PulsePromote, error) {
	DB.Logging.Debug("Server:MakeActive() Making node active")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	localNode := DB.Config.GetLocalNode()
	if in.Member != localNode.Hostname {
		return &rpc.PulsePromote{
			Success: false,
		}, nil
	}
	member := DB.MemberList.GetMemberByHostname(in.Member)
	if member == nil {
		return &rpc.PulsePromote{
			Success: false,
		}, nil
	}
	if err := member.MakeActive(); err != nil {
		return &rpc.PulsePromote{
			Success: false,
		}, nil
	}
	DB.Logging.Info(in.Member + " has been promoted to active")
	return &rpc.PulsePromote{
		Success: true,
	}, nil
}

/**
Make a member passive
*/
func (s *Server) MakePassive(ctx context.Context, in *rpc.PulsePromote) (*rpc.PulsePromote, error) {
	DB.Logging.Debug("Server:MakePassive() Making node passive")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	localNode := DB.Config.GetLocalNode()
	if in.Member != localNode.Hostname {
		return &rpc.PulsePromote{
			Success: false,
		}, nil
	}
	member := DB.MemberList.GetMemberByHostname(in.Member)
	if member == nil {
		return &rpc.PulsePromote{
			Success: false,
		}, nil
	}
	err := member.MakePassive()
	DB.Logging.Info(in.Member + " has been demoted to passive")
	return &rpc.PulsePromote{
		Success: err == nil,
	}, nil
}

/**

 */
func (s *Server) BringUpIP(ctx context.Context, in *rpc.PulseBringIP) (*rpc.PulseBringIP, error) {
	DB.Logging.Debug("Server:BringUpIP() Bringing up IP(s)")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	success := true
	msg := "success"
	if err := BringUpIPs(in.Iface, in.Ips); err != nil {
		success = false
		msg = err.Error()
	}
	return &rpc.PulseBringIP{Success: success, Message: msg}, nil
}

/**

 */
func (s *Server) BringDownIP(ctx context.Context, in *rpc.PulseBringIP) (*rpc.PulseBringIP, error) {
	DB.Logging.Debug("Server:BringDownIP() Bringing down IP(s)")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	success := true
	msg := "success"

	if err := BringDownIPs(in.Iface, in.Ips); err != nil {
		success = false
		msg = err.Error()
	}
	return &rpc.PulseBringIP{Success: success, Message: msg}, nil
}

// Logs Listens for new log entries and displays them in journal
func (s *Server) Logs(ctx context.Context, in *rpc.PulseLogs) (*rpc.PulseLogs, error) {
	if !CanCommunicate(ctx) {
		return nil, errors.New("unauthorized")
	}
	// Log the incoming errors
	switch in.Level {
	case rpc.PulseLogs_DEBUG:
		log.Debugf("[%s] %s", in.Node, in.Message)
		break
	case rpc.PulseLogs_INFO:
		log.Infof("[%s] %s", in.Node, in.Message)
		break
	case rpc.PulseLogs_ERROR:
		log.Errorf("[%s] %s", in.Node, in.Message)
		break
	case rpc.PulseLogs_WARNING:
		log.Warnf("[%s] %s", in.Node, in.Message)
		break
	}
	return &rpc.PulseLogs{}, nil
}

func (s *Server) Describe(ctx context.Context, in *rpc.PulseDescribe) (*rpc.PulseDescribe, error) {
	return &rpc.PulseDescribe{}, nil
}
