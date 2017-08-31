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
)

type Server struct {
	sync.Mutex
	Status HealthCheckResponse_ServingStatus
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
	State HealthCheckResponse_ServingStatus
}

/**
 *
 */
func (s *Server) Check(ctx context.Context, in *HealthCheckRequest) (*HealthCheckResponse, error) {
	s.Lock()
	defer s.Unlock()

	switch in.Request {
	case HealthCheckRequest_SETUP:
	case HealthCheckRequest_STATUS:
	default:
	}

	return nil, nil
}

/**
 *
 */
func (s *Server) Failover() {}

/**
 *
 */
func (s *Server) ConfigureCluster() {
	// Configure the cluster
}

/**
 *
 */
func (s *Server) MonitorResponses() {
	// handle the responses and act upon them
}

/**
 *
 */
func (s *Server) Setup(ip, port string) {
	lis, err := net.Listen("tcp", ip+":"+port)

	if err != nil {
		log.Errorf("Failed to listen: %s", err)
	}

	if CreateFolder("./certs") {
		log.Warning("TLS keys are missing! Generating..")
		GenOpenSSL()
	}

	creds, err := credentials.NewServerTLSFromFile("./certs/server.crt", "./certs/server.key")

	if err != nil {
		log.Error("Could not load TLS keys.")
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds))

	RegisterRequesterServer(grpcServer, s)

	log.Info("Initialised on "+ip+":"+port)

	if err := grpcServer.Serve(lis); err != nil {
		log.Errorf("GRPC unable to serve: %s", err)
	}
}

