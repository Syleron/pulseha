package main

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"os"
	"github.com/coreos/go-log/log"
	p "github.com/Syleron/PulseHA/proto"
	"context"
)

type Client struct {
	State      PulseState
	Connection *grpc.ClientConn
	Requester  p.RequesterClient
	Config *Config
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
func (c *Client) Connect(ip, port, hostname string) (error) {
	var err error

	if c.Config.Pulse.TLS {
		creds, err := credentials.NewClientTLSFromFile("./certs/" + hostname + ".crt", "")

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
		return err
	}

	c.Requester = p.NewRequesterClient(c.Connection)

	return nil
}

/**
 *
 */
func (c *Client) Close() {
	//c.Connection.Close()
}

// Senders. Consider moving these into their own file

/**
 *
 */
func (c *Client) SendCheck(data *p.HealthCheckRequest) (*p.HealthCheckResponse, error) {
	r, err := c.Requester.Check(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendJoin(data *p.PulseJoin) (*p.PulseJoin, error) {
 r, err := c.Requester.Join(context.Background(), data)

 return r, err
}

/**
 *
 */
func (c *Client) SendLeave(data *p.PulseLeave) (*p.PulseLeave, error) {
	r, err := c.Requester.Leave(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendGroupNew(data *p.PulseGroupNew) (*p.PulseGroupNew, error) {
	r, err := c.Requester.NewGroup(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendGroupDelete(data *p.PulseGroupDelete) (*p.PulseGroupDelete, error) {
	r, err := c.Requester.DeleteGroup(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendGroupIPAdd(data *p.PulseGroupAdd) (*p.PulseGroupAdd, error) {
	r, err := c.Requester.GroupIPAdd(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendCheckGroupIPRemove(data *p.PulseGroupRemove) (*p.PulseGroupRemove, error) {
	r, err := c.Requester.GroupIPRemove(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendGroupAssign(data *p.PulseGroupAssign) (*p.PulseGroupAssign, error) {
	r, err := c.Requester.GroupAssign(context.Background(), data)

	return r, err
}

/**
 *
 */
func (c *Client) SendGroupUnassign(data *p.PulseGroupUnassign) (*p.PulseGroupUnassign, error) {
	r, err := c.Requester.GroupUnassign(context.Background(), data)

	return r, err
}
