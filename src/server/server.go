package server

import (
	log "github.com/Sirupsen/logrus"
	"net"
	hc "github.com/Syleron/Pulse/src/proto"
	"github.com/Syleron/Pulse/src/structures"
	"github.com/Syleron/Pulse/src/utils"
	"github.com/Syleron/Pulse/src/client"
	"github.com/Syleron/Pulse/src/networking"
	"github.com/Syleron/Pulse/src/security"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"sync"
	"google.golang.org/grpc/codes"
	"time"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/credentials"
	"io/ioutil"
	"os"
	"strconv"
	oglog "log"
)

var (
	Config	structures.Configuration
	Role string
	Lis  *net.Listener
	ServerIP string
	ServerPort string
	Last_response time.Time // Last time we got a health check from the master
	Status hc.HealthCheckResponse_ServingStatus // The status of the cluster
)

type server struct{
	mu sync.Mutex
	status hc.HealthCheckResponse_ServingStatus
}

func (s *server) Check(ctx context.Context, in *hc.HealthCheckRequest) (*hc.HealthCheckResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch(in.Request) {
	case hc.HealthCheckRequest_SETUP:
		log.Info("Recieved setup request from master..")

		if (configureCluster()) {
			// Reset the last_response time
			Last_response = time.Now()
			// Successfully configured the cluster... now to monitor for health checks
			go monitorResponses()
			// We return unknown as the request was not successful.
			return &hc.HealthCheckResponse{
				Status: hc.HealthCheckResponse_CONFIGURED,
			}, nil
		} else {
			// We return unknown as the request was not successful.
			return nil, grpc.Errorf(codes.PermissionDenied, "Slave has already been configured.")
			//return &hc.HealthCheckResponse{
			//	Status: hc.HealthCheckResponse_UNKNOWN,
			//}, nil
		}
	case hc.HealthCheckRequest_STATUS:
		// Make sure we are configured
		if Config.Local.Configured {
			// Reset the last_response time
			Last_response = time.Now()

			return &hc.HealthCheckResponse{
				Status: hc.HealthCheckResponse_HEALTHY,
			}, nil
		} else {
			return nil, grpc.Errorf(codes.PermissionDenied, "A setup request must be made before the slave can respond to health checks.")
		}
	default:
		return nil, grpc.Errorf(codes.NotFound, "unknown request")
	}
}

/**
 * Function is used to configure a clustered pair
 */
func configureCluster() bool{
	// Check to see if we can configure this node
	// make sure we are a slave
	if Config.Local.Role == "slave" {
		// Are we in a configured state already?
		if Config.Local.Configured == false {
			// Set the local value to configured
			Config.Local.Configured = true;

			// Save
			utils.SaveConfig(Config)

			log.Info("Successfully configured slave.")

			return true;
		} else {
			return false
		}
	}
	return false
}

/*
 * Setup Function used to initialise the server
 */
func Setup() {
	// Load the config and validate
	Config = utils.LoadConfig()
	Config.Validate()

	// Are we master or slave?

	// Setup local variables
	setupLocalVariables()
	
	lis, err := net.Listen("tcp", ":" + ServerPort)
	grpclog.SetLogger(oglog.New(ioutil.Discard, "", 0))

	if err != nil {
		log.Error("failed to listen: %v", err)
	}

	Lis = &lis
	
	var s *grpc.Server

	// Check to see if we have TLS enabled.
	if Config.Local.TLS {
		// Create folder and keys if we have to
		// Note: This should probably check if the key files exist as well.
		if utils.CreateFolder("./certs") {
			log.Warn("Missing TLS keys.")
			security.Generate()
		}
		
		// Specify GRPC credentials
	    creds, err := credentials.NewServerTLSFromFile("./certs/server.crt", "./certs/server.key")
	    
	    if err != nil {
	    	log.Fatal("Could not load TLS keys: ", err)
	    	os.Exit(1)
	    }
	    
	    // Start GRPC server
		s = grpc.NewServer(grpc.Creds(creds))
	} else {
		s = grpc.NewServer()
	}
	
	// Log message
	log.Info(Role + " initialised on port " + ServerPort + " TLS: " + strconv.FormatBool(Config.Local.TLS));

	hc.RegisterRequesterServer(s, &server{})

	// If we are a slave.. we need to set the starting time
	// and fail-over checker
	// Note: as go routine otherwise the server doesn't serve!
	go func() {
		if Config.Local.Role == "slave" && Config.Local.Configured {
			Last_response = time.Now()
			monitorResponses()
		}
	}()

	s.Serve(*Lis)
}

/**
 * Slave function - used to monitor when the last health check we received.
 */
func monitorResponses() {
	for _ = range time.Tick(time.Duration(Config.Local.FOCInterval) * time.Millisecond) {
		elapsed := int64(time.Since(Last_response)) / 1e9
		
		if int(elapsed) > 0 && int(elapsed)%4 == 0 {
			log.Warn("No health checks are being made.. Perhaps a failover is required?")
		}

		// If 30 seconds has gone by.. something is wrong.
		if int(elapsed) >= Config.Local.FOCLimit {
			var addHCSuccess bool = false

			// Try communicating with the master through other methods
			if Config.HealthChecks.ICMP != (structures.Configuration{}.HealthChecks.ICMP) {
				if Config.HealthChecks.ICMP.Enabled {
					success := networking.ICMPIPv4(Config.Cluster.Nodes.Master.IP)

					if success {
						log.Warn("ICMP health check successful! Assuming master is still available..")
						addHCSuccess = true
					}
				}
			}

			if Config.HealthChecks.HTTP != (structures.Configuration{}.HealthChecks.HTTP) {
				if Config.HealthChecks.HTTP.Enabled {
					success := networking.Curl(Config.HealthChecks.HTTP.Address)

					if success {
						log.Warn("HTTP health check successful! Assuming master is still available..")
						addHCSuccess = true
					}
				}
			}

			if !addHCSuccess {
				// Nothing has worked.. assume the master has failed. Fail over.
				log.Info("Attempting a failover..")
				failover()
				break
			} else {
				Last_response = time.Now()
			}
		}
	}
}

/**
 * Slave Function - Used when the master is no longer around.
 */
func failover() {
	if (Config.Local.Role == "slave") {
		// update local role
		Config.Local.Role = "master"

		// Update local status
		Status = hc.HealthCheckResponse_FAILVER

		// Update master and slave configuration
		Config.Cluster.Nodes.Slave.IP = Config.Cluster.Nodes.Master.IP
		Config.Cluster.Nodes.Slave.Port = Config.Cluster.Nodes.Master.Port

		Config.Cluster.Nodes.Master.IP = ServerIP
		Config.Cluster.Nodes.Master.Port = ServerPort

		// Save to file
		utils.SaveConfig(Config)

		log.Info("Completed. Local role has been re-assigned as master..")

		// Close server
		var serverListener net.Listener
		serverListener = *Lis
		serverListener.Close()

		time.Sleep(1000)

		// Re-setup the server
		go Setup()
		// Tell the client to reload the config
		go client.ForceConfigReload()
	}
}

/**
 * Function to setup the local variables for this client.
 */
func setupLocalVariables() {
	switch Config.Local.Role {
	case "master":
		ServerIP = Config.Cluster.Nodes.Master.IP
		ServerPort = Config.Cluster.Nodes.Master.Port
		Role = "master"
	case "slave":
		ServerIP = Config.Cluster.Nodes.Slave.IP
		ServerPort = Config.Cluster.Nodes.Slave.Port
		Role = "slave"
	default:
		panic("Unable to initiate due to invalid role set in configuration.")
	}

	// Local configuration status
	if (Config.Local.Configured) {
		Status = hc.HealthCheckResponse_CONFIGURED
	} else {
		Status = hc.HealthCheckResponse_UNCONFIGURED
	}
}