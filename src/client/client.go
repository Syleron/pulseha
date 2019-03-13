/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2018  Andrew Zak <andrew@pulseha.com>

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
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	log "github.com/Sirupsen/logrus"
	p "github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/security"
	"github.com/Syleron/PulseHA/src/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io/ioutil"
	"time"
)

type Client struct {
	Connection *grpc.ClientConn
	Requester  p.ServerClient
}

// This should probably go into an enums folder
type ProtoFunction int

const (
	SendConfigSync ProtoFunction = 1 + iota
	SendJoin
	SendLeave
	SendMakePassive
	SendBringUpIP
	SendBringDownIP
	SendHealthCheck
	SendPromote
)

var protoFunctions = []string{
	"ConfigSync",
	"Join",
	"Leave",
	"MakePassive",
	"BringUpIP",
	"BringDownIP",
	"HealthCheck",
	"Promote",
}

func (p ProtoFunction) String() string {
	return protoFunctions[p-1]
}

// -----

/**

 */
func (c *Client) getProtoFuncList() map[string]interface{} {
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
		"Promote": func(ctx context.Context, data interface{}) (interface{}, error) {
			return c.Requester.Promote(ctx, data.(*p.PulsePromote))
		},
	}
	return funcList
}

/**
Note: Hostname is required for TLS as the certs are named after the hostname.
*/
func (c *Client) Connect(ip, port, hostname string, tlsEnabled bool) error {
	var err error
	if tlsEnabled {
		// Load member cert/key
		hostname, err := utils.GetHostname()
		if err != nil {
			return errors.New("unable to connect because cannot get hostname")
		}
		peerCert, err := tls.LoadX509KeyPair(
			security.CertDir+hostname+".client.crt",
			security.CertDir+hostname+".client.key",
		)
		if err != nil {
			return errors.New("Could not connect to host: " + err.Error())
		}
		// Load CA
		caCert, err := ioutil.ReadFile(security.CertDir + "ca.crt")
		if err != nil {
			return errors.New("Could not connect to host: " + err.Error())
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		creds := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{peerCert},
			RootCAs:      caCertPool,
		})
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithTransportCredentials(creds))
	} else {
		c.Connection, err = grpc.Dial(ip+":"+port, grpc.WithInsecure())
	}
	if err != nil {
		log.Errorf("GRPC client connection error: %s", err.Error())
		return errors.New("Could not connect to host: " + err.Error())
	}
	c.Requester = p.NewServerClient(c.Connection)
	log.Debug("Client:Connect() Connection made to " + ip + ":" + port)
	return nil
}

/**
Close the client connection
*/
func (c *Client) Close() {
	log.Debug("Client:Close() Connection closed")
	// Make sure we have a connection before trying to close it
	if c.Connection != nil {
		c.Connection.Close()
	}
}

/**
Send a specific GRPC call
*/
func (c *Client) Send(funcName ProtoFunction, data interface{}) (interface{}, error) {
	log.Debug("Client:Send() Sending " + funcName.String())
	funcList := c.getProtoFuncList()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return funcList[funcName.String()].(func(context.Context, interface{}) (interface{}, error))(
		ctx, data,
	)
}

/**
Create a new instance of our Client
 */
func New(ip, port, hostname string, tlsEnabled bool) (*Client, error) {
	log.Debug("Client:New() Creating new client object for " + hostname)
	client := new(Client)
	err := client.Connect(ip, port, hostname, tlsEnabled)
	if err != nil {
		return nil, err
	}
	return client, nil
}
