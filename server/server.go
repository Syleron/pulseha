package server

import (
	log "github.com/Sirupsen/logrus"
	"net"
	pb "github.com/syleron/pulse/pulse"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"sync"
	"github.com/syleron/pulse/structures"
	"github.com/syleron/pulse/utils"
)

const (
	port = ":50051"
)

var (
	Config	structures.Configuration
)

type server struct{}

func (s *server) Process(ctx context.Context, in *pb.Config) (*pb.Response, error) {
	return &pb.Response{Message: "Hello " + in.Name}, nil
}

/*
 * Setup Function used to initialise the server
 */
func Setup(wg *sync.WaitGroup) {
	log.Info("Initialising server..")

	// Load the config and validate
	Config = utils.LoadConfig()
	Config.Validate()

	defer wg.Done()
	lis, err := net.Listen("tcp", ":" + Config.General.ServerPort)
	if err != nil {
		log.Error("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterRequesterServer(s, &server{})
	s.Serve(lis)
}