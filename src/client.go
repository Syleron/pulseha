/*
    PulseHA - HA Cluster Daemon
    Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU Affero General Public License as published
    by the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU Affero General Public License for more details.

    You should have received a copy of the GNU Affero General Public License
    along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */
package main

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"github.com/coreos/go-log/log"
	p "github.com/Syleron/PulseHA/proto"
	"context"
	"errors"
)

type Client struct {
	//State      PulseState
	Connection *grpc.ClientConn
	Requester  p.RequesterClient
	Config *Config
}

/**
 *
 */
func (c *Client) Setup() {
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
			return errors.New("could not load node TLS cert: " + hostname + ".crt")
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
