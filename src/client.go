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
	"errors"
	"context"
	"reflect"
)

type Client struct {
	//State      PulseState
	Connection *grpc.ClientConn
	Requester  p.ServerClient
}

/**

 */
func (c *Client) GetFuncBroadcastList() (map[string]interface{}) {
	funcList := map[string]interface{} {
		"SendLeave": c.SendLeave,
		"SendConfigSync": c.SendConfigSync,
	}
	return funcList
}


func (c *Client) GetProtoFuncList() (map[string]interface{}) {
	funcList := map[string]interface{} {
		"ConfigSync": c.Requester.ConfigSync,
		"Join": c.Requester.Join,
		"Leave": c.Requester.Leave,
	}
	return funcList
}

/**

 */
func (c *Client) Setup() {

}

/**
	Note: Hostname is required for TLS as the certs are named after the hostname.
 */
func (c *Client) Connect(ip, port, hostname string) (error) {
	log.Debug("Client:Connect() Connection made to " + ip + ":" + port)
	var err error
	config := gconf.GetConfig()
	if config.Pulse.TLS {
		creds, err := credentials.NewClientTLSFromFile("./certs/"+hostname+".crt", "")
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

func (c *Client) Send(funcName string, params ... interface{}) (interface{}, error) {
	funcList := c.GetProtoFuncList()
	f := reflect.ValueOf(funcList[funcName])
	if len(params) != f.Type().NumIn() {
		return nil, errors.New("the number of passed parameters do not match the function")
	}
	vals := make([]reflect.Value, len(params))
	for k, param := range params {
		vals[k] = reflect.ValueOf(param)
	}
	f.Call(vals)
	return nil, nil
}
