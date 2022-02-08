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
	"github.com/syleron/pulseha/packages/language"
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

// Server defines our PulseHA server object.
type Server struct {
	sync.Mutex
	Server      *grpc.Server
	Listener    net.Listener
	HCScheduler func()
}

// Init used to start the bootstrap process
func (s *Server) Init(db *Database) {
	// Set our config
	DB = db
	// Setup/Load plugins
	DB.Plugins.Setup()
	// Setup the server
	s.Setup()
}

// Setup used to bootstrap the PulseHA server object.
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
		log.Fatal(err.Error())
		return
	}
	// load member cert/key
	peerCert, err := tls.LoadX509KeyPair(security.CertDir+"pulseha.crt", security.CertDir+"pulseha.key")
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
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{peerCert},
		ClientCAs:          caCertPool,
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

// serverInterceptor used to intercept each request.
// Note: We check to make sure we have a valid cert for each request
//       except the Join request.
func (s *Server) serverInterceptor(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (interface{}, error) {

	log.Debugf("Request received - Method: %s", info.FullMethod)

	// Skip authorize when join is requested
	if info.FullMethod != "/proto.Server/Join" {
		peer, ok := peer.FromContext(ctx)
		if ok {
			tlsInfo := peer.AuthInfo.(credentials.TLSInfo)
			if tlsInfo.SecurityLevel != credentials.PrivacyAndIntegrity {
				return nil, errors.New("invalid permissions")
			}
		}
	}

	// Calls the handler
	h, err := handler(ctx, req)

	return h, err
}

// Shutdown used to shutdown the PulseHA server listener.
// Note: This is not the CLI/CMD server listener.
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

// HealthCheck command used to receive the main RPC health check.
func (s *Server) HealthCheck(ctx context.Context, in *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	DB.Logging.Debug("Server:HealthCheck() Receiving health check")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return &rpc.HealthCheckResponse{}, errors.New(language.CLUSTER_UNATHORIZED)
	}
	activeHostname, _ := DB.MemberList.GetActiveMember()
	localMember, _ := DB.MemberList.GetLocalMember()
	if activeHostname != localMember.Hostname {
		localMember := DB.MemberList.GetMemberByHostname(localMember.Hostname)
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
		if hostname != localMember.Hostname {
			if err := localMember.MakePassive(); err != nil {
				DB.Logging.Error(err.Error())
			}
		} else {
			localMember.SetLastHCResponse(time.Time{})
		}
	}
	return &rpc.HealthCheckResponse{
		Score: int32(localMember.Score),
	}, nil
}

// Join command used to join a configured cluster.
func (s *Server) Join(ctx context.Context, in *rpc.JoinRequest) (*rpc.JoinResponse, error) {
	DB.Logging.Debug("Server:Join() " + strconv.FormatBool(in.Replicated) + " - Join Pulse cluster")
	s.Lock()
	defer s.Unlock()
	// Make sure we are in a cluster
	if DB.Config.ClusterCheck() {
		// Validate our cluster token
		if !security.SHA256StringValidation(in.Token, DB.Config.Pulse.ClusterToken) {
			DB.Logging.Warn(in.Uid + " attempted to join with an invalid cluster token")
			return &rpc.JoinResponse{
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
			return &rpc.JoinResponse{
				Success: false,
				Message: "Unable to unmarshal config node.",
			}, nil
		}
		// Make sure the node doesn't already exist
		if nodeExistsByUUID(in.Uid) {
			return &rpc.JoinResponse{
				Success: false,
				Message: "unable to join cluster as a node with id " + in.Uid + " already exists!",
			}, nil
		}
		// TODO: Node validation?
		// Add node to config
		if err = nodeAdd(in.Uid, originNode); err != nil {
			return &rpc.JoinResponse{
				Success: false,
				Message: "Failed to add new node to membership list",
			}, nil
		}
		// Save our new config to file
		if err := DB.Config.Save(); err != nil {
			if nodeExistsByUUID(in.Uid) {
				if err := nodeDelete(in.Uid); err != nil {
					return &rpc.JoinResponse{
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
			return &rpc.JoinResponse{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		// Attempt to read our CA details
		caCert, err := ioutil.ReadFile(security.CertDir + "ca.crt")
		if err != nil {
			log.Fatalf("Unable to load ca.crt: %s", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		// Make sure our CA cert files exist
		if !utils.CheckFileExists(security.CertDir+"ca.crt") ||
			!utils.CheckFileExists(security.CertDir+"ca.key") {
			log.Fatal("ca.crt and or ca.key does not exists")
			return &rpc.JoinResponse{
				Success: false,
				Message: "Unable to gather TLS details to join the cluster",
			}, nil
		}
		// Load our cert details
		caKey, err := ioutil.ReadFile(security.CertDir + "ca.key")
		if err != nil {
			log.Fatalf("Unable to load ca.key: %s", err)
			return &rpc.JoinResponse{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		DB.Logging.Info(in.Uid + " has joined the cluster")
		return &rpc.JoinResponse{
			Success: true,
			Message: "Successfully added ",
			Config:  buf,
			CaCrt:   string(caCert),
			CaKey:   string(caKey),
		}, nil
	}
	return &rpc.JoinResponse{
		Success: false,
		Message: "This node is not in a configured cluster.",
	}, nil
}

// Leave command used to leave from the current configured cluster.
func (s *Server) Leave(ctx context.Context, in *rpc.LeaveRequest) (*rpc.LeaveResponse, error) {
	DB.Logging.Debug("Server:Leave() " + strconv.FormatBool(in.Replicated) + " - Node leave cluster")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	// Get node by hostname
	id, _, _ := DB.Config.GetNodeByHostname(in.Hostname)
	// Remove from our member list
	DB.MemberList.MemberRemoveByHostname(in.Hostname)
	// Remove from our config
	if err := nodeDelete(id); err != nil {
		return &rpc.LeaveResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	if err := DB.Config.Save(); err != nil {
		log.Fatal(err)
	}
	DB.Logging.Info("Successfully removed " + in.Hostname + " from the cluster")
	return &rpc.LeaveResponse{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

// Remove command used to remove node from cluster by hostname.
func (s *Server) Remove(ctx context.Context, in *rpc.RemoveRequest) (*rpc.RemoveResponse, error) {
	DB.Logging.Debug("Server:Remove() " + strconv.FormatBool(in.Replicated) + " - Remove " + in.Hostname + "from cluster")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	// Make sure we can get our own hostname
	localHostname, err := utils.GetHostname()
	if err != nil {
		DB.Logging.Debug("Server:Remove() Fail. Unable to get local hostname to remove node from cluster")
		return &rpc.RemoveResponse{
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
			return &rpc.RemoveResponse{
				Success: false,
				Message: err.Error(),
			}, nil
		}
	}
	DB.Config.Save()
	DB.Logging.Info("Successfully removed node " + in.Hostname + " from the cluster")
	return &rpc.RemoveResponse{
		Success: true,
		Message: "Successfully removed node from local config",
	}, nil
}

// ConfigSync command used to take a config copy and update local.
func (s *Server) ConfigSync(ctx context.Context, in *rpc.ConfigSyncRequest) (*rpc.ConfigSyncResponse, error) {
	DB.Logging.Debug("Server:ConfigSync() " + strconv.FormatBool(in.Replicated) + " - Sync cluster config")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	// Define new node
	newConfig := &config.Config{}
	// unmarshal byte data to new node
	err := json.Unmarshal(in.Config, newConfig)
	// Handle failure to marshal config
	if err != nil {
		log.Fatalf("Unable to marshal config: %s", err)
		return &rpc.ConfigSyncResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	// !!!IMPORTANT!!!: Do not replace our local config
	newConfig.Pulse = DB.Config.Pulse
	// !!!IMPORTANT!!!: Do not replace our plugins config
	newConfig.Plugins = DB.Config.Plugins
	// Set our new config in memory
	DB.SetConfig(newConfig)
	// Save our config to file
	DB.Config.Save()
	// Update our member list
	DB.MemberList.Reload()
	// Let the logs know
	DB.Logging.Debug("Successfully r-synced local config")
	// Return with yay
	return &rpc.ConfigSyncResponse{
		Success: true,
	}, nil
}

// Promote command used to make the local node active.
func (s *Server) Promote(ctx context.Context, in *rpc.PromoteRequest) (*rpc.PromoteResponse, error) {
	DB.Logging.Debug("Server:MakeActive() Making node active")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		return &rpc.PromoteResponse{
			Success: false,
		}, nil
	}
	if in.Member != localNode.Hostname {
		return &rpc.PromoteResponse{
			Success: false,
		}, nil
	}
	member := DB.MemberList.GetMemberByHostname(in.Member)
	if member == nil {
		return &rpc.PromoteResponse{
			Success: false,
		}, nil
	}
	if err := member.MakeActive(); err != nil {
		return &rpc.PromoteResponse{
			Success: false,
		}, nil
	}
	DB.Logging.Info(in.Member + " has been promoted to active")
	return &rpc.PromoteResponse{
		Success: true,
	}, nil
}

// MakePassive command used to attempt to make the local node passive.
func (s *Server) MakePassive(ctx context.Context, in *rpc.MakePassiveRequest) (*rpc.MakePassiveResponse, error) {
	DB.Logging.Debug("Server:MakePassive() Making node passive")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	localNode, err := DB.Config.GetLocalNode()
	if err != nil {
		return &rpc.MakePassiveResponse{
			Success: false,
		}, nil
	}
	if in.Member != localNode.Hostname {
		return &rpc.MakePassiveResponse{
			Success: false,
		}, nil
	}
	member := DB.MemberList.GetMemberByHostname(in.Member)
	if member == nil {
		return &rpc.MakePassiveResponse{
			Success: false,
		}, nil
	}
	err = member.MakePassive()
	DB.Logging.Info(in.Member + " has been demoted to passive")
	return &rpc.MakePassiveResponse{
		Success: err == nil,
	}, nil
}

// BringUpIP command to bring up any number of floating ips.
func (s *Server) BringUpIP(ctx context.Context, in *rpc.UpIpRequest) (*rpc.UpIpResponse, error) {
	DB.Logging.Debug("Server:BringUpIP() Bringing up IP(s)")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	success := true
	msg := "success"
	if err := BringUpIPs(in.Iface, in.Ips); err != nil {
		success = false
		msg = err.Error()
	}
	return &rpc.UpIpResponse{Success: success, Message: msg}, nil
}

// BringDownIP command to bring down any number of floating ips.
func (s *Server) BringDownIP(ctx context.Context, in *rpc.DownIpRequest) (*rpc.DownIpResponse, error) {
	DB.Logging.Debug("Server:BringDownIP() Bringing down IP(s)")
	s.Lock()
	defer s.Unlock()
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	success := true
	msg := "success"

	if err := BringDownIPs(in.Iface, in.Ips); err != nil {
		success = false
		msg = err.Error()
	}
	return &rpc.DownIpResponse{Success: success, Message: msg}, nil
}

// Logs Listens for new log entries and displays them in journal
func (s *Server) Logs(ctx context.Context, in *rpc.LogsRequest) (*rpc.LogsResponse, error) {
	if !CanCommunicate(ctx) {
		return nil, errors.New(language.CLUSTER_UNATHORIZED)
	}
	// Log the incoming errors
	switch in.Level {
	case rpc.LogsRequest_DEBUG:
		log.Debugf("[%s] %s", in.Node, in.Message)
		break
	case rpc.LogsRequest_INFO:
		log.Infof("[%s] %s", in.Node, in.Message)
		break
	case rpc.LogsRequest_ERROR:
		log.Errorf("[%s] %s", in.Node, in.Message)
		break
	case rpc.LogsRequest_WARNING:
		log.Warnf("[%s] %s", in.Node, in.Message)
		break
	}
	return &rpc.LogsResponse{}, nil
}

// Describe command to return current node details
func (s *Server) Describe(ctx context.Context, in *rpc.DescribeRequest) (*rpc.DescribeResponse, error) {
	return &rpc.DescribeResponse{}, nil
}
