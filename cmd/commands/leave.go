package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"google.golang.org/grpc"
	"github.com/Syleron/Pulse/proto"
	"context"
)

type LeaveCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *LeaveCommand) Help() string {
	helpText := `
Usage: pulseha leave [options] ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *LeaveCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("leave", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error. Is the PulseHA service running?")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewRequesterClient(connection)

	r, err := client.Leave(context.Background(), &proto.PulseLeave{})

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
func (c *LeaveCommand) Synopsis() string {
	return "Tell Pulse to create new HA cluster"
}
