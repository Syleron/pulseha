package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"google.golang.org/grpc"
	"github.com/Syleron/Pulse/proto"
	"context"
	"fmt"
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
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *JoinCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("join", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

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

	connection, err := grpc.Dial(addr[0], grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewRequesterClient(connection)

	r, err := client.Join(context.Background(), &proto.PulseJoin{
		Address: addr[0],
	})

	if err != nil {
		c.Ui.Output("PulseHA CLI connection error")
		c.Ui.Output(err.Error())
	} else {
		fmt.Printf("response: %s", r.Success)
	}

	return 0
}

/**
 *
 */
func (c *JoinCommand) Synopsis() string {
	return "Tell Pulse to join a cluster"
}
