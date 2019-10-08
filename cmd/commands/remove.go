/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

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
	"github.com/syleron/pulseha/proto"
	"github.com/mitchellh/cli"
	"google.golang.org/grpc"
	"strings"
)

type RemoveCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *RemoveCommand) Help() string {
	helpText := `
Usage: pulseha remove [hostname] ...
  Tells a running PulseHA agent to remove a node from the cluster
  by specifying at least one existing member's hostname'.
Options:
  - hostname - node hostname
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *RemoveCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("leave", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	cmds := cmdFlags.Args()
	if len(cmds) == 0 {
		c.Ui.Error("Please specify a node hostname to remove")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error. Is the PulseHA service running?")
		c.Ui.Error(err.Error())
		return 1
	}

	defer connection.Close()

	client := proto.NewCLIClient(connection)

	r, err := client.Remove(context.Background(), &proto.PulseRemove{
		Hostname: cmds[0],
	})

	if err != nil {
		c.Ui.Output("PulseHA CLI connection error. Is the PulseHA service running?")
		c.Ui.Output(err.Error())
	} else {
		if r.Success {
			c.Ui.Output("\n[\u2713] " + r.Message + "\n")
		} else {
			c.Ui.Output("\n[x] " + r.Message + "\n")
			return 1
		}
	}

	return 0
}

/**
 *
 */
func (c *RemoveCommand) Synopsis() string {
	return "Remove node from PulseHA cluster"
}
