package main

import (
	"github.com/Syleron/PulseHA/proto"
	"github.com/coreos/go-log/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"os"
)

type Client struct {
	State      PulseState
	Connection *grpc.ClientConn
	Requester  proto.RequesterClient
	Config     *Config
}

type PulseState int

const (
	PulseConnected = iota
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
	//if c.ClusterCheck() {
	//	// We are in a cluster
	//	// Find the active node
	//	_, err := c.FindActiveNode()
	//
	//	if err != nil {
	//		// No one is available.. assume we must take responsibility.
	//		log.Info("No members available.")
	//	}
	//
	//} else {
	//	// we are not in a cluster
	//}
	// Are there any other members in the cluster online?

	// Are they the active appliance? (They should be)

	// If not.. who is the active appliance? (because there should be one)
}

/**
 *
 */
func (c *Client) Connect(ip, port string) {
	var err error

	if c.Config.Pulse.TLS {
		creds, err := credentials.NewClientTLSFromFile("./certs/client.crt", "")

		if err != nil {
			log.Errorf("Could not load TLS cert: ", err)
			os.Exit(1)
		}

		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(creds))
	} else {
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithInsecure())
	}

	if err != nil {
		log.Errorf("GRPC client connection error: ", err)
	}

	c.Requester = proto.NewRequesterClient(c.Connection)
}

/**
 *
 */
func (c *Client) Close() {
	//c.Connection.Close()
}

/**
 *
 */
func (c *Client) sendSetup() {}

/**
 *
 */
func (c *Client) configureCluster()    {}
func (c *Client) Join(ip, port string) {}
func (c *Client) Leave()               {}
func (c *Client) Broadcast()           {}
