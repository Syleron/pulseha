package server

import (
	log "github.com/Sirupsen/logrus"
	"net"
	pb "github.com/syleron/pulse/proto"
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
	Role	string
	ServerPort int
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

	// Are we master or slave?

	switch Config.Local.Role {
	case "master":
	case "slave":
	default:
		panic("Unable to initiate due to invalid role set in configuration.")
	}

	defer wg.Done()

	lis, err := net.Listen("tcp", ":" + "4000")

	if err != nil {
		log.Error("failed to listen: %v", err)
	}

	s := grpc.NewServer()

	pb.RegisterRequesterServer(s, &server{})

	s.Serve(lis)
}