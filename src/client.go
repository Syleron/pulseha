package main

import (
	"google.golang.org/grpc/credentials"
	"log"
	"os"
	"google.golang.org/grpc"
)

type Client struct {
	Logger *log.Logger
	State      PulseState
	Connection *grpc.ClientConn
	Requester  RequesterClient
}

type PulseState int

const (
	PulseConnected    = iota
	PulseDisconnected
)

func (p PulseState) String() string {
	switch p {
	case PulseConnected:
		return "connected"
	case PulseDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

func (c *Client) Setup() {
	// Set initial state
	c.State = PulseDisconnected
}

func (c *Client) Connect(ip, port string) {
	creds, err := credentials.NewClientTLSFromFile("./certs/client.crt", "")

	if err != nil {
		log.Fatal("Could not load TLS cert: ", err)
		os.Exit(1)
	}

	c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(creds))

	if err != nil {
		c.Logger.Printf("[ERR] Pulse: GRPC client connection error", err)
	}

	c.Requester = NewRequesterClient(c.Connection)
}

func (c *Client) Close() {
	c.Connection.Close()
}
