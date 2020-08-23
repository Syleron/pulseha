package pulseha

import (
	"context"
	"flag"
	"github.com/mitchellh/cli"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"strings"
)

type PromoteCommand struct {
	Ui cli.Ui
}

/**
 *
 */
func (c *PromoteCommand) Help() string {
	helpText := `
Usage: pulseha status [options] ...
Options:
`
	return strings.TrimSpace(helpText)
}

/**
 *
 */
func (c *PromoteCommand) Run(args []string) int {
	cmdFlags := flag.NewFlagSet("status", flag.ContinueOnError)
	cmdFlags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}
	addr := cmdFlags.Args()
	if len(addr) == 0 {
		c.Ui.Error("Please specify a node to promote!")
		c.Ui.Error("")
		c.Ui.Error(c.Help())
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

	r, err := client.Promote(context.Background(), &rpc.PulsePromote{
		Member: addr[0],
	})
	if err != nil {
		c.Ui.Output("PulseHA CLI connection error. Is the PulseHA service running?")
		c.Ui.Output(err.Error())
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
func (c *PromoteCommand) Synopsis() string {
	return "Promote a passive member/node in the cluster"
}
