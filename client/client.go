package client

import (
	"log"
	pb "github.com/syleron/pulse/pulse"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"fmt"
)

const (
	address     = "localhost:50051"
)

/*
 * Setup Function used to initialise the client
 */
func Setup() {
	fmt.Println("Initialising client..")
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Connection Error: %v", err)
	}
	defer conn.Close()
	c := pb.NewRequesterClient(conn)
	//health := pb.NewHealthClient(conn)
	//
	//var ctx context.Context
	//health.Check(ctx.Deadline(), pb.HealthCheckRequest{})

	name := "Andy"
	r, err := c.Process(context.Background(), &pb.Config{Name: name})
	if err != nil {
		log.Fatalf("could not greet: %v", err)
	}
	log.Printf("Response: %s", r.Message)
}

/**
 * Function to handle health checks accross the cluster
 */
func roundRobinHealthCheck() {

}