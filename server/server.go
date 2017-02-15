package server

import (
	"log"
	"net"

	pb "github.com/syleron/pulse/pulse"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"sync"
	"fmt"
)

const (
	port = ":50051"
)

type server struct{}

func (s *server) Process(ctx context.Context, in *pb.Config) (*pb.Response, error) {
	return &pb.Response{Message: "Hello " + in.Name}, nil
}

/*
 * Setup Function used to initialise the server
 */
func Setup(wg *sync.WaitGroup) {
	fmt.Println("Initialising server..")
	defer wg.Done()
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterRequesterServer(s, &server{})
	s.Serve(lis)
}