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
	"github.com/olekukonko/tablewriter"
	"google.golang.org/grpc"
	"os"
	"strings"
)

type StatusCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *StatusCommand) Help() string {
	helpText := `
Usage: pulseha status [options] ...
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *StatusCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("status", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())
	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
		return 1
	}
	defer connection.Close()
	client := proto.NewCLIClient(connection)
	c.drawStatusTable(client)

	return 0
}

/**
 *
 */
func (c *StatusCommand) drawStatusTable(client proto.CLIClient) {
	r, err := client.Status(context.Background(), &proto.PulseStatus{})
	if err != nil {
		c.Ui.Output("PulseHA CLI connection error")
		c.Ui.Output(err.Error())
	} else {
		data := [][]string{}
		for _, node := range r.Row {

			data = append(
				data,
				[]string{
					node.Hostname,
					node.Ip,
					node.Latency,
					node.Status.String(),
					node.LastReceived,
				})
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{
			"Node Hostname",
			"Bind Address",
			"Latency",
			"Status",
			"Last Received",
		})
		table.SetCenterSeparator("-")
		table.SetColumnSeparator("|")
		table.SetRowLine(true)
		table.SetAutoMergeCells(false)
		table.AppendBulk(data)
		table.Render()
	}
}

/**
 *
 */
func (c *StatusCommand) Synopsis() string {
	return "Provides a status overview of your configured cluster"
}
