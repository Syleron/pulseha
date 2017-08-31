package main

import (
	"google.golang.org/grpc/credentials"
	"os"
	"google.golang.org/grpc"
	"github.com/coreos/go-log/log"
)

type Client struct {
	State      PulseState
	Connection *grpc.ClientConn
	Requester  RequesterClient
	Config *Config
}

type PulseState int

const (
	PulseConnected    = iota
	PulseDisconnected
)

/**
 *
 */
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

/**
 *
 */
func (c *Client) Setup() {
	// Are we in a cluster?

	// Are there any other members in the cluster online?

	// Are they the active appliance? (They should be)

	// If not.. who is the active appliance? (because there should be one)
}

/**
 *
 */
func (c *Client) Connect(ip, port string) {
	creds, err := credentials.NewClientTLSFromFile("./certs/client.crt", "")

	if err != nil {
		log.Errorf("Could not load TLS cert: ", err)
		os.Exit(1)
	}

	c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(creds))

	if err != nil {
		log.Errorf("GRPC client connection error: ", err)
	}

	c.Requester = NewRequesterClient(c.Connection)
}

/**
 *
 */
func (c *Client) Close() {
	c.Connection.Close()
}

/**
 *
 */
func (c *Client) sendSetup() {}

/**
 *
 */
func (c *Client) configureCluster() {}

func (c *Client) Join() {}
func (c *Client) Leave() {}
func (c *Client) Broadcast() {}
