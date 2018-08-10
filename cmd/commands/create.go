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

type CreateCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *CreateCommand) Help() string {
	helpText := `
Usage: pulseha create <bind IP> <bind port>
  Tells the PulseHA daemon to configure a new cluster.
`
	return strings.TrimSpace(helpText)
}

/**
Run the CLI command
*/
func (c *CreateCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("create", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	// Make sure we have cmd args
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	cmds := cmdFlags.Args()

	// If no action is provided then just list our current config
	if len(cmds) < 2 {
		c.Ui.Error("Please specify an address and port for PulseHA to listen on\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// Define variables
	bindIP := cmds[0]
	bindPort := cmds[1]

	// IP validation
	if utils.IsIPv6(bindIP) {
		bindIP = utils.SanitizeIPv6(bindIP)
	} else if !utils.IsIPv4(bindIP) {
		c.Ui.Error("Please specify a valid join address.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	// Port validation
	if !utils.IsPort(bindPort) {
		c.Ui.Error("Please specify a valid port 0-65536.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
		return 1
	}

	defer connection.Close()

	client := proto.NewCLIClient(connection)

	r, err := client.Create(context.Background(), &proto.PulseCreate{
		BindIp:   bindIP,
		BindPort: bindPort,
	})

	if err != nil {
		c.Ui.Output("PulseHA CLI connection error. Is the PulseHA service running?")
		return 1
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
func (c *CreateCommand) Synopsis() string {
	return "Tell PulseHA to create new HA cluster"
}
