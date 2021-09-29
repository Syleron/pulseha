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

type TokenCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *TokenCommand) Help() string {
	helpText := `
Usage: pulsectl token
  Generates new new cluster token for PulseHA.
`
	return strings.TrimSpace(helpText)
}

/**
Run the CLI command
*/
func (c *TokenCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("cert", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	// Make sure we have cmd args
	if err := cmdFlags.Parse(args); err != nil {
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

	r, err := client.Token(context.Background(), &rpc.PulseToken{})

	if err != nil {
		c.Ui.Output("PulseHA CLI connection error. Is the PulseHA service running?")
		return 1
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
func (c *TokenCommand) Synopsis() string {
	return "Generate new cluster token for PulseHA"
}
