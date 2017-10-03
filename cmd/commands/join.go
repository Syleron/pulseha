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
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"google.golang.org/grpc"
	"github.com/Syleron/PulseHA/proto"
	"context"
	"github.com/Syleron/PulseHA/src/utils"
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
  -bind-addr Pulse daemon bind address and port
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *JoinCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("join", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	bindAddr := cmdFlags.String("bind-addr", "127.0.0.1:9443", "Bind address for local Pulse daemon")

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	addr := cmdFlags.Args()

	if len(addr) == 0 {
		c.Ui.Error("Please specify an address to join.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	// If we have the default.. which we don't want.. error out.
	if *bindAddr == "127.0.0.1:9443" {
		c.Ui.Error("Please specify a bind address.\n")
		c.Ui.Output(c.Help())
		return 1
	}

	bindIP, bindPort, _ :=utils.SplitIpPort(*bindAddr)

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error. Is the PulseHA service running?")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewCLIClient(connection)

	bindAddrString := strings.Split(addr[0], ":")

	if len(bindAddrString) < 2 {
		c.Ui.Error("Please provide an IP:Port")
		c.Ui.Output(c.Help())
		return 1
	}

	r, err := client.Join(context.Background(), &proto.PulseJoin{
		Ip: bindAddrString[0],
		Port: bindAddrString[1],
		BindIp: bindIP,
		BindPort: bindPort,
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
	return "Tell Pulse to join a cluster"
}
