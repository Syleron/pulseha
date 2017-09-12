package main

import (
	"sync"
	"context"
	"google.golang.org/grpc"
	"net"
	"google.golang.org/grpc/credentials"
	"os"
	"time"
	"github.com/coreos/go-log/log"
	"github.com/Syleron/Pulse/proto"
)

type Server struct {
	sync.Mutex
	Status proto.HealthCheckResponse_ServingStatus
	Last_response time.Time
	Members []Member
	Config *Config
	Log log.Logger
}

/**
 * Member node struct type
 */
type Member struct {
	Name   string
	Addr   net.IP
	Port uint16
	State proto.HealthCheckResponse_ServingStatus
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
 *
 */
func (s * Server) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Join() - Join Pulse cluster")

	return &proto.PulseJoin{
		Success: true,
	}, nil
}

/**
 *
 */
func (s * Server) Leave(ctx context.Context, in *proto.PulseLeave) (*proto.PulseLeave, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Leave() - Leave Pulse cluster")
	return &proto.PulseLeave{}, nil
}

/**
 *
 */
func (s * Server) Create(ctx context.Context, in *proto.PulseCreate) (*proto.PulseCreate, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Create() - Create Pulse cluster")
	// Method of first checking to see if we are in a cluster.
	if !_clusterCheck(s.Config) {
		// create a group
		s.Config.Groups["group1"] = []string{}
		// we are not in an active cluster
		newNode := Node{
			IP: in.BindIp,
			Port: in.BindPort,
			IPGroups: make(map[string][]string, 0),
		}
		// Assign interface names to node
		for _, name := range _getInterfaceNames() {
			if name != "lo" {
				newNode.IPGroups[name] = make([]string, 0)
			}
		}
		// Add the node to the nodes config
		s.Config.Nodes[GetHostname()] = newNode
		// Save the config
		s.Config.Save()
		// Setup the listener
		go s.Setup()
		// return if we were successful or not
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
 *
 */
func (s *Server) NewGroup(ctx context.Context, in *proto.PulseGroupNew) (*proto.PulseGroupNew, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:NewGroup() - Create floating IP group")

	// Check to make sure we are in a cluster
	if _clusterCheck(s.Config) {
		// Generate a new name.
		// Check to see if the group name has already been used!
		// Define the new group within our config
		s.Config.Groups[_genGroupName(s.Config)] = []string{}
		// Save to the config
		s.Config.Save()
		// Note: Do we need to reload?
		return &proto.PulseGroupNew{
			Success: true,
			Message: "Group successfully added.",
		}, nil
	} else {
		return &proto.PulseGroupNew{
			Success: false,
			Message: "Groups can only be created in a configured cluster.",
		}, nil
	}
}

/**
 *
 */
func (s *Server) DeleteGroup(ctx context.Context, in *proto.PulseGroupDelete) (*proto.PulseGroupDelete, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:DeleteGroup() - Delete floating IP group")
	if _groupExist(in.Name, s.Config) {
		delete(s.Config.Groups, in.Name)
		s.Config.Save()
		// Note: May need to reload!
		return &proto.PulseGroupDelete{
			Success: true,
			Message: "Group " + in.Name + " successfully deleted.",
		}, nil
	} else {
		return &proto.PulseGroupDelete{
			Success: false,
			Message: "Unable to delete group that doesn't exist!",
		}, nil
	}
}

/**
 *
 */
func (s *Server) GroupIPAdd(ctx context.Context, in *proto.PulseGroupAdd) (*proto.PulseGroupAdd, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:GroupIPAdd() - Add IP addresses to group " + in.Name)
	// Make sure that the group exists
	if !_groupExist(in.Name, s.Config) {
		return &proto.PulseGroupAdd{
			Success: false,
			Message: "IP group does not exist!",
		}, nil
	}
	// find group and add ips
	for _, ip := range in.Ips {
		if ValidIPAddress(ip) {
			// Do we have at least one?
			if len(s.Config.Groups[in.Name]) > 0 {
				// Make sure we don't have any duplicates
				if exists, _ := _groupIPExist(in.Name, ip, s.Config); !exists {
					s.Config.Groups[in.Name] = append(s.Config.Groups[in.Name], ip)
				} else {
					log.Warning(ip + " already exists in group " + in.Name + ".. skipping.")
				}
			} else {
				s.Config.Groups[in.Name] = append(s.Config.Groups[in.Name], ip)
			}
		} else {
			log.Warning(ip + " is not a valid IP address")
		}
	}
	// save to config
	s.Config.Save()
	// Note: May need to reload the config
	return &proto.PulseGroupAdd{
		Success: true,
		Message: "IP addresses successfully added to group " + in.Name,
	}, nil
}

/**
 *
 */
func (s *Server) GroupIPRemove(ctx context.Context, in *proto.PulseGroupRemove) (*proto.PulseGroupRemove, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:GroupIPRemove() - Removing IPs from group " + in.Name)
	// Make sure that the group exists
	if !_groupExist(in.Name, s.Config) {
		return &proto.PulseGroupRemove{
			Success: false,
			Message: "IP group does not exist!",
		}, nil
	}
	// Find ips and remove them
	for _, ip := range in.Ips {
		// Do we have at least one?
		if len(s.Config.Groups[in.Name]) > 0 {
			// Make sure we don't have any duplicates
			if exists, i := _groupIPExist(in.Name, ip, s.Config); exists {
				s.Config.Groups[in.Name] = append(s.Config.Groups[in.Name][:i], s.Config.Groups[in.Name][i+1:]...)
			} else {
				log.Warning(ip + " does not exist in group " + in.Name + ".. skipping.")
			}
		}
	}
	// Save the config
	s.Config.Save()
	// Note: May need to reload the config
	return &proto.PulseGroupRemove{
		Success: true,
		Message: "IP addresses successfully removed from group " + in.Name,
	}, nil
}

/**
 *
 */
func (s *Server) GroupAssign(ctx context.Context, in *proto.PulseGroupAssign) (*proto.PulseGroupAssign, error) {
	return &proto.PulseGroupAssign{}, nil
}

/**
 *
 */
func (s *Server) GroupUnassign(ctx context.Context, in *proto.PulseGroupUnassign) (*proto.PulseGroupUnassign, error) {
	return &proto.PulseGroupUnassign{}, nil
}
/**
 *
 */
func (s *Server) SetupCLI() {
	lis, err := net.Listen("tcp", "127.0.0.1:9443")
	if err != nil {
		log.Errorf("Failed to listen: %s", err)
	}
	grpcServer := grpc.NewServer()
	proto.RegisterRequesterServer(grpcServer, s)
	log.Info("CLI initialised on 127.0.0.1:9443")
	grpcServer.Serve(lis)
}

/**
 *
 */
func (s *Server) Setup() {
	// Only continue if we are in a configured cluster
	if !_clusterCheck(s.Config) {
		return
	}

	lis, err := net.Listen("tcp", s.Config.LocalNode().IP+":"+s.Config.LocalNode().Port)

	if err != nil {
		log.Errorf("Failed to listen: %s", err)
		os.Exit(1)
	}

	var grpcServer *grpc.Server
	if s.Config.Pulse.TLS {
		if CreateFolder("./certs") {
			log.Warning("TLS keys are missing! Generating..")
			GenOpenSSL()
		}

		creds, err := credentials.NewServerTLSFromFile("./certs/server.crt", "./certs/server.key")

		if err != nil {
			log.Error("Could not load TLS keys.")
			os.Exit(1)
		}

		grpcServer = grpc.NewServer(grpc.Creds(creds))
	} else {
		grpcServer = grpc.NewServer()
	}

	proto.RegisterRequesterServer(grpcServer, s)

	log.Info("Pulse initialised on " + s.Config.LocalNode().IP + ":" + s.Config.LocalNode().Port)

	grpcServer.Serve(lis)
}
