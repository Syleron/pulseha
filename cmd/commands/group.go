package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"google.golang.org/grpc"
	"github.com/Syleron/Pulse/proto"
	"context"
)

type GroupCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *GroupCommand) Help() string {
	helpText := `
Usage: pulseha group [options] ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
  - list - Lists available groups
  - New - Create new floating IP group
  - Delete - Delete floating IP group
  - Add - Add one or more floating IPs
  - Remove - Remove one or more floating IPs
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *GroupCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("group", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

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
	case "list":
		//r, err = client.NewGroup(context.Background(), &proto.Pulse{
		//})
	case "new":
		r, err := client.NewGroup(context.Background(), &proto.PulseGroupNew{})

		if err != nil {
			c.Ui.Output("PulseHA CLI connection error")
			c.Ui.Output(err.Error())
		} else {
			c.Ui.Output(r.Message)
		}
	case "delete":
		r, err := client.DeleteGroup(context.Background(), &proto.PulseGroupDelete{})

		if err != nil {
			c.Ui.Output("PulseHA CLI connection error")
			c.Ui.Output(err.Error())
		} else {
			c.Ui.Output(r.Message)
		}
	case "add":
		//r, err = client.NewGroup(context.Background(), &proto.PulseGroupAdd{
		//})
	case "remove":
		//r, err = client.NewGroup(context.Background(), &proto.PulseGroupRemove{
		//})
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
func (c *GroupCommand) Synopsis() string {
	return "Manage floating IP groups"
}
