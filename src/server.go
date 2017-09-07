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
		log.Debug("Server:Check() - HealthCheckRequest Setup")
	case proto.HealthCheckRequest_STATUS:
		log.Debug("Server:Check() - HealthCheckRequest Status")
		return &proto.HealthCheckResponse{
			Status: proto.HealthCheckResponse_CONFIGURED,
		}, nil
	default:
	}
	return nil, nil
}

func (s * Server) Join(ctx context.Context, in *proto.PulseJoin) (*proto.PulseJoin, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Join() - Join Pulse cluster")
	return &proto.PulseJoin{
		Success: true,
	}, nil
}

func (s * Server) Leave(ctx context.Context, in *proto.PulseLeave) (*proto.PulseLeave, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Join() - Join Pulse cluster")
	return &proto.PulseLeave{}, nil
}

func (s * Server) Create(ctx context.Context, in *proto.PulseCreate) (*proto.PulseCreate, error) {
	s.Lock()
	defer s.Unlock()
	log.Debug("Server:Join() - Join Pulse cluster")
	return &proto.PulseCreate{}, nil
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

