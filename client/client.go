package client

import (
	log "github.com/Sirupsen/logrus"
	pb "github.com/syleron/pulse/pulse"
	//"golang.org/x/net/context"
	"google.golang.org/grpc"
	"time"
	"github.com/syleron/pulse/structures"
	"github.com/syleron/pulse/utils"
	"fmt"
	"context"
)

const (
	address     = "localhost:50051"
)

var (
	Config		structures.Configuration
	Connection	pb.RequesterClient
)

/*
 * Setup Function used to initialise the clienti
 * TODO: Must be a better way of accessing the config!
 */
func Setup() {
	log.Info("Initialising client..")

	// Set the local config value - this should probably have validation - probably a nicer way og doing this.
	Config = utils.LoadConfig()
	Config.Validate()

	// Schedule the health checks to ensure each member within the cluster still exists!
	scheduler(roundRobinHealthCheck, time.Duration(Config.General.Interval) * time.Millisecond)
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
	conn, err := grpc.Dial(address, grpc.WithInsecure())
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

func test() {
	fmt.Print("testing 123 :)")
}
