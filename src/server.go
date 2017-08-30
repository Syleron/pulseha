package main

import (
	"sync"
	"context"
	"google.golang.org/grpc"
	"net"
	"google.golang.org/grpc/credentials"
	"log"
	"os"
	"time"
	"github.com/Syleron/Pulse/src/utils"
)

type Server struct {
	Logger *log.Logger
	sync.Mutex
	Status HealthCheckResponse_ServingStatus
	Last_response time.Time
	Members []Member
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
func (s *Server) ConfigureCluster() {}

/**
 *
 */
func (s *Server) MonitorResponses() {}

/**
 *
 */
func (s *Server) Setup(ip, port string) {
	lis, err := net.Listen("tcp", ip+":"+port)

	if err != nil {
		s.Logger.Printf("[ERR] Pulse: Failed to listen: %s", err)
	}

	if utils.CreateFolder("./certs") {
		s.Logger.Print("[WARN] Pulse: TLS keys are missing! Generating..")
		GenOpenSSL()
	}

	creds, err := credentials.NewServerTLSFromFile("./certs/server.crt", "./certs/server.key")

	if err != nil {
		s.Logger.Print("[ERR] Pulse: Could not load TLS keys.")
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds))

	RegisterRequesterServer(grpcServer, s)

	s.Logger.Printf("[INFO] Pulse: Initialised on "+ip+":"+port)

	if err := grpcServer.Serve(lis); err != nil {
		s.Logger.Printf("[ERR] Pulse: GRPC unable to serve: %s", err)
	}
}
