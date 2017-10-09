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
	"strings"
	"flag"
	"github.com/mitchellh/cli"
	"google.golang.org/grpc"
	"github.com/Syleron/PulseHA/proto"
	"github.com/olekukonko/tablewriter"
	"os"
	"context"
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

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())
	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
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
					node.Ping,
					node.Status.String(),
				})
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{
			"Node Hostname",
			"Bind Address",
			"Ping",
			"Status",
		})
		table.SetCenterSeparator("-")
		table.SetColumnSeparator("|")
		table.SetRowLine(true)
		table.SetAutoMergeCells(true)
		for _, v := range data {
			table.Append(v)
		}
		table.Render()
	}
}

/**
 *
 */
func (c *StatusCommand) Synopsis() string {
	return "Provides a status overview of your configured cluster"
}
