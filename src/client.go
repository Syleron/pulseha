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
	"context"
	"errors"
	p "github.com/Syleron/PulseHA/proto"
	log "github.com/Sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"time"
)

type Client struct {
	Connection *grpc.ClientConn
	Requester  p.ServerClient
}

// This should probably go into an enums folder
type protoFunction int

const (
	SendConfigSync protoFunction = 1 + iota
	SendJoin
	SendLeave
	SendMakeActive
	SendMakePassive
	SendBringUpIP
	SendBringDownIP
	SendHealthCheck
)

var protoFunctions = []string{
	"ConfigSync",
	"Join",
	"Leave",
	"MakeActive",
	"MakePassive",
	"BringUpIP",
	"BringDownIP",
	"HealthCheck",
}

func (p protoFunction) String() string {
	return protoFunctions[p-1]
}

// -----

/**

 */
func (c *Client) GetProtoFuncList() map[string]interface{} {
	funcList := map[string]interface{}{
		"ConfigSync": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.ConfigSync(ctx, data.(*p.PulseConfigSync))
		},
		"Join": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.Join(ctx, data.(*p.PulseJoin))
		},
		"Leave": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.Leave(ctx, data.(*p.PulseLeave))
		},
		"MakeActive": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.MakeActive(ctx, data.(*p.PulsePromote))
		},
		"MakePassive": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.MakePassive(ctx, data.(*p.PulsePromote))
		},
		"BringUpIP": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.BringUpIP(ctx, data.(*p.PulseBringIP))
		},
		"BringDownIP": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.BringDownIP(ctx, data.(*p.PulseBringIP))
		},
		"HealthCheck": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.HealthCheck(ctx, data.(*p.PulseHealthCheck))
		},
	}
	return funcList
}

/**
Note: Hostname is required for TLS as the certs are named after the hostname.
*/
func (c *Client) Connect(ip, port, hostname string) error {
	log.Debug("Client:Connect() Connection made to " + ip + ":" + port)
	var err error
	config := gconf.GetConfig()
	if config.Pulse.TLS {
		creds, err := credentials.NewClientTLSFromFile("./certs/"+hostname+".crt", "")
		if err != nil {
			log.Errorf("Could not load TLS cert: %s", err.Error())
			return errors.New("could not load node TLS cert: " + hostname + ".crt")
		}
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(creds))
	} else {
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithInsecure())
	}
	if err != nil {
		log.Errorf("GRPC client connection error: %s", err.Error())
		return err
	}
	c.Requester = p.NewServerClient(c.Connection)
	return nil
}

/**
Close the client connection
*/
func (c *Client) Close() {
	log.Debug("Client:Close() Connection closed")
	c.Connection.Close()
}

/**
Send a specific GRPC call
*/
func (c *Client) Send(funcName protoFunction, data interface{}) (interface{}, error) {
	log.Debug("Client:Send() Sending " + funcName.String())
	funcList := c.GetProtoFuncList()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	return funcList[funcName.String()].(func(context.Context, interface{}) (interface{}, error))(
		ctx, data,
	)
}
