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
	"os"
	"github.com/coreos/go-log/log"
	"github.com/Syleron/PulseHA/proto"
)

type Client struct {
	State      PulseState
	Connection *grpc.ClientConn
	Requester  proto.RequesterClient
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
