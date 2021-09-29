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

type PromoteCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *PromoteCommand) Help() string {
	helpText := `
Usage: pulsectl status [options] ...
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *PromoteCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("status", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}
	addr := cmdFlags.Args()
	if len(addr) == 0 {
		c.Ui.Error("Please specify a node to promote!")
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

	r, err := client.Promote(context.Background(), &rpc.PulsePromote{
		Member: addr[0],
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
func (c *PromoteCommand) Synopsis() string {
	return "Promote a passive member/node in the cluster"
}
