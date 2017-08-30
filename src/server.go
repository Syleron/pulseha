package main

import (
	"sync"
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc"
	"net"
	"google.golang.org/grpc/credentials"
	"log"
	"os"
)

type Server struct {
	Logger *log.Logger
	sync.Mutex
	Status HealthCheckResponse_ServingStatus
}

func (s *Server) Check(ctx context.Context, in *HealthCheckRequest) (*HealthCheckResponse, error) {
	return nil, grpc.Errorf(codes.NotFound, "unknown request")
}

func (s *Server) Setup(ip, port string) {
	lis, err := net.Listen("tcp", ip+":"+port)

	if err != nil {
		s.Logger.Printf("[ERR] Pulse: Failed to listen: %s", err)
	}

	creds, err := credentials.NewServerTLSFromFile("./certs/server.crt", "./certs/server.key")

	if err != nil {
		s.Logger.Print("[ERR] Pulse: Could not load TLS keys.")
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds))

	if err := grpcServer.Serve(lis); err != nil {
		s.Logger.Printf("[ERR] Pulse: GRPC unable to serve: %s", err)
	}

	s.Logger.Printf("[INFO] Pulse: Initialised on "+ip+":"+port)
}

func (s *Server) Failover() {

}
