// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package pulsectl

import (
	"context"
	"flag"
	"github.com/mitchellh/cli"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"strings"
)

type ConfigCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *ConfigCommand) Help() string {
	helpText := `
Usage: pulsectl config <key> <new-value> [options] ...
  Update pulse config key with a new value defined in the 
  pulsectl section in the config
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *ConfigCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("config", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}
	cmds := cmdFlags.Args()

	if len(cmds) == 0 {
		c.Ui.Error("Please specify a config key to update")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	if len(cmds) == 1 {
		c.Ui.Error("Please specify a new config value")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
		return 1
	}

	defer connection.Close()

	client := rpc.NewCLIClient(connection)

	r, err := client.Config(context.Background(), &rpc.ConfigRequest{
		Key: cmds[0],
		Value: cmds[1],
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

// Synopsis	- Description of the command
func (c *ConfigCommand) Synopsis() string {
	return "Manage floating IP groups"
}
