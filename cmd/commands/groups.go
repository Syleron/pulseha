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
	"github.com/olekukonko/tablewriter"
	"google.golang.org/grpc"
	"os"
	"strings"
)

type GroupsCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *GroupsCommand) Help() string {
	helpText := `
Usage: pulseha group [options] (new/delete/add/remove/assign/unassign) ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
  - name - Name of a group.
  - ips - Selected floating IPs separated by a comma.
  - node - Node hostname.
  - iface - Node network interface.
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *GroupsCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("group", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	groupName := cmdFlags.String("name", "", "Floating IP group name")
	fIPs := cmdFlags.String("ips", "", "Floating IPs")
	nodeHostname := cmdFlags.String("node", "", "Node hostname")
	nodeIface := cmdFlags.String("iface", "", "Node network interface")

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	cmds := cmdFlags.Args()

	connection, err := grpc.Dial("127.0.0.1:49152", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewCLIClient(connection)

	// If no action is provided then just list our current config
	if len(cmds) == 0 {
		c.drawGroupsTable(client)
		return 0
	}

	switch cmds[0] {
	case "new":
		return c.New(client)
	case "delete":
		return c.Delete(groupName, client)
	case "add":
		return c.Add(groupName, fIPs, client)
	case "remove":
		return c.Remove(groupName, fIPs, client)
	case "assign":
		return c.Assign(groupName, nodeHostname, nodeIface, client)
	case "unassign":
		return c.Unassign(groupName, nodeHostname, nodeIface, client)
	default:
		c.Ui.Error("Unknown action provided.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
}

/**
 *
 */
func (c *GroupsCommand) Synopsis() string {
	return "Manage floating IP groups"
}

/**
 *
 */
func (c *GroupsCommand) drawGroupsTable(client proto.CLIClient) {
	r, err := client.GroupList(context.Background(), &proto.GroupTable{})
	if err != nil {
		c.Ui.Output("PulseHA CLI connection error")
		c.Ui.Output(err.Error())
	} else {
		data := [][]string{}
		for _, group := range r.Row {

			data = append(
				data,
				[]string{
					group.Name,
					strings.Join(group.Ip, ", "),
					strings.Join(group.Nodes, "\n"),
					strings.Join(group.Interfaces, "\n"),
				})
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{
			"Group Name",
			"IP Assignments",
			"Nodes",
			"Ifaces",
		})
		table.SetCenterSeparator("-")
		table.SetColumnSeparator("|")
		table.SetRowLine(true)
		table.SetAutoMergeCells(true)
		table.AppendBulk(data)
		table.Render()
	}
}

/**
 *
 */
func (c *GroupsCommand) New(client proto.CLIClient) int {
	r, err := client.NewGroup(context.Background(), &proto.PulseGroupNew{})
	if err != nil {
		c.Ui.Output("PulseHA CLI connection error. Is the PulseHA service running?")
		c.Ui.Output(err.Error())
	} else {
		if r.Success {
			c.Ui.Output("\n[\u2713] " + r.Message + "\n")
		} else {
			c.Ui.Output("\n [x] " + r.Message + "\n")
		}
	}
	return 0
}

/**
 *
 */
func (c *GroupsCommand) Delete(groupName *string, client proto.CLIClient) int {
	if *groupName == "" {
		c.Ui.Error("Please specify a group name")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	r, err := client.DeleteGroup(context.Background(), &proto.PulseGroupDelete{
		Name: *groupName,
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
func (c *GroupsCommand) Add(groupName, fIPs *string, client proto.CLIClient) int {
	if *groupName == "" {
		c.Ui.Error("Please specify a group name")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	if *fIPs == "" {
		c.Ui.Error("Please specify at least one IP address")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	IPslice := strings.Split(*fIPs, ",")
	r, err := client.GroupIPAdd(context.Background(), &proto.PulseGroupAdd{
		Name: *groupName,
		Ips:  IPslice,
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
func (c *GroupsCommand) Remove(groupName, fIPs *string, client proto.CLIClient) int {
	if *groupName == "" {
		c.Ui.Error("Please specify a group name")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	if *fIPs == "" {
		c.Ui.Error("Please specify at least one IP address")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	IPslice := strings.Split(*fIPs, ",")
	r, err := client.GroupIPRemove(context.Background(), &proto.PulseGroupRemove{
		Name: *groupName,
		Ips:  IPslice,
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
func (c *GroupsCommand) Assign(groupName, nodeHostname, nodeIface *string, client proto.CLIClient) int {
	if *groupName == "" {
		c.Ui.Error("Please specify a group name")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	if *nodeHostname == "" {
		c.Ui.Error("Please specify the node hostname")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	if *nodeIface == "" {
		c.Ui.Error("Please specify ame network interface")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	r, err := client.GroupAssign(context.Background(), &proto.PulseGroupAssign{
		Group:     *groupName,
		Interface: *nodeIface,
		Node:      *nodeHostname,
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
func (c *GroupsCommand) Unassign(groupName, nodeHostname, nodeIface *string, client proto.CLIClient) int {
	if *groupName == "" {
		c.Ui.Error("Please specify a group name")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	if *nodeHostname == "" {
		c.Ui.Error("Please specify the node hostname")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	if *nodeIface == "" {
		c.Ui.Error("Please specify ame network interface")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}
	r, err := client.GroupUnassign(context.Background(), &proto.PulseGroupUnassign{
		Group:     *groupName,
		Interface: *nodeIface,
		Node:      *nodeHostname,
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
