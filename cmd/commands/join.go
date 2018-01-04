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
package commands

import (
	"context"
	"flag"
	"github.com/Syleron/PulseHA/proto"
	"github.com/Syleron/PulseHA/src/utils"
	"github.com/mitchellh/cli"
	"google.golang.org/grpc"
	"strings"
)

type JoinCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *JoinCommand) Help() string {
	helpText := `
Usage: pulseha join [options] address ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
  -bind-ip pulse daemon bind address
  -bind-port pulse daemon bind  port
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *JoinCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("join", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	// Set the acceptable cmd flags
	bindIP := cmdFlags.String("bind-ip", "127.0.0.1", "Bind IP address for local Pulse daemon")
	bindPort := cmdFlags.String("bind-port", "1234", "Bind port for local Pulse daemon")

	// Parse and handle error
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	// Get the command params
	cmds := cmdFlags.Args()

	// Make sure that the join address and port is set
	if len(cmds) < 2 {
		c.Ui.Error("Please specify an address and port to join.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	// If we have the default.. which we don't want.. error out.
	if *bindIP == "127.0.0.1" {
		c.Ui.Error("Please specify a bind IP address.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// If we have the default.. which we don't want.. error out.
	if *bindPort == "1234" {
		c.Ui.Error("Please specify a bind port.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// IP validation
	if utils.IsIPv6(*bindIP) {
		cleanIP := utils.SanitizeIPv6(*bindIP)
		bindIP = &cleanIP
	} else if !utils.IsIPv4(*bindIP) {
		c.Ui.Error("Please specify a valid join address.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// Port validation
	if !utils.IsPort(*bindPort) {
		c.Ui.Error("Please specify a valid port 0-65536.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// validate the join address
	joinIP := cmds[0]
	joinPort := cmds[1]

	if utils.IsIPv6(joinIP) {
		joinIP = utils.SanitizeIPv6(joinIP)
	} else if !utils.IsIPv4(joinIP) {
		c.Ui.Error("Please specify a valid join address.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// Validate join Port
	if !utils.IsPort(joinPort) {
		c.Ui.Error("Please specify a valid join port 0-65536.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// setup a connection
	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())

	// handle the error
	if err != nil {
		c.Ui.Error("GRPC client connection error. Is the PulseHA service running?")
		c.Ui.Error(err.Error())
	}

	// defer the close
	defer connection.Close()

	// setup new RPC client
	client := proto.NewCLIClient(connection)

	r, err := client.Join(context.Background(), &proto.PulseJoin{
		Ip:       joinIP,
		Port:     joinPort,
		BindIp:   *bindIP,
		BindPort: *bindPort,
		Hostname: utils.GetHostname(),
	})

	if err != nil {
		c.Ui.Output("PulseHA CLI connection error. Is the PulseHA service running?")
		c.Ui.Output(err.Error())
	} else {
		if r.Success {
			c.Ui.Output("\n[\u2713] " + r.Message + "\n")
		} else {
			c.Ui.Output("\n[x] " + r.Message + "\n")
		}
	}

	return 0
}

/**
 *
 */
func (c *JoinCommand) Synopsis() string {
	return "Tell PulseHA to join a cluster"
}
