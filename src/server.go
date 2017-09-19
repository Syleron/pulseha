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
	"github.com/Syleron/PulseHA/proto"
	"path/filepath"
)

/**
 * Server struct type
 */
type Server struct {
	sync.Mutex
	Status proto.HealthCheckResponse_ServingStatus
	Last_response time.Time
	Members []Member
	Config *Config
	Log log.Logger
	Server *grpc.Server
	Listener net.Listener
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
 * Attempt to join a configured cluster
 */
func (s * Server) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Join() - Join Pulse cluster")

	// Are we configured?
	if _clusterCheck(s.Config) {
		// This is called by our local daemon/agent
		// It needs to send a request to the peer/node to get cluster details.
		// Add the node to the config
		// Notify our peers that a new member has joined
		return &proto.PulseJoin{
			Success: true,
		}, nil
	}

	return &proto.PulseJoin{
		Success: false,
		Message: "Unable to join as node is not in a configured cluster",
	}, nil
}

/**
 * Break cluster / Leave from cluster
 */
func (s * Server) Leave(ctx context.Context, in *proto.PulseLeave) (*proto.PulseLeave, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Leave() - Leave Pulse cluster")

	nodeTotal := len(s.Config.Nodes)

	// Are we even in a cluster?
	if nodeTotal == 0 {
		return &proto.PulseLeave{
			Success: false,
			Message: "Unable to leave as no cluster was found",
		}, nil
	}

	// Clear out the groups
	s.Config.Groups = map[string][]string{}
	// Clear out the nodes
	s.Config.Nodes = map[string]Node{}
	// save our config
	s.Config.Save()
	// Shutdown our main server
	s.Close()

	// Check to see if we are the only member in the cluster
	if nodeTotal == 1 {
		return &proto.PulseLeave{
			Success: true,
			Message: "Successfully dismantled cluster",
		}, nil
	}

	// We need to inform our peers that we have left!
	return &proto.PulseLeave{
		Success: true,
		Message: "Successfully left from cluster",
	}, nil
}

/**
 * Note: This will probably need to be replicated..
 */
func (s * Server) Create(ctx context.Context, in *proto.PulseCreate) (*proto.PulseCreate, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Create() - Create Pulse cluster")
	// Method of first checking to see if we are in a cluster.
	if !_clusterCheck(s.Config) {
		// we are not in an active cluster
		newNode := Node{
			IP: in.BindIp,
			Port: in.BindPort,
			IPGroups: make(map[string][]string, 0),
		}
		// Add the node to the nodes config
		s.Config.Nodes[GetHostname()] = newNode
		// Assign interface names to node
		for _, ifaceName := range _getInterfaceNames() {
			if ifaceName != "lo" {
				// Add the interface to the node
				newNode.IPGroups[ifaceName] = make([]string, 0)
				// Create a new group name
				groupName := _genGroupName(s.Config)
				// Create a group for the interface
				s.Config.Groups[groupName] = []string{}
				// assign the group to the interface
				s.assignGroupToNode(GetHostname(), ifaceName, groupName)
			}
		}
		// Save the config
		s.Config.Save()
		// Reload the config
		//s.Config.Reload()
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
 * Add a new floating IP group
 * Note: This will probably need to be replicated..
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
		groupName := _genGroupName(s.Config)
		s.Config.Groups[groupName] = []string{}
		// Save to the config
		s.Config.Save()
		// Note: Do we need to reload?
		return &proto.PulseGroupNew{
			Success: true,
			Message: groupName + " successfully added.",
		}, nil
	} else {
		return &proto.PulseGroupNew{
			Success: false,
			Message: "Groups can only be created in a configured cluster.",
		}, nil
	}
}

/**
 * Delete floating IP group
 */
func (s *Server) DeleteGroup(ctx context.Context, in *proto.PulseGroupDelete) (*proto.PulseGroupDelete, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:DeleteGroup() - Delete floating IP group")
	if _groupExist(in.Name, s.Config) {
		// Check to see if we are assigned to an interface
		if !_nodeAssignedToInterface(in.Name, s.Config) {
			delete(s.Config.Groups, in.Name)
		} else {
			return &proto.PulseGroupDelete{
				Success: false,
				Message: "Group has network interface assignments. Please remove them and try again.",
			}, nil
		}
		// Save config
		s.Config.Save()
		// Note: May need to reload!
		return &proto.PulseGroupDelete{
			Success: true,
			Message: in.Name + " successfully deleted.",
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
 * Note: This will probably need to be replicated..
 */
func (s *Server) GroupIPAdd(ctx context.Context, in *proto.PulseGroupAdd) (*proto.PulseGroupAdd, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:GroupIPAdd() - Add IP addresses to group " + in.Name)
	// Make sure that the group exists
	if !_groupExist(in.Name, s.Config) {
		return &proto.PulseGroupAdd{
			Success: false,
			Message: "Group does not exist!",
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
		Message: "IP address(es) successfully added to " + in.Name,
	}, nil
}

/**
 *
 * Note: This will probably need to be replicated..
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
		Message: "IP address(es) successfully removed from " + in.Name,
	}, nil
}

/**
 *
 * Note: This will probably need to be replicated..
 */
func (s *Server) GroupAssign(ctx context.Context, in *proto.PulseGroupAssign) (*proto.PulseGroupAssign, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:GroupAssign() - Assigning group " + in.Group + " to interface " + in.Interface + " on node " + in.Node)
	// Make sure that the group exists
	if !_groupExist(in.Group, s.Config) {
		return &proto.PulseGroupAssign{
			Success: false,
			Message: "IP group does not exist!",
		}, nil
	}
	// Make sure the interface exists
	if !_interfaceExist(in.Interface) {
		return &proto.PulseGroupAssign{
			Success: false,
			Message: "Interface does not exist!",
		}, nil
	}
	// Assign group to the interface
	s.assignGroupToNode(in.Node, in.Interface, in.Group)
	// save to config
	s.Config.Save()
	// Note: May need to reload the config
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
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:GroupUnassign() - Unassigning group " + in.Group + " from interface " + in.Interface + " on node " + in.Node)
	// Make sure that the group exists
	if !_groupExist(in.Group, s.Config) {
		return &proto.PulseGroupUnassign{
			Success: false,
			Message: "IP group does not exist!",
		}, nil
	}
	// Make sure the interface exists
	if !_interfaceExist(in.Interface) {
		return &proto.PulseGroupUnassign{
			Success: false,
			Message: "Interface does not exist!",
		}, nil
	}
	// Assign group to the interface
	s.unassignGroupFromNode(in.Node, in.Interface, in.Group)
	// save to config
	s.Config.Save()
	// Note: May need to reload the config
	return &proto.PulseGroupUnassign{
		Success: true,
		Message: in.Group + " unassigned from interface " + in.Interface + " on node " + in.Node,
	}, nil
}

/**
 *
 */
func (s *Server) GroupList(ctx context.Context, in *proto.PulseGroupList) (*proto.PulseGroupList, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:GroupList() - Getting groups and their IPs")

	list := make(map[string]*proto.Group)

	for name, ips := range s.Config.Groups {
		list[name] = &proto.Group{
			Ips: ips,
		}
	}

	return &proto.PulseGroupList{
		Groups: list,
	}, nil
}

/**
 * Assigns a group to an interface.
 * Note: This function does not save to config file.
 */
func (s *Server) assignGroupToNode(node, iface, group string) {
	if exists, _ := _nodeInterfaceGroupExists(node, iface, group, s.Config); !exists {
		s.Config.Nodes[node].IPGroups[iface] = append(s.Config.Nodes[node].IPGroups[iface], group)
	} else {
		log.Warning(group + " already exists in node " + node + ".. skipping.")
	}
}

/**
 * Unassign a group from an interface
 * Note: This function does not save to config file.
 */
func (s * Server) unassignGroupFromNode(node, iface, group string) {
	if exists, i := _nodeInterfaceGroupExists(node, iface, group, s.Config); exists {
		s.Config.Nodes[node].IPGroups[iface] = append(s.Config.Nodes[node].IPGroups[iface][:i], s.Config.Nodes[node].IPGroups[iface][i+1:]...)
	} else {
		log.Warning(group + " does not exist in node " + node + ".. skipping.")
	}
}

/**
 * Setup pulse cli type
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
 * Setup pulse server type
 */
func (s *Server) Setup() {
	// Only continue if we are in a configured cluster
	if !_clusterCheck(s.Config) {
		return
	}

	var err error
	s.Listener, err = net.Listen("tcp", s.Config.LocalNode().IP+":"+s.Config.LocalNode().Port)

	if err != nil {
		log.Errorf("Failed to listen: %s", err)
		os.Exit(1)
	}

	if s.Config.Pulse.TLS {
		// Get project directory location
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			log.Emergency(err)
		}
		if CreateFolder(dir + "/certs") {
			log.Warning("TLS keys are missing! Generating..")
			GenOpenSSL()
		}

		creds, err := credentials.NewServerTLSFromFile(dir + "/certs/server.crt", dir + "/certs/server.key")

		if err != nil {
			log.Error("Could not load TLS keys.")
			os.Exit(1)
		}

		s.Server = grpc.NewServer(grpc.Creds(creds))
	} else {
		s.Server = grpc.NewServer()
	}

	proto.RegisterRequesterServer(s.Server, s)

	log.Info("Pulse initialised on " + s.Config.LocalNode().IP + ":" + s.Config.LocalNode().Port)

	s.Server.Serve(s.Listener)
}

/**
 * Shutdown pulse server (not cli/cmd)
 */
func (s *Server) Close() {
	log.Debug("Shutting down server")
	s.Server.GracefulStop()
	s.Listener.Close()
}
