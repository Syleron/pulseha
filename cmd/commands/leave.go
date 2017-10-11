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
	"github.com/mitchellh/cli"
	"google.golang.org/grpc"
	"strings"
)

type LeaveCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *LeaveCommand) Help() string {
	helpText := `
Usage: pulseha leave [options] ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *LeaveCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("leave", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error. Is the PulseHA service running?")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewCLIClient(connection)

	r, err := client.Leave(context.Background(), &proto.PulseLeave{})

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
func (c *LeaveCommand) Synopsis() string {
	return "Tell Pulse to create new HA cluster"
}
