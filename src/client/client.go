package client

import (
	log "github.com/Sirupsen/logrus"
	hc "github.com/Syleron/Pulse/src/proto"
	"github.com/Syleron/Pulse/src/structures"
	"github.com/Syleron/Pulse/src/utils"
	"github.com/Syleron/Pulse/src/networking"
	"google.golang.org/grpc"
	"time"
	"context"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/credentials"
	oglog "log"
	"io/ioutil"
	"os"
)

var (
	Config    structures.Configuration
	Connected bool

	// Local Variables
	LocalIP   string
	LocalPort string

	// Peer's Connection Details
	PeerIP   string
	PeerPort string

	Conn   *grpc.ClientConn
	Client hc.RequesterClient
)

/*
 * Setup Function used to initialise the client
 */
func Setup() {
	// Set the local config value - this should probably have validation - probably a nicer way og doing this.
	Config = utils.LoadConfig()
	Config.Validate()

	// Set all the local variables from the config.
	setupLocalVariables()

	// Set connected value
	Connected = false

	// Setup local networking
	_configureLocalNetwork()

	var err error

	// Setup GRPC client
	// Create the client TLS credentials
	creds, err := credentials.NewClientTLSFromFile("./certs/client.crt", "")
	if err != nil {
		log.Fatal("Could not load TLS cert: ", err)
		os.Exit(1)
	}

	// Create a connection with the TLS credentials
	Conn, err = grpc.Dial(PeerIP+":"+PeerPort, grpc.WithTransportCredentials(creds))

	if err != nil {
		log.Warning("GRPC Connection Error ", err)
	}

	// Set the default logger for grpc
	grpclog.SetLogger(oglog.New(ioutil.Discard, "", 0))

	// Defer closing the client connection.
	defer Conn.Close()

	Client = hc.NewRequesterClient(Conn)

	// Check to see what role we are
	if Config.Local.Role == "master" {
		// We are the master.
		// Are we in a configured state?
		if (Config.Local.Configured) {
			log.Info("This is a configured cluster..")
			// Start the health check scheduler
			log.Info("Starting healthcheck scheduler..")
			utils.Scheduler(healthCheck, time.Duration(Config.Local.HCInterval)*time.Millisecond)
		} else {
			// Logo that we have not been configured yet.
			log.Warn("This is an unconfigured deployment.")

			// Send setup request to slave
			sendSetup()
		}

		// Schedule the health checks to ensure each member within the cluster still exists!
		//scheduler(healthCheck, time.Duration(Config.Local.Interval) * time.Millisecond)
	} else {
		// We are the slave.
		// Are we in a configured state?
		if (Config.Local.Configured) {
			log.Info("This is a configured cluster..")
			// check to see when the last time we received a health check
			// Do we need to failover?
		} else {
			// Log that that we have not been configured yet.
			log.Warn("This is an unconfigured deployment.")
			// Log that we are listening for a setup request
			log.Info("Waiting for setup request from master..")
			// We have not been configured yet. Sit and listen
		}
	}
}

/**
 * Handle floating ip up/down based on master or slave.
 */
func _configureLocalNetwork() {
	if (Config.Local.Role == "master") {
		// Attempt to bring floating IP up
		networking.BringIPup(Config.Local.Interface, Config.Cluster.FloatingIP)
		// Send grat arp
		networking.SendGARP(Config.Local.Interface, Config.Cluster.FloatingIP)
	} else {
		// Attempt to bring floating IP down
		networking.BringIPdown(Config.Local.Interface, Config.Cluster.FloatingIP)
	}
}

/**
 * Master Function to handle health checks across the cluster
 */
func healthCheck() {
	r, err := Client.Check(context.Background(), &hc.HealthCheckRequest{
		Request: hc.HealthCheckRequest_STATUS,
	})

	if err != nil {
		// Oops we couldn't connect... let's try again!
		logConnectionStatus(false)
		Conn.Close()
		time.Sleep(time.Second * 5)
		healthCheck()
	} else {
		logConnectionStatus(true)
		log.Printf("Response: %s", r.Status)
	}
}

/**
 * Master unction to setup cluster as a master.
 */
func sendSetup() {
	r, err := Client.Check(context.Background(), &hc.HealthCheckRequest{
		Request: hc.HealthCheckRequest_SETUP,
	})

	if err != nil {
		// Oops we couldn't connect... let's try again!
		logConnectionStatus(false)
		time.Sleep(time.Second * 5)
		sendSetup()
	} else {
		logConnectionStatus(true)
		switch (r.Status) {
		case hc.HealthCheckResponse_UNKNOWN:
			log.Printf("Response: %s", r.Status)
		case hc.HealthCheckResponse_CONFIGURED:
			if (configureCluster()) {
				// start sending healthchecks
				log.Info("Starting healthcheck scheduler..")
				utils.Scheduler(healthCheck, time.Duration(Config.Local.HCInterval)*time.Millisecond)
			}
		default:
			log.Printf("Default Response: %s", r.Status)
		}
	}
}

/**
 * Master This function should only ever be called by the master to configure itself.
 * NOTE: This function could be shared between the client and server so this could
 *       be moved to the utils package.
 */
func configureCluster() bool {
	// Check to see if we can configure this node
	// make sure we are a slave
	if Config.Local.Role == "master" {
		// Are we in a configured state already?
		if Config.Local.Configured == false {
			// Set the local value to configured
			Config.Local.Configured = true;

			// Save
			utils.SaveConfig(Config)

			log.Info("Successfully configured master.")

			return true;
		} else {
			return false
		}
	}
	return false
}

/**
 * Function to setup the local variables for this client.
 */
func setupLocalVariables() {
	switch Config.Local.Role {
	case "master":
		PeerIP = Config.Cluster.Nodes.Slave.IP
		PeerPort = Config.Cluster.Nodes.Slave.Port

		LocalIP = Config.Cluster.Nodes.Master.IP
		LocalPort = Config.Cluster.Nodes.Master.Port
	case "slave":
		PeerIP = Config.Cluster.Nodes.Master.IP
		PeerPort = Config.Cluster.Nodes.Master.Port

		LocalIP = Config.Cluster.Nodes.Slave.IP
		LocalPort = Config.Cluster.Nodes.Slave.Port
	default:
		panic("Unable to initiate due to invalid role set in configuration.")
	}
}

/**
 * Function to completely reload the config and re-setup the client.
 */
func ForceConfigReload() {
	log.Info("Client config forced reload..")

	// Call setup to re-set the client
	Setup()
}

/**
 * Master Function - Used to log whether we are still connected or not
 */
func logConnectionStatus(status bool) {
	if Connected && !status {
		log.Warn("Disconnected from slave.. Attempting to reconnect!")
		Connected = false
	} else if !Connected && status {
		log.Info("Connection with slave established!")
		Connected = true
	}
}
