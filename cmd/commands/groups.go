package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"google.golang.org/grpc"
	"github.com/Syleron/Pulse/proto"
	"context"
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

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	cmds := cmdFlags.Args()

	if len(cmds) == 0 {
		c.Ui.Error("Please specify an action.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewRequesterClient(connection)

	switch cmds[0] {
	case "new":
		r, err := client.NewGroup(context.Background(), &proto.PulseGroupNew{})

		if err != nil {
			c.Ui.Output("PulseHA CLI connection error")
			c.Ui.Output(err.Error())
		} else {
			c.Ui.Output(r.Message)
		}
	case "delete":
		// Make sure we have a group name
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
			c.Ui.Output("PulseHA CLI connection error")
			c.Ui.Output(err.Error())
		} else {
			c.Ui.Output(r.Message)
		}
	case "add":
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
			Ips: IPslice,
		})
		if err != nil {
			c.Ui.Output("PulseHA CLI connection error")
			c.Ui.Output(err.Error())
		} else {
			c.Ui.Output(r.Message)
		}
	case "remove":
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
			Ips: IPslice,
		})
		if err != nil {
			c.Ui.Output("PulseHA CLI connection error")
			c.Ui.Output(err.Error())
		} else {
			c.Ui.Output(r.Message)
		}
	default:
		c.Ui.Error("Unknown action provided.")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
		return 1
	}

	return 0
}

/**
 *
 */
func (c *GroupsCommand) Synopsis() string {
	return "Manage floating IP groups"
}
