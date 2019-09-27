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
"github.com/Syleron/PulseHA/proto"
"github.com/mitchellh/cli"
"google.golang.org/grpc"
"strings"
)

type NetworkCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *NetworkCommand) Help() string {
	helpText := `
Usage: pulseha network [options] (resync) ...
  Instruct PulseHA to perform particular network related commnads
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *NetworkCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("leave", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	cmds := cmdFlags.Args()

	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error. Is the PulseHA service running?")
		c.Ui.Error(err.Error())
		return 1
	}

	defer connection.Close()

	client := proto.NewCLIClient(connection)

	// If no action is provided then just list our current config
	if len(cmds) == 0 {
		c.drawGroupsTable(client)
		return 0
	}

	switch cmds[0] {
	case "resync":
		return c.New(groupName, client)
	default:
		c.Ui.Error("Unknown action provided.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
}


func resync() {

}

/**
 *
 */
func (c *NetworkCommand) Synopsis() string {
	return "Network related commands"
}
