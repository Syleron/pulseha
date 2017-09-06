package main

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"os"
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
	if c.ClusterCheck() {
		// We are in a cluster
		// Find the active node
		_, err := c.FindActiveNode()

		if err != nil {
			// No one is available.. assume we must take responsibility.
			log.Info("No members available.")
		}

	} else {
		// we are not in a cluster
	}
	// Are there any other members in the cluster online?

	// Are they the active appliance? (They should be)

	// If not.. who is the active appliance? (because there should be one)
}

/**
 *
 */
func (c *Client) Connect(ip, port string) {
	var err error

	if c.Config.Cluster.TLS {
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

	c.Requester = NewRequesterClient(c.Connection)
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
func (c *Client) configureCluster() {}

func (c *Client) Join(ip, port string) {
	// Are we already in a cluster?
	if !c.ClusterCheck() {
		// Was an ip port provided? if not, create a new cluster
		if ip == "" || port == "" {

		} else {
			// Attempt to connect to node to join the cluster
		}
	} else {
		log.Error("Unable to join cluster. Already configured in a cluster.")
	}
}
func (c *Client) Leave() {}
func (c *Client) Broadcast() {}

func (c *Client) ClusterCheck() bool {
	if len(c.Config.Nodes.Nodes) > 0 && len(c.Config.Pools.Pools) > 0 {
		return true
	}

	return false
}

func (c *Client) FindActiveNode() (*Member, error) {
	return &Member{}, nil
}


