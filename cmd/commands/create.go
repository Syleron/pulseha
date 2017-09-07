package commands

import (
	"github.com/mitchellh/cli"
	"strings"
	"flag"
	"fmt"
	"google.golang.org/grpc"
	"github.com/Syleron/Pulse/proto"
	"context"
)

type CreateCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *CreateCommand) Help() string {
	helpText := `
Usage: PulseHA create [options] address ...
  Tells a running PulseHA agent to join the cluster
  by specifying at least one existing member.
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *CreateCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("create", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }

	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	connection, err := grpc.Dial("127.0.0.1:9443", grpc.WithInsecure())

	if err != nil {
		c.Ui.Error("GRPC client connection error")
		c.Ui.Error(err.Error())
	}

	defer connection.Close()

	client := proto.NewRequesterClient(connection)

	r, err := client.Create(context.Background(), &proto.PulseCreate{})

	if err != nil {
		c.Ui.Output("PulseHA CLI connection error")
	} else {
		fmt.Printf("response: %s", r.Success)
	}

	return 0
}

/**
 *
 */
func (c *CreateCommand) Synopsis() string {
	return "Tell Pulse to create new HA cluster"
}
