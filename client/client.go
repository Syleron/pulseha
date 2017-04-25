package client

import (
	log "github.com/Sirupsen/logrus"
	hc "github.com/syleron/pulse/proto"
	"google.golang.org/grpc"
	"time"
	"github.com/syleron/pulse/structures"
	"github.com/syleron/pulse/utils"
	"context"
)
var (
	Config		structures.Configuration
	Connection	hc.RequesterClient

	// Local Variables
	LocalIP		string
	LocalPort	string

	// Peer's Connection Details
	PeerIP		string
	PeerPort	string
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

	// Check to see what role we are
	if (Config.Local.Role == "master") {
		// We are the master.
		// Are we in a configured state?
		if (Config.Local.Configured) {
			log.Info("This is a configured setup..")
			// Start the health check scheduler
			log.Info("Starting healthcheck scheduler..")
			utils.Scheduler(healthCheck, time.Duration(Config.Local.HCInterval) * time.Millisecond)
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
			log.Info("This is a configured setup..")
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
 * Slave function - used to monitor when the last healthcheck we received.
 */
func monitorResponses() {

}

/**
 * Function to handle health checks across the cluster
 */
func healthCheck() {
	conn, err := grpc.Dial(PeerIP+":"+PeerPort, grpc.WithInsecure())

	if err != nil {
		log.Error("Connection Error: %v", err)
	}

	defer conn.Close()

	c := hc.NewRequesterClient(conn)

	r, err := c.Check(context.Background(), &hc.HealthCheckRequest{
		Request: hc.HealthCheckRequest_STATUS,
	})

	if err != nil {
		// Oops we couldn't connect... let's try again!
		time.Sleep(time.Second * 5)
		healthCheck()
	}

	log.Printf("Response: %s", r.Status)
}

/**
 * Function to setup cluster as a master.
 */
func sendSetup() {
	conn, err := grpc.Dial(PeerIP+":"+PeerPort, grpc.WithInsecure())

	if err != nil {
		//log.Error("Connection Error: %v", err)
	}

	defer conn.Close()

	c := hc.NewRequesterClient(conn)

	r, err := c.Check(context.Background(), &hc.HealthCheckRequest{
		Request: hc.HealthCheckRequest_SETUP,
	})

	if err != nil {
		// Oops we couldn't connect... let's try again!
		time.Sleep(time.Second * 5)
		sendSetup()
	}

	switch (r.Status) {
	case hc.HealthCheckResponse_UNKNOWN:
		log.Printf("Response: %s", r.Status)
	case hc.HealthCheckResponse_CONFIGURED:
		if (configureCluster()) {
			// start sending healthchecks
			log.Info("Starting healthcheck scheduler..")
			utils.Scheduler(healthCheck, time.Duration(Config.Local.HCInterval) * time.Millisecond)
		}
	default:
		log.Printf("Default Response: %s", r.Status)
	}
}

/**
 * This function should only ever be called by the master to configure itself.
 */
func configureCluster() bool{
	// Check to see if we can configure this node
	// make sure we are a slave
	if Config.Local.Role == "master" {
		// Are we in a configured state already?
		if Config.Local.Configured == false {
			// Set the local value to configured
			Config.Local.Configured = true;

			// Save
			utils.SaveConfig(Config)

			log.Info("Succesfully configured master.")

			return true;
		} else {
			return false
		}
	}
	return false
}

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

func ForceConfigReload() {
	log.Info("Client config forced reload..")

	// Call setup to re-set the client
	Setup()

	// double check for a role change.. if so we need to start sending health checks!


	// "cluster configuration has recovered."
}