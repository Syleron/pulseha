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
	"github.com/Syleron/Pulse/proto"
)

type Server struct {
	sync.Mutex
	Status proto.HealthCheckResponse_ServingStatus
	Last_response time.Time
	Members []Member
	Config *Config
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
	case proto.HealthCheckRequest_STATUS:
		return &proto.HealthCheckResponse{
			Status: proto.HealthCheckResponse_CONFIGURED,
		}, nil
	default:
	}

	return nil, nil
}

/**
 *
 */
func (s *Server) Setup(ip, port string) {
	lis, err := net.Listen("tcp", ip+":"+port)

	if err != nil {
		log.Errorf("Failed to listen: %s", err)
	}

	var grpcServer *grpc.Server
	if s.Config.Pulse.TLS {
		if CreateFolder("./certs") {
			log.Warning("TLS keys are missing! Generating..")
			GenOpenSSL()
		}

		creds, err := credentials.NewServerTLSFromFile("./certs/server.crt", "./certs/server.key")

		if err != nil {
			log.Error("Could not load TLS keys.")
			os.Exit(1)
		}

		grpcServer = grpc.NewServer(grpc.Creds(creds))
	} else {
		grpcServer = grpc.NewServer()
	}

	proto.RegisterRequesterServer(grpcServer, s)

	log.Info("Pulse initialised on "+ip+":"+port)

	s.SetupCLI()

	grpcServer.Serve(lis)
}

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

func (s * Server) Join(ip, port string) {

}

//func (c *Client) ClusterCheck() bool {
//	if len(c.Config.Nodes.Nodes) > 0 && len(c.Config.Pools.Pools) > 0 {
//		return true
//	}
//
//	return false
//}
