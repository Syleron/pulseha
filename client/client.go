package client

import (
	log "github.com/Sirupsen/logrus"
	pb "github.com/syleron/pulse/proto"
	"google.golang.org/grpc"
	"time"
	"github.com/syleron/pulse/structures"
	"github.com/syleron/pulse/utils"
	"context"
)
var (
	Config		structures.Configuration
	Connection	pb.RequesterClient

	// Peer's Connection Details
	PeerIP		string
	PeerPort	string
)

/*
 * Setup Function used to initialise the client
 */
func Setup() {
	log.Info("Initialising client..")

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
			// Start the health check scheduler
		} else {

		}
	} else {
		// We are the slave.
		// Are we in a configured state?
		if (Config.Local.Configured) {
			// check to see when the last time we received a health check
			// Do we need to failover?
		} else {
			// We have not been configured yet. Sit and listen
		}
	}

	// Schedule the health checks to ensure each member within the cluster still exists!
	scheduler(roundRobinHealthCheck, time.Duration(Config.Local.Interval) * time.Millisecond)
}

/**
 * Function to schedule the execution every x time as time.Duration.
 */
func scheduler(method func(), delay time.Duration) {
	for _ = range time.Tick(delay) {
		method()
	}
}

/**
 * Function to handle health checks across the cluster
 * TODO: Update this function to perform a health check that is secure.
 */
func roundRobinHealthCheck() {
	var opts []grpc.DialOption

	// setup TLS stuff
	if Config.Local.TLS {
		//var sn string
		//var creds credentials.TransportAuthenticator
		//creds = credentials.NewClientTLSFromCert(nil, sn)
		//opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(PeerIP+":"+PeerPort, opts...)
	if err != nil {
		log.Error("Connection Error: %v", err)
	}
	defer conn.Close()

	c := pb.NewRequesterClient(conn)

	name := "world"

	r, err := c.Process(context.Background(), &pb.Config{Name: name})

	r, err := c.Process(context.Background(), &pb.)

	if err != nil {
		log.Fatalf("could not greet: %v", err)
	}

	log.Printf("Response: %s", r.Message)
}

func sendSetup() {
	var opts []grpc.DialOption

	// setup TLS stuff
	if Config.Local.TLS {
		//var sn string
		//var creds credentials.TransportAuthenticator
		//creds = credentials.NewClientTLSFromCert(nil, sn)
		//opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(PeerIP+":"+PeerPort, opts...)

	if err != nil {
		log.Error("Connection Error: %v", err)
	}

	defer conn.Close()

	c := pb.NewRequesterClient(conn)

	name := "world"

	r, err := c.Process(context.Background(), &pb.Config{Name: name})

	if err != nil {
		log.Fatalf("could not greet: %v", err)
	}

	log.Printf("Response: %s", r.Message)

}

func setupLocalVariables() {
	switch Config.Local.Role {
	case "master":
		PeerIP = Config.Cluster.Nodes.Slave.IP
		PeerPort = Config.Cluster.Nodes.Slave.Port
	case "slave":
		PeerIP = Config.Cluster.Nodes.Master.IP
		PeerPort = Config.Cluster.Nodes.Master.Port
	default:
		panic("Unable to initiate due to invalid role set in configuration.")
	}
}
